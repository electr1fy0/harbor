package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitMetadataReadsExactCommitWithoutRemoteAccess(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, data)
		}
		return strings.TrimSpace(string(data))
	}
	run("init", "-q")
	run("config", "user.name", "Harbor Test")
	run("config", "user.email", "test@example.com")
	os.Mkdir(filepath.Join(dir, "nested"), 0700)
	os.WriteFile(filepath.Join(dir, "nested", "file.txt"), []byte("hello"), 0600)
	run("add", ".")
	run("commit", "-q", "-m", "Deploy exact commit")
	sha := run("rev-parse", "HEAD")
	metadata, err := ReadGitMetadata(context.Background(), filepath.Join(dir, "nested"), "https://github.com/example/repository", "")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CommitSHA != sha || metadata.Author != "Harbor Test" || metadata.Message != "Deploy exact commit" || metadata.URL != "https://github.com/example/repository/commit/"+sha {
		t.Fatalf("metadata=%+v", metadata)
	}
}
func TestMetadataOutputDrainsWithoutUnboundedAllocation(t *testing.T) {
	var writer boundedMetadataOutput
	content := []byte(strings.Repeat("x", 64<<10))
	n, err := writer.Write(content)
	if err != nil || n != len(content) || writer.Len() != 16<<10 || !writer.tooLarge {
		t.Fatalf("n=%d len=%d tooLarge=%v err=%v", n, writer.Len(), writer.tooLarge, err)
	}
	writer.Write(content)
	if writer.Len() != 16<<10 {
		t.Fatal("buffer grew past bound")
	}
}
