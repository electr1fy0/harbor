package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"paas/internal/service"
	"paas/internal/store"
)

func TestOfflineExportDecryptionAuthenticatesAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	manager := service.NewManager(state, dir)
	exported, err := manager.ExportEncrypted(context.Background(), "synthetic-passphrase", nil)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(exported.EncryptedData)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "archive.enc")
	output := filepath.Join(dir, "archive.tar.gz")
	if err = os.WriteFile(input, encrypted, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(password string) int {
		var stdout, stderr bytes.Buffer
		return runWithInput([]string{"export-decrypt", input, output, "--passphrase-stdin"}, strings.NewReader(password+"\n"), &stdout, &stderr)
	}
	if status := run("incorrect-passphrase"); status != 1 {
		t.Fatalf("wrong password status=%d", status)
	}
	if _, err = os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed authentication created output")
	}
	if status := run("synthetic-passphrase"); status != 0 {
		t.Fatalf("valid decryption status=%d", status)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("output mode=%v error=%v", info, err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 2 || before[0] != 0x1f || before[1] != 0x8b {
		t.Fatal("decrypted export is not gzip")
	}
	if status := run("synthetic-passphrase"); status != 1 {
		t.Fatalf("overwrite status=%d", status)
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("existing archive changed")
	}

	failedOutput := filepath.Join(dir, "archive-with-closed-stdout.tar.gz")
	var stderr bytes.Buffer
	stdout := &failAfterWrites{err: errors.New("output closed")}
	status := runWithInput([]string{"export-decrypt", input, failedOutput, "--passphrase-stdin"}, strings.NewReader("synthetic-passphrase\n"), stdout, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), "write confirmation: output closed") {
		t.Fatalf("failed success message status=%d stderr=%q", status, stderr.String())
	}
}
