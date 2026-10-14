//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
	"paas/internal/model"
)

func TestDeploymentLifecycle(t *testing.T) {
	for _, name := range []string{"PAASD_PUBLIC_ORIGIN", "PAASD_ADMIN_PASSWORD_HASH", "PAASD_SESSION_SECRET"} {
		t.Setenv(name, "")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp("/tmp", "harbor-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(work, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
		if err := os.RemoveAll(work); err != nil {
			t.Errorf("clean acceptance workspace: %v", err)
		}
	})
	if err := os.Chmod(work, 0755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(work, "data")
	socket := filepath.Join(work, "paasd.sock")
	logPath := filepath.Join(work, "daemon.log")
	var command *exec.Cmd
	var exited chan error
	buildTimeout := "5m"
	start := func() {
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		command = exec.Command(filepath.Join(root, "bin", "paasd"), "-socket", socket, "-data-dir", data, "-listen", "127.0.0.1:0", "-web-listen", "127.0.0.1:0", "-build-timeout", buildTimeout, "-sandbox", "host")
		command.Stdout = log
		command.Stderr = log
		if err := command.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		exited = make(chan error, 1)
		go func() { exited <- command.Wait(); _ = log.Close() }()
	}
	stop := func() {
		if command == nil {
			return
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(15 * time.Second):
			_ = command.Process.Kill()
			<-exited
			t.Error("daemon failed to stop")
		}
		command = nil
	}
	t.Cleanup(stop)
	ready := func() string {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err := daemon.Ping(ctx, socket)
			cancel()
			if err == nil {
				body, _ := os.ReadFile(logPath)
				match := regexp.MustCompile("router=http://([^ ]+) ").FindStringSubmatch(string(body))
				if len(match) > 1 {
					return "http://" + match[1]
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		body, _ := os.ReadFile(logPath)
		t.Fatalf("daemon readiness: %s", body)
		return ""
	}
	call := func(op, name, source string, wantSuccess bool) *daemon.Response {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		out, err := daemon.Execute(ctx, socket, op, daemon.Request{Name: name, SourceDir: source})
		if wantSuccess && err != nil {
			t.Fatalf("%s %s: %v response=%+v", op, name, err, out)
		}
		if !wantSuccess && err == nil {
			t.Fatalf("%s %s unexpectedly succeeded", op, name)
		}
		return out
	}
	dashboardCall := func(operation, input string) map[string]any {
		body, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		match := regexp.MustCompile(`dashboard ready: (http://[^\s]+)`).FindSubmatch(body)
		if len(match) != 2 {
			t.Fatalf("dashboard readiness missing: %s", body)
		}
		origin := string(match[1])
		req, err := http.NewRequest(http.MethodPost, origin+"/api/dashboard/"+operation, strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("dashboard %s: status=%d body=%s err=%v", operation, response.StatusCode, result, err)
		}
		for _, private := range []string{data, "dashboard-secret-value", `"PID"`, `"LogPath"`, `"artifact_dir"`} {
			if strings.Contains(string(result), private) {
				t.Fatalf("dashboard %s exposed private data %q: %s", operation, private, result)
			}
		}
		var output map[string]any
		if err := json.Unmarshal(result, &output); err != nil {
			t.Fatal(err)
		}
		return output
	}
	cli := func(args ...string) string {
		command := exec.Command(filepath.Join(root, "bin", "paas"), append([]string{"--socket", socket}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %v: %v: %s", args, err, output)
		}
		return string(output)
	}
	route := func(address, host string, want int) string {
		req, err := http.NewRequest("GET", address+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("host %s status=%d body=%s want=%d", host, response.StatusCode, body, want)
		}
		return string(body)
	}
	source := func(fixture, name string) string {
		target := filepath.Join(work, name)
		if err := os.Mkdir(target, 0755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(root, "examples", fixture))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				body, err := os.ReadFile(filepath.Join(root, "examples", fixture, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0644)
				if strings.HasSuffix(entry.Name(), ".sh") {
					mode = 0755
				}
				if err := os.WriteFile(filepath.Join(target, entry.Name()), body, mode); err != nil {
					t.Fatal(err)
				}
			}
		}
		return target
	}
	start()
	address := ready()
	helloSource := source("hello-go", "hello-source")
	hello := call("deploy", "hello", helloSource, true).Deployment
	if hello.State != model.Running || hello.ReleaseID == "" || hello.Port == 0 {
		t.Fatalf("incomplete deployment: %+v", hello)
	}
	if body := route(address, "HELLO.localhost:8080", 200); !strings.Contains(body, "hello from harbor") {
		t.Fatal(body)
	}
	if route(address, "missing.localhost", 404) == "" {
		t.Fatal("empty unknown-host response")
	}
	if err := os.RemoveAll(helloSource); err != nil {
		t.Fatal(err)
	}
	route(address, "hello.localhost", 200)
	inspect := call("inspect", "hello", "", true).Deployment
	if inspect.ReleaseID != hello.ReleaseID {
		t.Fatal("inspect release differs")
	}
	logs := call("logs", "hello", "", true).Logs
	if !strings.Contains(logs, "hello-go listening") {
		t.Fatalf("runtime logs missing: %s", logs)
	}
	if output := cli("inspect", "hello"); !strings.Contains(output, hello.ReleaseID) || !strings.Contains(output, "RUNNING") {
		t.Fatalf("real CLI inspection incomplete: %s", output)
	}
	// The embedded dashboard and CLI operate on the same live deployment, while
	// the browser receives only public metadata and write-only environment keys.
	dashboardCall("configureApp", `{"name":"hello","environment":{"DASHBOARD_SECRET":"dashboard-secret-value"}}`)
	webApp := dashboardCall("getApp", `{"name":"hello"}`)
	webDeployment, ok := webApp["deployment"].(map[string]any)
	if !ok || webDeployment["ReleaseID"] != hello.ReleaseID || webDeployment["State"] != "RUNNING" {
		t.Fatalf("dashboard deployment differs from CLI: %#v", webApp)
	}
	webSettings := dashboardCall("getAppSettings", `{"name":"hello"}`)
	if keys, ok := webSettings["environmentKeys"].([]any); !ok || len(keys) != 1 || keys[0] != "DASHBOARD_SECRET" {
		t.Fatalf("dashboard environment metadata: %#v", webSettings)
	}
	if output := cli("apps"); !strings.Contains(output, "hello") || !strings.Contains(output, "RUNNING") {
		t.Fatalf("real CLI apps incomplete: %s", output)
	}
	if output := cli("logs", "hello"); !strings.Contains(output, "hello-go listening") {
		t.Fatalf("real CLI logs missing: %s", output)
	}
	releasePath := filepath.Join(data, "releases", hello.ReleaseID)
	manifest, err := os.ReadFile(filepath.Join(releasePath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(manifest, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["id"] != hello.ReleaseID || metadata["runtime"] != "go" || len(fmt.Sprint(metadata["source_hash"])) != 64 {
		t.Fatalf("incomplete release manifest: %s", manifest)
	}
	if info, err := os.Stat(filepath.Join(releasePath, "app", "server")); err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("release writable or missing: %v %v", info, err)
	}
	if os.Geteuid() == 0 {
		out, err := exec.Command("ps", "-o", "uid=", "-p", strconv.Itoa(hello.PID)).Output()
		if err != nil || strings.TrimSpace(string(out)) == "0" {
			t.Fatalf("runtime is root or missing: %s %v", out, err)
		}
		uid, err := exec.Command("id", "-u", "paas-runtime").Output()
		if err != nil || strings.TrimSpace(string(out)) != strings.TrimSpace(string(uid)) {
			t.Fatalf("runtime identity differs: got %s want %s: %v", out, uid, err)
		}
		probe := exec.Command("runuser", "-u", "paas-runtime", "--", "touch", filepath.Join(releasePath, "app", "cannot-write"))
		if probe.Run() == nil {
			t.Fatal("runtime user modified immutable release")
		}
		t.Logf("runtime uid=%s; release write rejected", strings.TrimSpace(string(out)))
		invalid := exec.Command(filepath.Join(root, "bin", "paasd"), "-build-user", "paas-build", "-runtime-user", "paas-build")
		if output, err := invalid.CombinedOutput(); err == nil || !strings.Contains(string(output), "distinct non-root UIDs") {
			t.Fatalf("shared build/runtime identity was not rejected: %s %v", output, err)
		}
	}
	// A second daemon must not steal the active Unix socket.
	second := exec.Command(filepath.Join(root, "bin", "paasd"), "-socket", socket, "-data-dir", filepath.Join(work, "second"), "-listen", "127.0.0.1:0", "-web-listen", "127.0.0.1:0")
	if out, err := second.CombinedOutput(); err == nil || !strings.Contains(string(out), "already listening") {
		t.Fatalf("second daemon socket protection: %s %v", out, err)
	}
	if _, err := daemon.Ping(context.Background(), socket); err != nil {
		t.Fatal(err)
	}
	second = exec.Command(filepath.Join(root, "bin", "paasd"), "-socket", filepath.Join(work, "other.sock"), "-data-dir", data, "-listen", "127.0.0.1:0", "-web-listen", "127.0.0.1:0")
	if out, err := second.CombinedOutput(); err == nil || !strings.Contains(string(out), "another daemon is using") {
		t.Fatalf("second daemon data-directory protection: %s %v", out, err)
	}
	if call("inspect", "hello", "", true).Deployment.State != model.Running {
		t.Fatal("duplicate daemon disturbed live deployment")
	}
	broken := call("deploy", "broken", source("broken-build", "broken-source"), false)
	if broken.Deployment == nil || broken.Deployment.State != model.BuildFailed {
		t.Fatalf("build failure response=%+v", broken)
	}
	route(address, "broken.localhost", 404)
	unhealthy := call("deploy", "unhealthy", source("unhealthy", "unhealthy-source"), false)
	if unhealthy.Deployment == nil || unhealthy.Deployment.State != model.Unhealthy {
		t.Fatalf("health failure response=%+v", unhealthy)
	}
	route(address, "unhealthy.localhost", 404)
	// Two simultaneously healthy apps get distinct ports; omitting the health
	// path exercises TCP readiness instead of HTTP readiness.
	tcpSource := source("hello-go", "tcp-source")
	cfgBody, err := os.ReadFile(filepath.Join(tcpSource, "paas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfgBody = []byte(strings.Replace(string(cfgBody), "  path: /health", "  path: ''", 1))
	if err := os.WriteFile(filepath.Join(tcpSource, "paas.yaml"), cfgBody, 0644); err != nil {
		t.Fatal(err)
	}
	if output := cli("deploy", tcpSource, "--name", "tcp"); !strings.Contains(output, "tcp.localhost:") {
		t.Fatalf("real CLI deploy URL missing: %s", output)
	}
	tcp := call("inspect", "tcp", "", true).Deployment
	if tcp.Port == hello.Port || tcp.State != model.Running {
		t.Fatalf("distinct port/TCP readiness: %+v", tcp)
	}
	route(address, "tcp.localhost", 200)
	route(address, "hello.localhost", 200)
	cli("stop", "tcp")
	route(address, "tcp.localhost", 404)
	if os.Geteuid() == 0 { // Root daemon enforces the dedicated build account.
		evidence := source("hello-go", "identity-source")
		cfg := fmt.Sprintf("name: identity\nruntime: go\nbuild:\n  command: 'id -u; go build -o \"$PAAS_OUT/server\" main.go'\nstart:\n  command: /app/server\nhealth:\n  path: /health\n")
		if err := os.WriteFile(filepath.Join(evidence, "paas.yaml"), []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
		call("deploy", "identity", evidence, true)
		buildLog := call("logs", "identity", "", true).Logs
		uidOut, err := exec.Command("id", "-u", "paas-build").Output()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buildLog, strings.TrimSpace(string(uidOut))+"\n") {
			t.Fatalf("build UID evidence missing: %s", buildLog)
		}
		call("stop", "identity", "", true)
	}
	dashboardCall("stopApp", `{"name":"hello"}`)
	route(address, "hello.localhost", 404)
	if got := call("inspect", "hello", "", true).Deployment.State; got != model.Stopped {
		t.Fatalf("stop state=%s", got)
	}
	// A desired-running app recovers its immutable artifact after a graceful
	// daemon restart, even when its original source tree has been removed.
	recoverySource := source("hello-go", "recovery-source")
	recovery := call("deploy", "recovery", recoverySource, true).Deployment
	if err := os.RemoveAll(recoverySource); err != nil {
		t.Fatal(err)
	}
	// Persistence and explicit stop intent are observable after a clean daemon
	// restart: recovery starts again, while hello remains stopped.
	stop()
	start()
	address = ready()
	persisted := call("inspect", "hello", "", true).Deployment
	if persisted.ID != hello.ID || persisted.ReleaseID != hello.ReleaseID || persisted.State != model.Stopped {
		t.Fatalf("state lost on restart: %+v", persisted)
	}
	route(address, "hello.localhost", 404)
	recovered := call("inspect", "recovery", "", true).Deployment
	if recovered.State != model.Running || recovered.ReleaseID != recovery.ReleaseID || recovered.ID == recovery.ID || recovered.PID == recovery.PID {
		t.Fatalf("immutable release was not recovered into a new runtime: before=%+v after=%+v", recovery, recovered)
	}
	if body := route(address, "recovery.localhost", 200); !strings.Contains(body, "hello from harbor") {
		t.Fatalf("recovered route did not serve immutable artifact: %s", body)
	}
	// Real process death must withdraw its route and become CRASHED.
	crash := call("deploy", "crash", source("hello-go", "crash-source"), true).Deployment
	if err := syscall.Kill(crash.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if call("inspect", "crash", "", true).Deployment.State == model.Crashed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if call("inspect", "crash", "", true).Deployment.State != model.Crashed {
		t.Fatal("crash not recorded")
	}
	route(address, "crash.localhost", 404)
	// A hard daemon death kills the direct Linux runtime. Desired-running apps
	// are restarted from their immutable release, while explicit stops remain
	// stopped across subsequent restarts.
	if runtime.GOOS == "linux" {
		orphanSource := source("hello-go", "orphan-source")
		orphan := call("deploy", "orphan", orphanSource, true).Deployment
		if err := os.RemoveAll(orphanSource); err != nil {
			t.Fatal(err)
		}
		_ = command.Process.Kill()
		<-exited
		command = nil
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && processAlive(orphan.PID) {
			time.Sleep(50 * time.Millisecond)
		}
		if processAlive(orphan.PID) {
			t.Fatal("direct runtime survived hard daemon death")
		}
		start()
		address = ready()
		recoveredOrphan := call("inspect", "orphan", "", true).Deployment
		if recoveredOrphan.State != model.Running || recoveredOrphan.ReleaseID != orphan.ReleaseID || recoveredOrphan.ID == orphan.ID {
			t.Fatalf("restart failed to recover orphaned app from immutable release: %+v", recoveredOrphan)
		}
		route(address, "orphan.localhost", 200)
		if call("inspect", "hello", "", true).Deployment.State != model.Stopped {
			t.Fatal("explicitly stopped app was restarted after daemon crash")
		}
	}
	// Use a short timeout with a real child process to prove build cancellation
	// cleans up the process group/systemd unit, not just the HTTP request.
	stop()
	buildTimeout = "1s"
	start()
	address = ready()
	timeoutSource := source("hello-go", "timeout-source")
	timeoutConfig := "name: timeout\nruntime: go\nbuild:\n  command: 'sleep 30 & echo timeout-child=$!; wait'\nstart:\n  command: /app/server\n"
	if err := os.WriteFile(filepath.Join(timeoutSource, "paas.yaml"), []byte(timeoutConfig), 0644); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	timedOut := call("deploy", "timeout", timeoutSource, false).Deployment
	if timedOut == nil || timedOut.State != model.BuildFailed || time.Since(began) > 10*time.Second {
		t.Fatalf("bounded timeout failed: %+v", timedOut)
	}
	buildLog := call("logs", "timeout", "", true).Logs
	match := regexp.MustCompile(`timeout-child=(\d+)`).FindStringSubmatch(buildLog)
	if len(match) != 2 {
		t.Fatalf("timeout child evidence missing: %s", buildLog)
	}
	childPID, _ := strconv.Atoi(match[1])
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && processAlive(childPID) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(childPID) {
		t.Fatal("timed-out build left a live descendant")
	}
	route(address, "timeout.localhost", 404)
	apps := call("apps", "", "", true).Apps
	if len(apps) < 4 {
		t.Fatalf("apps missing deployments: %+v", apps)
	}
	t.Log("deploy, source independence, logs, immutability, failures, HTTP/TCP routing, stop, persistence, crashes, build timeout: passed")
}

func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
}
