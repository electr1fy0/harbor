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
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
)

func TestCycle4NodeDeploymentAndConcurrency(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Cycle 4 integration test")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("/tmp", "harbor-cycle4-node-")
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
		"-build-timeout", "5m",
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

	var routerURL string
	deadline := time.Now().Add(15 * time.Second)
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

	query := func(host, path string) (int, string, error) {
		req, err := http.NewRequest("GET", routerURL+path, nil)
		if err != nil {
			return 0, "", err
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), nil
	}

	// 1. Deploy Node application
	nodeSrc := filepath.Join(root, "examples", "hello-node")
	deployOut, err := cli("deploy", nodeSrc, "--name", "nodeapp")
	if err != nil {
		t.Fatalf("deploy hello-node failed: %v: %s", err, deployOut)
	}
	if !strings.Contains(deployOut, "State: RUNNING") {
		t.Fatalf("unexpected deploy output: %s", deployOut)
	}

	// 2. Query nodeapp
	status, body, err := query("nodeapp.localhost", "/")
	if err != nil {
		t.Fatalf("query nodeapp failed: %v", err)
	}
	if status != 200 || !strings.Contains(body, "hello from harbor node") {
		t.Fatalf("nodeapp query returned status %d, body %q", status, body)
	}

	// 3. Query nodeapp health
	status, body, err = query("nodeapp.localhost", "/health")
	if err != nil {
		t.Fatalf("query nodeapp health failed: %v", err)
	}
	if status != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("nodeapp health returned status %d, body %q", status, body)
	}

	// 4. Deploy Go application concurrently
	goSrc := filepath.Join(root, "examples", "hello-go")
	deployGoOut, err := cli("deploy", goSrc, "--name", "goapp")
	if err != nil {
		t.Fatalf("deploy hello-go failed: %v: %s", err, deployGoOut)
	}
	if !strings.Contains(deployGoOut, "State: RUNNING") {
		t.Fatalf("unexpected go deploy output: %s", deployGoOut)
	}

	// 5. Query both applications concurrently
	var wg sync.WaitGroup
	var nodeErr, goErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			s, b, err := query("nodeapp.localhost", "/")
			if err != nil || s != 200 || !strings.Contains(b, "hello from harbor node") {
				nodeErr = fmt.Errorf("nodeapp error: err=%v status=%d body=%s", err, s, b)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			s, b, err := query("goapp.localhost", "/")
			if err != nil || s != 200 || !strings.Contains(b, "hello from harbor") {
				goErr = fmt.Errorf("goapp error: err=%v status=%d body=%s", err, s, b)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	wg.Wait()
	if nodeErr != nil {
		t.Fatal(nodeErr)
	}
	if goErr != nil {
		t.Fatal(goErr)
	}

	// 6. Inspect nodeapp
	inspectOut, err := cli("inspect", "nodeapp")
	if err != nil {
		t.Fatalf("inspect nodeapp failed: %v: %s", err, inspectOut)
	}
	if !strings.Contains(inspectOut, "PID isolation:     yes") {
		t.Errorf("inspect missing PID isolation: %s", inspectOut)
	}

	// 7. Stop nodeapp and goapp
	if out, err := cli("stop", "nodeapp"); err != nil {
		t.Fatalf("stop nodeapp failed: %v: %s", err, out)
	}
	if out, err := cli("stop", "goapp"); err != nil {
		t.Fatalf("stop goapp failed: %v: %s", err, out)
	}

	// Verify route withdrawn
	s, _, _ := query("nodeapp.localhost", "/")
	if s != 404 {
		t.Errorf("expected status 404 for nodeapp after stop, got %d", s)
	}
}

func TestCycle4PostgresManagedServiceAndPersistence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Cycle 4 integration test")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("/tmp", "harbor-cycle4-pg-")
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

	var daemonCmd *exec.Cmd
	var daemonExited chan error

	stopDaemon := func() {
		if daemonCmd != nil && daemonCmd.Process != nil {
			_ = daemonCmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-daemonExited:
			case <-time.After(5 * time.Second):
				_ = daemonCmd.Process.Kill()
				<-daemonExited
			}
			daemonCmd = nil
		}
	}
	t.Cleanup(stopDaemon)

	startDaemon := func() string {
		stopDaemon()
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(
			filepath.Join(root, "bin", "paasd"),
			"-socket", socket,
			"-data-dir", data,
			"-listen", "127.0.0.1:0",
			"-build-timeout", "5m",
		)
		cmd.Stdout = log
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait(); _ = log.Close() }()
		daemonCmd = cmd
		daemonExited = exited

		var routerURL string
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, pingErr := daemon.Ping(ctx, socket)
			cancel()
			if pingErr == nil {
				body, _ := os.ReadFile(logPath)
				matches := regexp.MustCompile("router=http://([^ ]+) ").FindAllStringSubmatch(string(body), -1)
				if len(matches) > 0 {
					routerURL = "http://" + matches[len(matches)-1][1]
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		if routerURL == "" {
			body, _ := os.ReadFile(logPath)
			t.Fatalf("daemon failed to become ready: %s", string(body))
		}
		return routerURL
	}

	routerURL := startDaemon()

	cli := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(root, "bin", "paas"), append([]string{"--socket", socket}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	query := func(host, path string) (int, string, error) {
		req, err := http.NewRequest("GET", routerURL+path, nil)
		if err != nil {
			return 0, "", err
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), nil
	}

	// 1. Add PostgreSQL managed service
	addOut, err := cli("add", "postgres", "--name", "db")
	if err != nil {
		t.Fatalf("paas add postgres failed: %v: %s", err, addOut)
	}
	if !strings.Contains(addOut, "✓ db READY") {
		t.Fatalf("unexpected paas add output: %s", addOut)
	}

	// 2. Verify paas services lists db
	servicesOut, err := cli("services")
	if err != nil {
		t.Fatalf("paas services failed: %v: %s", err, servicesOut)
	}
	if !strings.Contains(servicesOut, "db postgres READY") {
		t.Fatalf("unexpected services list: %s", servicesOut)
	}

	// 3. Link application to db
	linkOut, err := cli("link", "pgapp", "db")
	if err != nil {
		t.Fatalf("paas link failed: %v: %s", err, linkOut)
	}
	if !strings.Contains(linkOut, "✓ Injected DATABASE_URL into pgapp") {
		t.Fatalf("unexpected link output: %s", linkOut)
	}

	// 4. Deploy postgres-app
	pgSrc := filepath.Join(root, "examples", "postgres-app")
	deployOut, err := cli("deploy", pgSrc, "--name", "pgapp")
	if err != nil {
		t.Fatalf("deploy postgres-app failed: %v: %s", err, deployOut)
	}
	if !strings.Contains(deployOut, "State: RUNNING") {
		t.Fatalf("unexpected deploy output: %s", deployOut)
	}

	// 5. Query /counter twice (expect 1, then 2)
	status, body, err := query("pgapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "1" {
		t.Fatalf("counter query 1: status=%d body=%q err=%v", status, body, err)
	}

	status, body, err = query("pgapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "2" {
		t.Fatalf("counter query 2: status=%d body=%q err=%v", status, body, err)
	}

	// 6. Stop pgapp
	if stopOut, err := cli("stop", "pgapp"); err != nil {
		t.Fatalf("stop pgapp failed: %v: %s", err, stopOut)
	}

	// Verify route withdrawn
	s, _, _ := query("pgapp.localhost", "/counter")
	if s != 404 {
		t.Fatalf("expected 404 after stop, got %d", s)
	}

	// 7. Redeploy pgapp to verify persistent volume data survived
	deployOut2, err := cli("deploy", pgSrc, "--name", "pgapp")
	if err != nil {
		t.Fatalf("redeploy pgapp failed: %v: %s", err, deployOut2)
	}
	if !strings.Contains(deployOut2, "State: RUNNING") {
		t.Fatalf("unexpected redeploy output: %s", deployOut2)
	}

	// Query /counter (expect 3, then 4)
	status, body, err = query("pgapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "3" {
		t.Fatalf("counter query 3: status=%d body=%q err=%v", status, body, err)
	}

	status, body, err = query("pgapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "4" {
		t.Fatalf("counter query 4: status=%d body=%q err=%v", status, body, err)
	}

	// 8. Daemon restart reconciliation test:
	// Stop daemon and restart pointing to the same data directory
	stopDaemon()
	routerURL = startDaemon()

	// Query apps list after restart
	appsOut, err := cli("apps")
	if err != nil {
		t.Fatalf("apps after restart failed: %v: %s", err, appsOut)
	}
	if !strings.Contains(appsOut, "pgapp") {
		t.Fatalf("expected pgapp in apps list after restart: %s", appsOut)
	}
	// Managed PostgreSQL must be restored before its dependent application.
	// The recovered release continues from the same persistent database volume.
	status, body, err = query("pgapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "5" {
		t.Fatalf("counter after daemon restart: status=%d body=%q err=%v", status, body, err)
	}

	// Clean up: stop pgapp and services
	_, _ = cli("stop", "pgapp")
}
