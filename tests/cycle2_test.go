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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/daemon"
	"paas/internal/model"
	"paas/internal/sandbox"
)

type cycle2Harness struct{ root, socket, routerURL string }

func newCycle2Harness(t *testing.T) *cycle2Harness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux, systemd, cgroup v2, and Bubblewrap")
	}
	if _, err := sandbox.NewSystemd(); err != nil {
		t.Fatalf("Cycle 2 sandbox prerequisites: %v", err)
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
		// Releases are immutable directories; restore owner permissions for cleanup.
		if err := filepath.WalkDir(work, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return nil
		}); err != nil {
			t.Errorf("prepare cleanup: %v", err)
		}
		if err := os.RemoveAll(work); err != nil {
			t.Errorf("remove workspace: %v", err)
		}
	})
	if err := os.Chmod(work, 0755); err != nil {
		t.Fatal(err)
	}
	h := &cycle2Harness{root: root, socket: filepath.Join(work, "paasd.sock")}
	logPath := filepath.Join(work, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(root, "bin", "paasd"), "-socket", h.socket, "-data-dir", filepath.Join(work, "data"), "-listen", "127.0.0.1:0", "-sandbox", "systemd")
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil && !strings.Contains(err.Error(), "already finished") {
			t.Errorf("signal daemon: %v", err)
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(15 * time.Second):
			_ = command.Process.Kill()
			<-exited
			t.Error("daemon shutdown timed out")
		}
	})
	routerPattern := regexp.MustCompile(`router=http://([^ ]+) `)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, pingErr := daemon.Ping(ctx, h.socket)
		cancel()
		if pingErr == nil {
			body, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if match := routerPattern.FindStringSubmatch(string(body)); len(match) > 1 {
				h.routerURL = "http://" + match[1]
				return h
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, err := os.ReadFile(logPath)
	t.Fatalf("daemon readiness: %v\n%s", err, body)
	return nil
}

func (h *cycle2Harness) call(t *testing.T, op, name, source string) (*daemon.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	return daemon.Execute(ctx, h.socket, op, daemon.Request{Name: name, SourceDir: source})
}
func (h *cycle2Harness) cli(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(h.root, "bin", "paas"), append([]string{"--socket", h.socket}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("CLI %v: %v: %s", args, err, out)
	}
	return string(out)
}
func (h *cycle2Harness) route(t *testing.T, name string, want int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.routerURL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = name + ".localhost"
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("route %s status=%d want=%d body=%s", name, response.StatusCode, want, body)
	}
	return string(body)
}

func TestCycle2RuntimeIsolationAndLimits(t *testing.T) {
	h := newCycle2Harness(t)
	response, err := h.call(t, "deploy", "hello", filepath.Join(h.root, "examples", "hello-go"))
	if err != nil {
		t.Fatal(err)
	}
	d := response.Deployment
	if d == nil || d.State != model.Running || d.SandboxType != "systemd" || d.SystemdUnit == "" {
		t.Fatalf("incomplete systemd deployment: %+v", d)
	}
	if body := h.route(t, "hello", http.StatusOK); !strings.Contains(body, "hello from harbor") {
		t.Fatalf("unexpected body: %s", body)
	}
	sb, err := sandbox.NewSystemd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := sb.Inspect(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	cgroup := filepath.Join("/sys/fs/cgroup", info.ControlGroup)
	for file, want := range map[string]string{"memory.max": "134217728", "memory.swap.max": "0", "pids.max": "64"} {
		body, err := os.ReadFile(filepath.Join(cgroup, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(body)) != want {
			t.Fatalf("%s=%q want=%s", file, body, want)
		}
	}
	cpu, err := os.ReadFile(filepath.Join(cgroup, "cpu.max"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(string(cpu))
	if len(parts) != 2 {
		t.Fatalf("invalid cpu.max=%q", cpu)
	}
	quota, qerr := strconv.ParseFloat(parts[0], 64)
	period, perr := strconv.ParseFloat(parts[1], 64)
	if qerr != nil || perr != nil || period <= 0 || quota/period != 0.5 {
		t.Fatalf("CPU quota unenforced: %q", cpu)
	}
	assertRuntimeNamespaces(t, cgroup)
	output := h.cli(t, "inspect", "hello")
	for _, expected := range []string{"State: RUNNING", "PID isolation:     yes", "Mount isolation:   yes", "Memory limit: 128Mi", "CPU quota:    50%", "Tasks limit:  64", "Unit: paas-deploy-dep_"} {
		if !strings.Contains(output, expected) {
			t.Errorf("inspect missing %q:\n%s", expected, output)
		}
	}
	logs := h.cli(t, "logs", "hello")
	if !strings.Contains(logs, "hello-go listening on 127.0.0.1:") {
		t.Errorf("runtime logs missing:\n%s", logs)
	}
	if _, err := h.call(t, "stop", "hello", ""); err != nil {
		t.Fatal(err)
	}
	h.route(t, "hello", http.StatusNotFound)
}

func assertRuntimeNamespaces(t *testing.T, cgroup string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	uid := ""
	if os.Geteuid() == 0 {
		out, err := exec.Command("id", "-u", "paas-runtime").Output()
		if err != nil {
			t.Fatal(err)
		}
		uid = strings.TrimSpace(string(out))
	}
	isolated := false
	for _, pid := range strings.Fields(string(body)) {
		if uid != "" {
			status, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
			if err != nil {
				t.Fatal(err)
			}
			matched := false
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "Uid:") {
					fields := strings.Fields(line)
					matched = len(fields) == 5 && fields[1] == uid && fields[2] == uid
				}
			}
			if !matched {
				t.Fatalf("runtime process %s does not run as paas-runtime:\n%s", pid, status)
			}
		}
		all := true
		for _, ns := range []string{"pid", "mnt", "ipc", "uts", "user", "net"} {
			host, err := os.Readlink(filepath.Join("/proc/self/ns", ns))
			if err != nil {
				t.Fatal(err)
			}
			child, err := os.Readlink(filepath.Join("/proc", pid, "ns", ns))
			if err != nil {
				t.Fatal(err)
			}
			if ns == "net" {
				if child != host {
					t.Fatalf("runtime unexpectedly isolated networking")
				}
			} else if child == host {
				all = false
			}
		}
		isolated = isolated || all
	}
	if !isolated {
		t.Fatal("no runtime process has the required namespace isolation")
	}
}

func TestCycle2MemoryHogOOMKill(t *testing.T) {
	h := newCycle2Harness(t)
	response, deployErr := h.call(t, "deploy", "memhog", filepath.Join(h.root, "examples", "memory-hog"))
	if response == nil || response.Deployment == nil {
		t.Fatalf("deploy produced no deployment: %v", deployErr)
	}
	deadline := time.Now().Add(10 * time.Second)
	var d *model.Deployment
	for time.Now().Before(deadline) {
		response, err := h.call(t, "inspect", "memhog", "")
		if err != nil {
			t.Fatal(err)
		}
		d = response.Deployment
		if d != nil && d.State == model.OOMKilled {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d == nil || d.State != model.OOMKilled {
		t.Fatalf("expected OOM_KILLED; deployErr=%v deployment=%+v", deployErr, d)
	}
	output := h.cli(t, "inspect", "memhog")
	for _, expected := range []string{"State: OOM_KILLED", "Failure:", "Result: oom-kill", fmt.Sprintf("Memory limit: %s", d.MemoryLimit)} {
		if !strings.Contains(output, expected) {
			t.Errorf("inspect missing %q:\n%s", expected, output)
		}
	}
	h.route(t, "memhog", http.StatusNotFound)
}
