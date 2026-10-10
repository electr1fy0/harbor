//go:build integration

package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
	"paas/internal/model"
)

type cycle3Harness struct {
	t                           *testing.T
	root, work, socket, address string
}

func newCycle3Harness(t *testing.T) *cycle3Harness {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp("/tmp", "harbor-cycle3-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := filepath.WalkDir(work, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return nil
		}); err != nil {
			t.Errorf("make workspace removable: %v", err)
		}
		if err := os.RemoveAll(work); err != nil {
			t.Errorf("remove Cycle 3 workspace: %v", err)
		}
	})
	if err := os.Chmod(work, 0755); err != nil {
		t.Fatal(err)
	}
	h := &cycle3Harness{t: t, root: root, work: work, socket: filepath.Join(work, "daemon.sock")}
	logPath := filepath.Join(work, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-socket", h.socket, "-data-dir", filepath.Join(work, "data"), "-listen", "127.0.0.1:0", "-build-timeout", "5m"}
	// Exercise the production Linux backend where available, and the real
	// host runtime elsewhere. Candidate lifecycle is platform independent.
	if runtime.GOOS != "linux" {
		args = append(args, "-sandbox", "host")
	}
	cmd := exec.Command(filepath.Join(root, "bin", "paasd"), args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { err := cmd.Wait(); exited <- err; _ = logFile.Close() }()
	t.Cleanup(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
			t.Errorf("signal daemon: %v", err)
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Error("daemon failed to stop")
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	pattern := regexp.MustCompile(`router=http://([^ ]+) `)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, pingErr := daemon.Ping(ctx, h.socket)
		cancel()
		if pingErr == nil {
			body, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if match := pattern.FindStringSubmatch(string(body)); len(match) == 2 {
				h.address = "http://" + match[1]
				return h
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, err := os.ReadFile(logPath)
	t.Fatalf("daemon readiness: %s (%v)", body, err)
	return nil
}

func (h *cycle3Harness) cli(success bool, args ...string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(h.root, "bin", "paas"), append([]string{"--socket", h.socket}, args...)...)
	out, err := cmd.CombinedOutput()
	if (err == nil) != success {
		h.t.Fatalf("CLI %v success=%v: %v: %s", args, success, err, out)
	}
	return string(out)
}

func (h *cycle3Harness) inspect(name string) *model.Deployment {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := daemon.Execute(ctx, h.socket, "inspect", daemon.Request{Name: name})
	if err != nil || response.Deployment == nil {
		h.t.Fatalf("inspect %s: %+v %v", name, response, err)
	}
	return response.Deployment
}

func (h *cycle3Harness) source(fixture, name string) string {
	h.t.Helper()
	target := filepath.Join(h.work, name)
	if err := os.Mkdir(target, 0755); err != nil {
		h.t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(h.root, "examples", fixture))
	if err != nil {
		h.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			h.t.Fatalf("unexpected fixture directory: %s", entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(h.root, "examples", fixture, entry.Name()))
		if err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, entry.Name()), body, 0644); err != nil {
			h.t.Fatal(err)
		}
	}
	return target
}

func cycle3Request(ctx context.Context, address, host string) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/", nil)
	if err != nil {
		return 0, "", err
	}
	request.Host = host
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	return response.StatusCode, string(body), err
}

func awaitCycle3(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", message)
}

func TestCycle3ZeroDowntimeRedeploymentAndRollback(t *testing.T) {
	h := newCycle3Harness(t)
	v1 := h.source("hello-go", "v1")
	h.cli(true, "deploy", v1, "--name", "hello")
	first := h.inspect("hello")
	v2 := h.source("hello-go", "v2")
	path := filepath.Join(v2, "main.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(body), "hello from harbor", "hello v2 from harbor")), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	probeErrors := make(chan error, 1)
	var seenV1, seenV2 atomic.Int64
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			code, body, err := cycle3Request(ctx, h.address, "hello.localhost")
			if ctx.Err() != nil {
				return
			}
			if err != nil || code != 200 || (!strings.Contains(body, "hello from harbor") && !strings.Contains(body, "hello v2 from harbor")) {
				select {
				case probeErrors <- fmt.Errorf("status=%d body=%q error=%v", code, body, err):
				default:
				}
				return
			}
			if strings.Contains(body, "hello v2 from harbor") {
				seenV2.Add(1)
			} else {
				seenV1.Add(1)
			}
		}
	}()
	var stopOnce sync.Once
	stopProbe := func() { stopOnce.Do(func() { cancel(); <-done }) }
	t.Cleanup(stopProbe)
	awaitCycle3(t, "v1 traffic before replacement", func() bool { return seenV1.Load() > 0 })
	h.cli(true, "deploy", v2, "--name", "hello")
	second := h.inspect("hello")
	awaitCycle3(t, "v2 traffic after replacement", func() bool { return seenV2.Load() > 0 })
	if first.ID == second.ID || first.ReleaseID == second.ReleaseID {
		t.Fatalf("replacement reused identity: %+v %+v", first, second)
	}
	rows := strings.Split(strings.TrimSpace(h.cli(true, "releases", "hello")), "\n")
	if len(rows) != 3 {
		t.Fatalf("release history: %v", rows)
	}
	activeCount := 0
	for _, row := range rows[1:] {
		fields := strings.Fields(row)
		if len(fields) != 4 {
			t.Fatalf("malformed release row: %q", row)
		}
		if fields[3] == "yes" {
			activeCount++
			if fields[0] != second.ReleaseID {
				t.Fatalf("wrong active release: %q", row)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("active release count=%d", activeCount)
	}
	if err := os.RemoveAll(v1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(v2); err != nil {
		t.Fatal(err)
	}
	beforeRollback := seenV1.Load()
	h.cli(true, "rollback", "hello", "--release", first.ReleaseID)
	rolled := h.inspect("hello")
	if rolled.ReleaseID != first.ReleaseID || rolled.ID == first.ID || rolled.HealthPath != "/health" {
		t.Fatalf("rollback result: %+v", rolled)
	}
	awaitCycle3(t, "v1 traffic after rollback", func() bool { return seenV1.Load() > beforeRollback })
	stopProbe()
	select {
	case err := <-probeErrors:
		t.Fatalf("traffic interrupted during redeploy/rollback: %v", err)
	default:
	}
	h.cli(true, "stop", "hello")
	code, _, err := cycle3Request(context.Background(), h.address, "hello.localhost")
	if err != nil || code != http.StatusNotFound {
		t.Fatalf("stop retained route: status=%d err=%v", code, err)
	}
}

func TestCycle3CandidateFailureSafety(t *testing.T) {
	h := newCycle3Harness(t)
	h.cli(true, "deploy", filepath.Join(h.root, "examples", "hello-go"), "--name", "hello")
	first := h.inspect("hello")
	for _, fixture := range []string{"broken-build", "unhealthy"} {
		out := h.cli(false, "deploy", filepath.Join(h.root, "examples", fixture), "--name", "hello")
		if !strings.Contains(out, "State: BUILD_FAILED") && !strings.Contains(out, "State: UNHEALTHY") {
			t.Fatalf("failure result: %s", out)
		}
		active := h.inspect("hello")
		if active.ID != first.ID || active.State != model.Running {
			t.Fatalf("candidate disturbed active app: %+v", active)
		}
		code, body, err := cycle3Request(context.Background(), h.address, "hello.localhost")
		if err != nil || code != http.StatusOK || !strings.Contains(body, "hello from harbor") {
			t.Fatalf("candidate disturbed traffic: %d %q %v", code, body, err)
		}
	}
	out := h.cli(false, "rollback", "hello")
	if !strings.Contains(out, "no previous release") {
		t.Fatalf("failed candidate became rollback target: %s", out)
	}
	if active := h.inspect("hello"); active.ID != first.ID {
		t.Fatalf("failed rollback changed route: %+v", active)
	}
}
