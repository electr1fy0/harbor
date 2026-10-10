//go:build linux

package builder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRootBuildResourceProperties(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root and the dedicated build account; run make test-privileged")
	}
	work, err := os.MkdirTemp("/tmp", "harbor-build-limits-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeTree(work)
	if err := os.Chmod(work, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(work, "build.log")
	request := fixtureRequest(writeGoFixture(t, filepath.Join(work, "source")), logPath)
	request.BuildCommand = `set -eu; unit=$(basename "$(cut -d: -f3 /proc/self/cgroup)"); systemctl show "$unit" -p User -p MemoryMax -p MemorySwapMax -p TasksMax -p CPUQuotaPerSecUSec -p RuntimeMaxUSec; printf '#!/bin/sh\nexit 0\n' > "$PAAS_OUT/server"; chmod +x "$PAAS_OUT/server"`
	if _, err := New(Options{DataDir: filepath.Join(work, "data"), Timeout: 10 * time.Second}).Build(context.Background(), request); err != nil {
		body, _ := os.ReadFile(logPath)
		t.Fatalf("root build: %v: %s", err, body)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"User=paas-build", "MemoryMax=536870912", "MemorySwapMax=0", "TasksMax=128", "CPUQuotaPerSecUSec=1s", "RuntimeMaxUSec=10s"} {
		if !strings.Contains(string(body), expected+"\n") {
			t.Fatalf("effective build property %q missing: %s", expected, body)
		}
	}
	t.Log("effective build user, memory, swap, task, CPU, and timeout limits verified inside the active unit")
}
