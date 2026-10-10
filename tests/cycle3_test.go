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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
)

func TestCycle3ZeroDowntimeRedeploymentAndRollback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Cycle 3 integration test")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("/tmp", "harbor-cycle3-")
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

	// Wait for daemon readiness and discover router port
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

	// 1. Deploy hello-go v1
	helloSrc := filepath.Join(root, "examples", "hello-go")
	deployOut1 := cli("deploy", helloSrc, "--name", "hello")
	if !strings.Contains(deployOut1, "State: RUNNING") {
		t.Fatalf("unexpected v1 deploy output: %s", deployOut1)
	}

	// Verify v1 responds
	req, _ := http.NewRequest("GET", routerURL+"/", nil)
	req.Host = "hello.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request to hello.localhost failed: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), "hello from harbor") {
		t.Fatalf("expected v1 response, got %s", string(bodyBytes))
	}

	// 2. Prepare hello-go v2 source with modified response body
	v2Dir := filepath.Join(work, "hello-v2")
	if err := os.MkdirAll(v2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	mainGoBytes, err := os.ReadFile(filepath.Join(helloSrc, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	modifiedMain := strings.Replace(string(mainGoBytes), `"hello from harbor"`, `"hello from harbor v2"`, 1)
	if err := os.WriteFile(filepath.Join(v2Dir, "main.go"), []byte(modifiedMain), 0644); err != nil {
		t.Fatal(err)
	}
	paasYamlBytes, err := os.ReadFile(filepath.Join(helloSrc, "paas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2Dir, "paas.yaml"), paasYamlBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Zero-downtime redeployment with concurrent HTTP probe
	var probeErrors atomic.Int64
	var totalRequests atomic.Int64
	var seenV1 atomic.Bool
	var seenV2 atomic.Bool
	stopProbe := make(chan struct{})
	var probeWg sync.WaitGroup

	probeWg.Add(1)
	go func() {
		defer probeWg.Done()
		client := &http.Client{Timeout: time.Second}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopProbe:
				return
			case <-ticker.C:
				r, err := http.NewRequest("GET", routerURL+"/", nil)
				if err != nil {
					continue
				}
				r.Host = "hello.localhost"
				res, doErr := client.Do(r)
				totalRequests.Add(1)
				if doErr != nil {
					probeErrors.Add(1)
					continue
				}
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if res.StatusCode != 200 {
					probeErrors.Add(1)
				}
				if strings.Contains(string(body), "hello from harbor v2") {
					seenV2.Store(true)
				} else if strings.Contains(string(body), "hello from harbor") {
					seenV1.Store(true)
				}
			}
		}
	}()

	// Perform redeploy of v2
	deployOut2 := cli("deploy", v2Dir, "--name", "hello")
	if !strings.Contains(deployOut2, "State: RUNNING") {
		t.Fatalf("unexpected v2 deploy output: %s", deployOut2)
	}

	// Give router a moment to complete traffic transition and drain grace period
	time.Sleep(1 * time.Second)
	close(stopProbe)
	probeWg.Wait()

	t.Logf("Probe completed: total=%d, errors=%d, seenV1=%v, seenV2=%v",
		totalRequests.Load(), probeErrors.Load(), seenV1.Load(), seenV2.Load())

	if probeErrors.Load() > 0 {
		t.Fatalf("zero-downtime violation: %d requests failed during redeployment", probeErrors.Load())
	}
	if !seenV1.Load() || !seenV2.Load() {
		t.Fatalf("expected to see both v1 and v2 during zero-downtime redeploy: v1=%v v2=%v", seenV1.Load(), seenV2.Load())
	}

	// 4. Verify release history
	releasesOut := cli("releases", "hello")
	if !strings.Contains(releasesOut, "RELEASE SOURCE_HASH CREATED ACTIVE") {
		t.Fatalf("unexpected releases header: %s", releasesOut)
	}
	lines := strings.Split(strings.TrimSpace(releasesOut), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 2 releases in history, got output:\n%s", releasesOut)
	}
	if !strings.HasSuffix(lines[1], "yes") {
		t.Errorf("expected newest release to be ACTIVE=yes, got: %s", lines[1])
	}
	if !strings.HasSuffix(lines[2], "no") {
		t.Errorf("expected older release to be ACTIVE=no, got: %s", lines[2])
	}

	// 5. Rollback to v1
	rollbackOut := cli("rollback", "hello")
	if !strings.Contains(rollbackOut, "State: RUNNING") {
		t.Fatalf("unexpected rollback output: %s", rollbackOut)
	}

	// Verify route now serves v1 again
	reqV1, _ := http.NewRequest("GET", routerURL+"/", nil)
	reqV1.Host = "hello.localhost"
	respV1, err := http.DefaultClient.Do(reqV1)
	if err != nil {
		t.Fatalf("HTTP request after rollback failed: %v", err)
	}
	defer respV1.Body.Close()
	bodyV1, _ := io.ReadAll(respV1.Body)
	if strings.Contains(string(bodyV1), "hello from harbor v2") || !strings.Contains(string(bodyV1), "hello from harbor") {
		t.Fatalf("expected v1 response after rollback, got %s", string(bodyV1))
	}

	// 6. Stop application
	stopOut := cli("stop", "hello")
	if !strings.Contains(stopOut, "Stopped hello") {
		t.Fatalf("unexpected stop output: %s", stopOut)
	}

	// Route withdrawn
	req404, _ := http.NewRequest("GET", routerURL+"/", nil)
	req404.Host = "hello.localhost"
	resp404, err := http.DefaultClient.Do(req404)
	if err != nil {
		t.Fatal(err)
	}
	resp404.Body.Close()
	if resp404.StatusCode != 404 {
		t.Fatalf("expected 404 after stop, got %d", resp404.StatusCode)
	}
}

func TestCycle3CandidateFailureSafety(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Cycle 3 integration test")
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("/tmp", "harbor-cycle3-fail-")
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

	// 1. Deploy hello-go
	helloSrc := filepath.Join(root, "examples", "hello-go")
	deployOut, err := cli("deploy", helloSrc, "--name", "hello")
	if err != nil || !strings.Contains(deployOut, "State: RUNNING") {
		t.Fatalf("initial deploy failed: %s (err: %v)", deployOut, err)
	}

	assertLive := func(tag string) {
		req, _ := http.NewRequest("GET", routerURL+"/", nil)
		req.Host = "hello.localhost"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("[%s] HTTP request to hello failed: %v", tag, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(body), "hello from harbor") {
			t.Fatalf("[%s] live app disturbed: status=%d body=%s", tag, resp.StatusCode, string(body))
		}
		inspectOut, inspectErr := cli("inspect", "hello")
		if inspectErr != nil || !strings.Contains(inspectOut, "State: RUNNING") {
			t.Fatalf("[%s] inspect failed: %s", tag, inspectOut)
		}
	}

	// 2. Candidate build failure must not disturb live app
	brokenSrc := filepath.Join(root, "examples", "broken-build")
	failOut, failErr := cli("deploy", brokenSrc, "--name", "hello")
	if failErr == nil {
		t.Fatalf("expected broken build to fail, got success: %s", failOut)
	}
	assertLive("after broken build candidate")

	// 3. Candidate health failure must not disturb live app
	unhealthySrc := filepath.Join(root, "examples", "unhealthy")
	failOut2, failErr2 := cli("deploy", unhealthySrc, "--name", "hello")
	if failErr2 == nil {
		t.Fatalf("expected unhealthy candidate to fail, got success: %s", failOut2)
	}
	assertLive("after unhealthy candidate")

	// 4. Verify rollback refuses to select the failed unhealthy candidate
	rbOut, rbErr := cli("rollback", "hello")
	if rbErr == nil {
		t.Fatalf("expected rollback to fail with no previous release, got success: %s", rbOut)
	}
	assertLive("after rejected rollback to failed candidate")

	_, _ = cli("stop", "hello")
}
