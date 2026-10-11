package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"paas/internal/daemon"
	"paas/internal/model"
)

func TestParseArgsSupportsSocketBeforeOrAfterCommand(t *testing.T) {
	t.Setenv("PAASD_SOCKET", "")
	for _, test := range []struct {
		name   string
		args   []string
		socket string
	}{
		{name: "short before", args: []string{"-socket", "/tmp/one.sock", "ping"}, socket: "/tmp/one.sock"},
		{name: "long after", args: []string{"ping", "--socket", "/tmp/two.sock"}, socket: "/tmp/two.sock"},
		{name: "long equals before", args: []string{"--socket=/tmp/three.sock", "ping"}, socket: "/tmp/three.sock"},
		{name: "short equals after", args: []string{"ping", "-socket=/tmp/four.sock"}, socket: "/tmp/four.sock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket, command, err := parseArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if socket != test.socket || command != "ping" {
				t.Fatalf("parseArgs() = (%q, %q), want (%q, ping)", socket, command, test.socket)
			}
		})
	}
}

func TestParseArgsUsesEnvironmentSocket(t *testing.T) {
	const socket = "/tmp/from-env.sock"
	t.Setenv("PAASD_SOCKET", socket)
	gotSocket, command, err := parseArgs([]string{"ping"})
	if err != nil {
		t.Fatal(err)
	}
	if gotSocket != socket || command != "ping" {
		t.Fatalf("parseArgs() = (%q, %q), want (%q, ping)", gotSocket, command, socket)
	}
}

func TestParseArgsRejectsUnknownOptionAndExtraArgument(t *testing.T) {
	t.Setenv("PAASD_SOCKET", "")
	for _, args := range [][]string{{"--unknown", "ping"}, {"ping", "extra"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) unexpectedly succeeded", args)
		}
	}
}

func TestRunPingSupportsSocketOptionsAroundCommand(t *testing.T) {
	socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ping" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("pong\n"))
	}))

	for _, args := range [][]string{
		{"-socket", socket, "ping"},
		{"ping", "--socket", socket},
		{"--socket=" + socket, "ping"},
		{"ping", "-socket=" + socket},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Errorf("run(%q) code = %d, stderr = %q", args, code, stderr.String())
		}
		if stdout.String() != "pong\n" || stderr.Len() != 0 {
			t.Errorf("run(%q) output = (%q, %q), want (pong newline, empty)", args, stdout.String(), stderr.String())
		}
	}
}

func TestRunUsesEnvironmentSocket(t *testing.T) {
	socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong\n"))
	}))
	t.Setenv("PAASD_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"ping"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() code = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "pong\n" {
		t.Fatalf("stdout = %q, want pong newline", stdout.String())
	}
}

func TestRunCommands(t *testing.T) {
	sourceDir := t.TempDir()
	workingDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	inspectDeployment := &model.Deployment{
		ID:        "dep-inspect",
		AppName:   "sample",
		ReleaseID: "rel-inspect",
		Port:      43123,
		State:     model.Running,
		PID:       321,
	}
	tests := []struct {
		name            string
		args            func(socket string) []string
		response        daemon.Response
		wantCommand     string
		wantRequest     daemon.Request
		wantStdout      string
		wantStdoutExact bool
	}{
		{
			name: "deploy with explicit name",
			args: func(socket string) []string {
				return []string{"deploy", sourceDir, "--name", "sample", "--socket=" + socket}
			},
			response: daemon.Response{Deployment: &model.Deployment{
				ID: "dep-deploy", AppName: "sample", ReleaseID: "rel-deploy", Port: 43001, State: model.Running,
			}, URL: "http://127.0.0.1:43001"},
			wantCommand:     "deploy",
			wantRequest:     daemon.Request{Name: "sample", SourceDir: sourceDir},
			wantStdout:      "State: RUNNING\nRelease: rel-deploy\nPort: 43001\nURL: http://127.0.0.1:43001\n",
			wantStdoutExact: true,
		},
		{
			name: "deploy keeps source name selection with no override",
			args: func(socket string) []string {
				return []string{"-socket", socket, "deploy", "."}
			},
			response: daemon.Response{Deployment: &model.Deployment{
				ID: "dep-config", AppName: "from-config", ReleaseID: "rel-config", Port: 43002, State: model.Running,
			}},
			wantCommand: "deploy",
			wantRequest: daemon.Request{SourceDir: workingDir},
			wantStdout:  "URL: http://from-config.localhost:8080",
		},
		{
			name: "apps",
			args: func(socket string) []string { return []string{"apps", "--socket", socket} },
			response: daemon.Response{Apps: []daemon.AppView{{
				Application: model.Application{Name: "sample"}, Deployment: inspectDeployment,
			}}},
			wantCommand:     "apps",
			wantStdout:      "NAME STATE RELEASE PORT\nsample RUNNING rel-inspect 43123\n",
			wantStdoutExact: true,
		},
		{
			name: "inspect",
			args: func(socket string) []string { return []string{"-socket=" + socket, "inspect", "sample"} },
			response: daemon.Response{Apps: []daemon.AppView{{
				Application: model.Application{Name: "sample"}, Deployment: inspectDeployment,
			}}},
			wantCommand: "inspect",
			wantRequest: daemon.Request{Name: "sample"},
			wantStdout: "Application: sample\nRelease: rel-inspect\nDeployment: dep-inspect\n" +
				"State: RUNNING\nPID: 321\nPort: 43123\nLast error: -\n",
			wantStdoutExact: true,
		},
		{
			name:            "logs preserve daemon bytes",
			args:            func(socket string) []string { return []string{"logs", "sample", "--socket=" + socket} },
			response:        daemon.Response{Logs: "first line\nsecond line"},
			wantCommand:     "logs",
			wantRequest:     daemon.Request{Name: "sample"},
			wantStdout:      "first line\nsecond line",
			wantStdoutExact: true,
		},
		{
			name:            "stop",
			args:            func(socket string) []string { return []string{"stop", "sample", "-socket", socket} },
			response:        daemon.Response{},
			wantCommand:     "stop",
			wantRequest:     daemon.Request{Name: "sample"},
			wantStdout:      "Stopped sample\n",
			wantStdoutExact: true,
		},
		{
			name: "releases",
			args: func(socket string) []string { return []string{"releases", "sample", "--socket=" + socket} },
			response: daemon.Response{Releases: []model.ReleaseView{
				{ID: "rel_1", SourceHash: "sha256hash123", CreatedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), IsActive: true},
			}},
			wantCommand:     "releases",
			wantRequest:     daemon.Request{Name: "sample"},
			wantStdout:      "RELEASE SOURCE_HASH CREATED ACTIVE\nrel_1 sha256hash12 2026-10-10T12:00:00Z yes\n",
			wantStdoutExact: true,
		},
		{
			name: "rollback with release flag",
			args: func(socket string) []string {
				return []string{"rollback", "sample", "--release", "rel_old", "--socket=" + socket}
			},
			response: daemon.Response{Deployment: &model.Deployment{
				ID: "dep-rb", AppName: "sample", ReleaseID: "rel_old", Port: 43003, State: model.Running,
			}, URL: "http://127.0.0.1:43003"},
			wantCommand:     "rollback",
			wantRequest:     daemon.Request{Name: "sample", ReleaseID: "rel_old"},
			wantStdout:      "State: RUNNING\nRelease: rel_old\nPort: 43003\nURL: http://127.0.0.1:43003\n",
			wantStdoutExact: true,
		},
		{
			name: "rollback without release flag",
			args: func(socket string) []string {
				return []string{"rollback", "sample", "--socket=" + socket}
			},
			response: daemon.Response{Deployment: &model.Deployment{
				ID: "dep-rb2", AppName: "sample", ReleaseID: "rel_prev", Port: 43004, State: model.Running,
			}, URL: "http://127.0.0.1:43004"},
			wantCommand:     "rollback",
			wantRequest:     daemon.Request{Name: "sample"},
			wantStdout:      "State: RUNNING\nRelease: rel_prev\nPort: 43004\nURL: http://127.0.0.1:43004\n",
			wantStdoutExact: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PAASD_SOCKET", "")
			calls := make(chan capturedRequest, 1)
			socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request daemon.Request
				decodeErr := json.NewDecoder(r.Body).Decode(&request)
				calls <- capturedRequest{command: strings.TrimPrefix(r.URL.Path, "/"), request: request, err: decodeErr}
				if decodeErr != nil {
					http.Error(w, decodeErr.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(test.response)
			}))

			var stdout, stderr bytes.Buffer
			if code := run(test.args(socket), &stdout, &stderr); code != 0 {
				t.Fatalf("run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if test.wantStdoutExact {
				if got := stdout.String(); got != test.wantStdout {
					t.Fatalf("stdout = %q, want %q", got, test.wantStdout)
				}
			} else if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), test.wantStdout)
			}

			call := <-calls
			if call.err != nil {
				t.Fatalf("decode request: %v", call.err)
			}
			if call.command != test.wantCommand || !reflect.DeepEqual(call.request, test.wantRequest) {
				t.Fatalf("daemon call = (%q, %#v), want (%q, %#v)", call.command, call.request, test.wantCommand, test.wantRequest)
			}
		})
	}
}

func TestRunRollbackFailureShowsError(t *testing.T) {
	socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(daemon.Response{
			Error: "no previous release available for rollback",
		})
	}))
	var stdout, stderr bytes.Buffer
	code := run([]string{"rollback", "sample", "--socket", socket}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "paas rollback: no previous release available for rollback") {
		t.Fatalf("stderr = %q, want 'paas rollback:' prefix", stderr.String())
	}
}

func TestRunConfigureReadsSecretsFromStdinAndPrintsMetadata(t *testing.T) {
	secret := "cli-secret-value"
	received := make(chan daemon.Request, 1)
	socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/configure" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		var request daemon.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- request
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(daemon.Response{Settings: &model.Settings{
			Domains: []string{"app.example.com"}, EnvironmentKeys: []string{"TOKEN"},
			MemoryLimit: "128Mi", CPUQuota: 0.5, TasksLimit: 32,
		}})
	}))
	var stdout, stderr bytes.Buffer
	args := []string{"-socket", socket, "configure", "sample", "--env-stdin", "--domain", "app.example.com", "--memory", "128Mi", "--cpu", "0.5", "--tasks", "32"}
	if code := runWithInput(args, strings.NewReader("TOKEN="+secret+"\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("runWithInput() code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	request := <-received
	if request.Name != "sample" || request.Environment["TOKEN"] != secret || request.Domains == nil || len(*request.Domains) != 1 || (*request.Domains)[0] != "app.example.com" {
		t.Fatalf("configure request = %#v", request)
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) || !strings.Contains(stdout.String(), "TOKEN") || !strings.Contains(stdout.String(), "app.example.com") {
		t.Fatalf("configure CLI output leaked or omitted metadata: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunDeployFailureShowsDeploymentAndError(t *testing.T) {
	sourceDir := t.TempDir()
	socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(daemon.Response{
			Error: "build failed: missing compiler",
			Deployment: &model.Deployment{
				ID: "dep-failed", AppName: "broken", ReleaseID: "", State: model.BuildFailed,
			},
		})
	}))
	var stdout, stderr bytes.Buffer
	code := run([]string{"deploy", sourceDir, "--name", "broken", "--socket", socket}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "State: BUILD_FAILED") || !strings.Contains(stdout.String(), "Deployment: dep-failed") {
		t.Fatalf("stdout = %q, want failure state and deployment ID", stdout.String())
	}
	if !strings.Contains(stderr.String(), "build failed: missing compiler") {
		t.Fatalf("stderr = %q, want build error", stderr.String())
	}
}

func TestRunReturnsUsageStatusForInvalidArguments(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAASD_SOCKET", "")
	for _, args := range [][]string{
		{},
		{"not-a-command"},
		{"--unknown", "ping"},
		{"ping", "extra"},
		{"apps", "--unknown"},
		{"deploy"},
		{"deploy", "/path/that/does/not/exist"},
		{"deploy", filePath},
		{"deploy", t.TempDir(), "extra"},
		{"deploy", t.TempDir(), "--name"},
		{"deploy", t.TempDir(), "--name", "first", "--name", "second"},
		{"deploy", t.TempDir(), "--unknown"},
		{"inspect"},
		{"inspect", "sample", "extra"},
		{"logs", "--name", "sample"},
		{"stop"},
		{"stop", "sample", "extra"},
		{"ping", "--socket="},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%q) code = %d, want 2; stderr = %q", args, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%q) stdout = %q, want empty", args, stdout.String())
		}
	}
}

func TestRunReturnsFailureForDaemonErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		body    string
		status  int
	}{
		{name: "operation response", command: "inspect", body: `{"Error":"app not found"}`, status: http.StatusNotFound},
		{name: "ping transport response", command: "ping", body: "unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PAASD_SOCKET", "")
			socket := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			args := []string{test.command, "--socket", socket}
			if test.command == "inspect" {
				args = []string{"inspect", "missing", "--socket", socket}
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 1 {
				t.Fatalf("run() code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("stderr is empty, want daemon error")
			}
		})
	}
}

type capturedRequest struct {
	command string
	request daemon.Request
	err     error
}

func startFakeDaemon(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "harbor-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "paasd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return socket
}

type failAfterWrites struct {
	writes, allow int
	err           error
}

func TestFailedDeploymentReportsOutputError(t *testing.T) {
	for _, command := range []string{"deploy", "rollback"} {
		var stderr bytes.Buffer
		response := &daemon.Response{Deployment: &model.Deployment{ID: "dep-test", State: model.StartFailed}, Error: "startup failed"}
		status := reportDeployment(&failAfterWrites{err: errors.New("output closed")}, &stderr, command, daemon.Request{}, response, nil)
		if status != 1 || !strings.Contains(stderr.String(), "write deployment result: output closed") || !strings.Contains(stderr.String(), "paas "+command+": startup failed") {
			t.Fatalf("failure diagnostics: status=%d stderr=%q", status, stderr.String())
		}
	}
}

func (w *failAfterWrites) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.allow {
		return 0, w.err
	}
	return len(p), nil
}

func TestInspectionDoesNotInventIsolationOrLimits(t *testing.T) {
	for _, kind := range []string{"host", "bubblewrap", "systemd", ""} {
		t.Run(kind, func(t *testing.T) {
			d := &model.Deployment{AppName: "demo", SandboxType: kind, SystemdUnit: "paas-deploy-test.service", MemoryLimit: "64Mi", CPUQuota: 0.005, TasksLimit: 32}
			var output bytes.Buffer
			if err := printInspection(&output, "demo", &daemon.Response{Deployment: d}); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			if kind == "host" && strings.Contains(text, "PID isolation:     yes") {
				t.Fatal("host isolation fabricated")
			}
			if kind == "" && strings.Contains(text, "Sandbox:") {
				t.Fatal("legacy unknown backend fabricated")
			}
			if kind == "systemd" {
				if !strings.Contains(text, "CPU quota:    0.5%") {
					t.Fatalf("fractional CPU lost: %s", text)
				}
			} else if strings.Contains(text, "Memory limit:") || strings.Contains(text, "Systemd:") {
				t.Fatalf("unenforced limits or fictitious unit reported: %s", text)
			}
		})
	}
}

func TestInspectionPropagatesEveryOutputError(t *testing.T) {
	expected := errors.New("broken writer")
	d := &model.Deployment{AppName: "demo", SandboxType: "systemd", SystemdUnit: "paas-deploy-test.service", MemoryLimit: "64Mi", CPUQuota: 0.5, TasksLimit: 32, State: model.OOMKilled}
	for allow := 0; allow < 5; allow++ {
		writer := &failAfterWrites{allow: allow, err: expected}
		if err := printInspection(writer, "demo", &daemon.Response{Deployment: d}); !errors.Is(err, expected) {
			t.Fatalf("after %d writes, error=%v", allow, err)
		}
	}
}

func TestRollbackRejectsEmptyAndRepeatedReleaseFlags(t *testing.T) {
	for _, args := range [][]string{
		{"demo", "--release="}, {"demo", "-release="},
		{"demo", "--release=--bad"},
		{"demo", "--release", "first", "--release=second"},
		{"demo", "-release=first", "--release", "second"},
	} {
		if _, err := parseCommandRequest("rollback", args); err == nil {
			t.Errorf("accepted malformed rollback args %v", args)
		}
	}
}
