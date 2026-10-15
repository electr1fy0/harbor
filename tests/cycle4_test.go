//go:build integration

package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
)

func TestCycle4NodeDeploymentAndConcurrency(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Node deployment acceptance requires Linux or macOS")
	}
	for _, tool := range []string{"node", "npm"} {
		if _, err := exec.LookPath(tool); err != nil {
			if runtime.GOOS == "darwin" {
				t.Skipf("Node deployment acceptance requires %s", tool)
			}
			t.Fatalf("Node deployment acceptance requires system-wide %s: %v", tool, err)
		}
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
		_ = filepath.WalkDir(work, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
		if err := os.RemoveAll(work); err != nil {
			t.Errorf("clean Node acceptance workspace: %v", err)
		}
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
		"-listen", "127.0.0.1:0", "-web-listen", "127.0.0.1:0",
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
	wantIsolation := "no"
	if runtime.GOOS == "linux" {
		wantIsolation = "yes"
	}
	if !strings.Contains(inspectOut, "PID isolation:     "+wantIsolation) {
		t.Errorf("inspect missing expected PID isolation %q: %s", wantIsolation, inspectOut)
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

func TestCycle4SQLiteManagedServiceAndPersistence(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	work, err := os.MkdirTemp("", "harbor-cycle4-sqlite-")
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
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(
			filepath.Join(root, "bin", "paasd"),
			"-socket", socket,
			"-data-dir", data,
			"-listen", "127.0.0.1:0", "-web-listen", "127.0.0.1:0",
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

	// Importing without a database must explain the application exit, even if
	// its short-lived systemd cgroup disappears while limits are being checked.
	sqliteSrc := filepath.Join(root, "examples", "sqlite-app")
	missingDBOut, err := cli("deploy", sqliteSrc, "--name", "sqliteapp")
	if err == nil || !strings.Contains(missingDBOut, "State: START_FAILED") || !strings.Contains(missingDBOut, "exit") || strings.Contains(missingDBOut, "memory.max") {
		t.Fatalf("unbound SQLite app should report its startup exit: %v: %s", err, missingDBOut)
	}
	missingDBLogs, err := cli("logs", "sqliteapp")
	if err != nil || !strings.Contains(missingDBLogs, "DATABASE_URL is required") {
		t.Fatalf("missing database diagnostics: %v: %s", err, missingDBLogs)
	}
	if status, _, err := query("sqliteapp.localhost", "/counter"); err != nil || status != http.StatusNotFound {
		t.Fatalf("unbound SQLite app published a route: status=%d err=%v", status, err)
	}

	// 1. Add SQLite managed service
	addOut, err := cli("add", "sqlite", "--name", "db")
	if err != nil {
		t.Fatalf("paas add sqlite failed: %v: %s", err, addOut)
	}
	if !strings.Contains(addOut, "✓ db READY") {
		t.Fatalf("unexpected paas add output: %s", addOut)
	}

	// 2. Verify paas services lists db
	servicesOut, err := cli("services")
	if err != nil {
		t.Fatalf("paas services failed: %v: %s", err, servicesOut)
	}
	if !strings.Contains(servicesOut, "db sqlite READY") {
		t.Fatalf("unexpected services list: %s", servicesOut)
	}

	// 3. Link application to db
	linkOut, err := cli("link", "sqliteapp", "db")
	if err != nil {
		t.Fatalf("paas link failed: %v: %s", err, linkOut)
	}
	if !strings.Contains(linkOut, "✓ Injected DATABASE_URL into sqliteapp") {
		t.Fatalf("unexpected link output: %s", linkOut)
	}

	// 4. Deploy the SQLite counter app
	deployOut, err := cli("deploy", sqliteSrc, "--name", "sqliteapp")
	if err != nil {
		buildLogs, _ := cli("logs", "sqliteapp")
		t.Fatalf("deploy sqlite-app failed: %v: %s\nbuild logs: %s", err, deployOut, buildLogs)
	}
	if !strings.Contains(deployOut, "State: RUNNING") {
		t.Fatalf("unexpected deploy output: %s", deployOut)
	}

	// 5. Query /counter twice (expect 1, then 2)
	status, body, err := query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "1" {
		t.Fatalf("counter query 1: status=%d body=%q err=%v", status, body, err)
	}

	status, body, err = query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "2" {
		t.Fatalf("counter query 2: status=%d body=%q err=%v", status, body, err)
	}

	// 6. Re-linking is idempotent and retains the binding's database.
	if relinkOut, err := cli("link", "sqliteapp", "db"); err != nil {
		t.Fatalf("repeat link failed: %v: %s", err, relinkOut)
	}
	status, body, err = query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "3" {
		t.Fatalf("counter after idempotent re-link: status=%d body=%q err=%v", status, body, err)
	}

	// 7. Redeploy and verify the per-binding database survives.
	deployOut2, err := cli("deploy", sqliteSrc, "--name", "sqliteapp")
	if err != nil {
		t.Fatalf("redeploy sqlite-app failed: %v: %s", err, deployOut2)
	}
	if !strings.Contains(deployOut2, "State: RUNNING") {
		t.Fatalf("unexpected redeploy output: %s", deployOut2)
	}

	// Query /counter after redeploy (expect 4).
	status, body, err = query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "4" {
		t.Fatalf("counter after redeploy: status=%d body=%q err=%v", status, body, err)
	}

	// Rollback starts the earlier immutable release and must use the same DB.
	rollbackOut, err := cli("rollback", "sqliteapp")
	if err != nil || !strings.Contains(rollbackOut, "State: RUNNING") {
		t.Fatalf("rollback sqliteapp failed: %v: %s", err, rollbackOut)
	}
	status, body, err = query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "5" {
		t.Fatalf("counter after rollback: status=%d body=%q err=%v", status, body, err)
	}

	// Stop and redeploy also preserve the file-backed database.
	if stopOut, err := cli("stop", "sqliteapp"); err != nil {
		t.Fatalf("stop sqliteapp failed: %v: %s", err, stopOut)
	}
	if s, _, _ := query("sqliteapp.localhost", "/counter"); s != 404 {
		t.Fatalf("expected 404 after stop, got %d", s)
	}
	if deployOut, err := cli("deploy", sqliteSrc, "--name", "sqliteapp"); err != nil || !strings.Contains(deployOut, "State: RUNNING") {
		t.Fatalf("redeploy after stop failed: %v: %s", err, deployOut)
	}
	status, body, err = query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "6" {
		t.Fatalf("counter after stop and redeploy: status=%d body=%q err=%v", status, body, err)
	}
	assertRootSQLitePermissions(t, data)

	// 8. Daemon restart reconciliation test:
	// Stop daemon and restart pointing to the same data directory
	stopDaemon()
	routerURL = startDaemon()

	// Query apps list after restart
	appsOut, err := cli("apps")
	if err != nil {
		t.Fatalf("apps after restart failed: %v: %s", err, appsOut)
	}
	if !strings.Contains(appsOut, "sqliteapp") {
		t.Fatalf("expected sqliteapp in apps list after restart: %s", appsOut)
	}
	// Recovery must preserve the database and publish the app after service
	// restoration.
	status, body, err = query("sqliteapp.localhost", "/counter")
	if err != nil || status != 200 || strings.TrimSpace(body) != "7" {
		daemonLog, _ := os.ReadFile(logPath)
		t.Fatalf("counter after daemon restart: status=%d body=%q err=%v; daemon log: %s", status, body, err, daemonLog)
	}

	// Clean up the recovered app; daemon shutdown handles service state.
	_, _ = cli("stop", "sqliteapp")
}

func assertRootSQLitePermissions(t *testing.T, dataDir string) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return
	}
	runtimeUser, err := user.Lookup("paas-runtime")
	if err != nil {
		t.Skipf("paas-runtime is not configured for root permission assertions: %v", err)
	}
	volumesDir := filepath.Join(dataDir, "volumes")
	var databasePaths []string
	err = filepath.WalkDir(volumesDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "database.sqlite" {
			databasePaths = append(databasePaths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("find SQLite database file: %v", err)
	}
	if len(databasePaths) != 1 {
		t.Fatalf("SQLite database files = %v, want exactly one", databasePaths)
	}
	wantUID := uint32(mustParseUserID(t, runtimeUser.Uid))
	checkOwner := func(path string, mode os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != wantUID || info.Mode().Perm() != mode {
			t.Fatalf("%s owner/mode = %+v, want UID %d and mode %04o", path, info, wantUID, mode)
		}
	}
	databasePath := databasePaths[0]
	checkOwner(filepath.Dir(databasePath), 0700)
	checkOwner(databasePath, 0600)
	for directory := filepath.Dir(filepath.Dir(databasePath)); ; directory = filepath.Dir(directory) {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatalf("stat SQLite parent %s: %v", directory, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0711 {
			t.Fatalf("SQLite parent %s mode = %04o, want execute-only 0711", directory, info.Mode().Perm())
		}
		if directory == dataDir {
			break
		}
	}
}

func mustParseUserID(t *testing.T, value string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
