package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// tailBuffer drains command output while retaining only a bounded tail.
type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= b.limit {
		b.data = append(b.data[:0], p[n-b.limit:]...)
		return n, nil
	}
	if overflow := len(b.data) + n - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func JournalLogs(ctx context.Context, unit string) (string, error) {
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "paas-deploy-"), ".service")
	expected, err := SystemdUnitName(id)
	if err != nil || unit != expected {
		return "", fmt.Errorf("invalid journal unit %q", unit)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := append(managerArgs(), "-u", unit, "--no-pager", "-n", "500", "-o", "cat", "--quiet")
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.WaitDelay = time.Second
	output := &tailBuffer{limit: 1 << 20}
	stderr := &tailBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read journal for %s: %w: %s", unit, errors.Join(err, ctx.Err()), strings.TrimSpace(string(stderr.data)))
	}
	return string(output.data), nil
}
