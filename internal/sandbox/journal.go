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
	return JournalLogsWithLimit(ctx, unit, 1<<20)
}

// JournalLogsWithLimit returns a bounded journal tail with caller-selected
// lookbehind so downstream redaction can inspect secrets crossing its visible
// tail boundary before it trims the response.
func JournalLogsWithLimit(ctx context.Context, unit string, limit int) (string, error) {
	return JournalLogsWithLimitAndRecords(ctx, unit, limit, 500)
}

// JournalLogsWithLimitAndRecords also lets downstream consumers request
// additional journal records as redaction lookbehind. The byte tail remains
// bounded independently of the record count.
func JournalLogsWithLimitAndRecords(ctx context.Context, unit string, limit, recordLimit int) (string, error) {
	if limit < 1 || limit > 16<<20 {
		return "", errors.New("journal log tail limit must be between 1 byte and 16 MiB")
	}
	if recordLimit < 1 || recordLimit > (1<<20)+500 {
		return "", errors.New("journal record limit must be between 1 and 1 Mi records")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "paas-deploy-"), ".service")
	expected, err := SystemdUnitName(id)
	if err != nil || unit != expected {
		return "", fmt.Errorf("invalid journal unit %q", unit)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := append(managerArgs(), "-u", unit, "--no-pager", "-n", fmt.Sprint(recordLimit), "-o", "cat", "--quiet")
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.WaitDelay = time.Second
	output := &tailBuffer{limit: limit}
	stderr := &tailBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read journal for %s: %w: %s", unit, errors.Join(err, ctx.Err()), strings.TrimSpace(string(stderr.data)))
	}
	return string(output.data), nil
}
