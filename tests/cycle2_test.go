//go:build integration

package tests

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
)

func TestCycle2RuntimeIsolationAndLimits(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Cycle 2 integration test")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("/tmp", "harbor-cycle2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(work)
	})
	_ = os.Chmod(work, 0755)

	data := filepath.Join(work, "data")
	socket := filepath.Join(work, "paasd.sock")
	logPath := filepath.Join(work, "daemon.log")

	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}

	daemonCmd := exec.Command(
		filepath.Join(root, "bin", "paasd"),
		"-socket", socket,
		"-data-dir", data,
		"-listen", "127.0.0.1:0",
		"-build-timeout", "2m",
	)
	daemonCmd.Stdout = log
	daemonCmd.Stderr = log
	if err := daemonCmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemonCmd.Wait(); _ = log.Close() }()

	t.Cleanup(func() {
		if daemonCmd != nil && daemonCmd.Process != nil {
			_ = daemonCmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				_ = daemonCmd.Process.Kill()
				<-exited
			}
		}
	})

	// Wait for daemon ping and discover router port
	var routerURL string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, pingErr := daemon.Ping(ctx, socket)
		cancel()
		if pingErr == nil {
			body, _ := os.ReadFile(logPath)
			match := regexp.MustCompile("router=http://([^ ]+) ").FindStringSubmatch(string(body))
			if len(match) > 1 {
				routerURL = "http://" + match[1]
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if routerURL == "" {
		body, _ := os.ReadFile(logPath)
		t.Fatalf("daemon failed to become ready: %s", string(body))
	}

	cli := func(args ...string) string {
		cmd := exec.Command(filepath.Join(root, "bin", "paas"), append([]string{"--socket", socket}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("paas %s failed: %v: %s", strings.Join(args, " "), err, string(out))
		}
		return string(out)
	}

	// 1. Deploy hello-go
	helloSrc := filepath.Join(root, "examples", "hello-go")
	deployOut := cli("deploy", helloSrc, "--name", "hello")
	if !strings.Contains(deployOut, "State: RUNNING") {
		t.Fatalf("unexpected deploy output: %s", deployOut)
	}

	// 2. Query application via router
	req, _ := http.NewRequest("GET", routerURL+"/", nil)
	req.Host = "hello.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request to hello.localhost failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "hello from harbor") {
		t.Fatalf("unexpected response body: %s", string(respBody))
	}

	// 3. Inspect hello - verify Sandbox, Resources, Systemd blocks
	inspectOut := cli("inspect", "hello")
	expectedBlocks := []string{
		"State: RUNNING",
		"Sandbox:",
		"PID isolation:     yes",
		"Mount isolation:   yes",
		"IPC isolation:     yes",
		"UTS isolation:     yes",
		"User isolation:    yes",
		"Network isolation: no",
		"Resources:",
		"Memory limit: 128Mi",
		"CPU quota:    50%",
		"Tasks limit:  64",
		"Systemd:",
		"Unit: paas-deploy-dep_",
	}
	for _, expected := range expectedBlocks {
		if !strings.Contains(inspectOut, expected) {
			t.Errorf("inspect output missing %q; got:\n%s", expected, inspectOut)
		}
	}

	// 4. Logs hello - verify journald captures output
	logsOut := cli("logs", "hello")
	if !strings.Contains(logsOut, "hello-go listening on 127.0.0.1:") {
		t.Errorf("logs output missing listening message; got:\n%s", logsOut)
	}

	// 5. Stop hello
	stopOut := cli("stop", "hello")
	if !strings.Contains(stopOut, "Stopped hello") {
		t.Errorf("unexpected stop output: %s", stopOut)
	}

	// 6. Verify route withdrawn
	req404, _ := http.NewRequest("GET", routerURL+"/", nil)
	req404.Host = "hello.localhost"
	resp404, err := http.DefaultClient.Do(req404)
	if err != nil {
		t.Fatalf("HTTP request to withdrawn route failed: %v", err)
	}
	resp404.Body.Close()
	if resp404.StatusCode != 404 {
		t.Errorf("expected 404 after stop, got %d", resp404.StatusCode)
	}
}

func TestCycle2MemoryHogOOMKill(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Cycle 2 integration test")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("/tmp", "harbor-cycle2-oom-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(work)
	})
	_ = os.Chmod(work, 0755)

	data := filepath.Join(work, "data")
	socket := filepath.Join(work, "paasd.sock")
	logPath := filepath.Join(work, "daemon.log")

	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}

	daemonCmd := exec.Command(
		filepath.Join(root, "bin", "paasd"),
		"-socket", socket,
		"-data-dir", data,
		"-listen", "127.0.0.1:0",
		"-build-timeout", "2m",
	)
	daemonCmd.Stdout = log
	daemonCmd.Stderr = log
	if err := daemonCmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemonCmd.Wait(); _ = log.Close() }()

	t.Cleanup(func() {
		if daemonCmd != nil && daemonCmd.Process != nil {
			_ = daemonCmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				_ = daemonCmd.Process.Kill()
				<-exited
			}
		}
	})

	// Wait for daemon ping and discover router port
	var routerURL string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, pingErr := daemon.Ping(ctx, socket)
		cancel()
		if pingErr == nil {
			body, _ := os.ReadFile(logPath)
			match := regexp.MustCompile("router=http://([^ ]+) ").FindStringSubmatch(string(body))
			if len(match) > 1 {
				routerURL = "http://" + match[1]
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if routerURL == "" {
		body, _ := os.ReadFile(logPath)
		t.Fatalf("daemon failed to become ready: %s", string(body))
	}

	cli := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(root, "bin", "paas"), append([]string{"--socket", socket}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Deploy memory-hog (which requests 64Mi in paas.yaml and tries to allocate 256 MiB)
	memHogSrc := filepath.Join(root, "examples", "memory-hog")
	deployOut, err := cli("deploy", memHogSrc, "--name", "memhog")
	t.Logf("deploy memhog result: %s (err: %v)", deployOut, err)

	// Poll inspect memhog until state becomes OOM_KILLED
	var inspectOut string
	oomDetected := false
	pollDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(pollDeadline) {
		time.Sleep(200 * time.Millisecond)
		out, inspectErr := cli("inspect", "memhog")
		if inspectErr == nil {
			inspectOut = out
			if strings.Contains(out, "State: OOM_KILLED") {
				oomDetected = true
				break
			}
		}
	}

	if !oomDetected {
		t.Fatalf("timed out waiting for memhog to become OOM_KILLED; inspect output:\n%s", inspectOut)
	}

	// Verify inspect output contains failure block details
	expectedFailureDetails := []string{
		"State: OOM_KILLED",
		"Failure:",
		"Result: oom-kill",
		"Signal: SIGKILL",
		"Cause:  Exceeded memory limit (128Mi)",
		"Memory limit: 128Mi",
	}
	for _, expected := range expectedFailureDetails {
		if !strings.Contains(inspectOut, expected) {
			t.Errorf("inspect output missing %q; got:\n%s", expected, inspectOut)
		}
	}

	// Verify route is withdrawn
	req, _ := http.NewRequest("GET", routerURL+"/", nil)
	req.Host = "memhog.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request to memhog route failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for OOM-killed app, got %d", resp.StatusCode)
	}

	_, _ = cli("stop", "memhog")
}
