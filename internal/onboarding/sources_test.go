package onboarding

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceStoreWithoutDataDirectoryFailsClosed(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	sentinel := []byte("do not overwrite current-directory data")
	if err := os.WriteFile(sourceFileName, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSourceStore("")
	settings := SourceSettings{Name: "sample-app", RepoURL: "https://github.com/example/sample-app"}
	if err := store.save(settings); err == nil {
		t.Fatal("save succeeded without a configured data directory")
	}
	if err := store.delete(settings.Name); err == nil {
		t.Fatal("delete succeeded without a configured data directory")
	}
	data, err := os.ReadFile(filepath.Join(workingDir, sourceFileName))
	if err != nil || string(data) != string(sentinel) {
		t.Fatalf("current-directory file changed: %q, err=%v", data, err)
	}
}

func TestUpdateSourceLeavesSettingsUnchangedWhenProtectedStoreIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	sources := newSourceStore(dir)
	protected := newProtectedStore(dir)
	previous := SourceSettings{Name: "sample-app", RepoURL: "https://github.com/example/old"}
	if err := sources.save(previous); err != nil {
		t.Fatal(err)
	}
	if err := protected.update(func(state *protectedState) error {
		state.BuildEnvironment[previous.Name] = map[string]string{"API_TOKEN": "old-token"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected.path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := &Manager{sources: sources, protected: protected}
	environment := map[string]string{"API_TOKEN": "new-token"}
	err := manager.UpdateSource(context.Background(), SourceSettings{
		Name: previous.Name, RepoURL: "https://github.com/example/new",
	}, &environment)
	if err == nil {
		t.Fatal("UpdateSource succeeded with unreadable protected state")
	}
	got, err := sources.get(previous.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoURL != previous.RepoURL {
		t.Fatalf("source settings changed after failed update: %+v", got)
	}
}

func TestUpdateSourceRollsBackSecretsWhenSourceSaveFails(t *testing.T) {
	dir := t.TempDir()
	sources := newSourceStore(dir)
	protected := newProtectedStore(dir)
	previous := SourceSettings{Name: "sample-app", RepoURL: "https://github.com/example/old"}
	if err := sources.save(previous); err != nil {
		t.Fatal(err)
	}
	if err := protected.update(func(state *protectedState) error {
		state.BuildEnvironment[previous.Name] = map[string]string{"API_TOKEN": "old-token"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sources.path, 0o644); err != nil {
		t.Fatal(err)
	}

	manager := &Manager{sources: sources, protected: protected}
	environment := map[string]string{"API_TOKEN": "new-token"}
	err := manager.UpdateSource(context.Background(), SourceSettings{
		Name: previous.Name, RepoURL: "https://github.com/example/new",
	}, &environment)
	if err == nil {
		t.Fatal("UpdateSource succeeded despite unsafe source settings file")
	}
	state, err := protected.read()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.BuildEnvironment[previous.Name]["API_TOKEN"]; got != "old-token" {
		t.Fatalf("protected environment after failed update = %q, want old value", got)
	}
	data, err := os.ReadFile(sources.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), previous.RepoURL) || strings.Contains(string(data), "example/new") {
		t.Fatalf("source file changed after failed update: %s", data)
	}
}

func TestSaveSourceForCurrentJobDoesNotPartiallyUpdateSettings(t *testing.T) {
	dir := t.TempDir()
	sources := newSourceStore(dir)
	protected := newProtectedStore(dir)
	previous := SourceSettings{Name: "sample-app", RepoURL: "https://github.com/example/old"}
	if err := sources.save(previous); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected.path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		sources:   sources,
		protected: protected,
		jobs: map[string]*jobRecord{
			"job_active": {Job: Job{ID: "job_active", Name: previous.Name, State: "RUNNING"}},
		},
	}
	err := manager.saveSourceForCurrentJob("job_active", SourceSettings{
		Name: previous.Name, RepoURL: "https://github.com/example/new",
	}, map[string]string{"API_TOKEN": "new-token"})
	if err == nil {
		t.Fatal("source save succeeded with unreadable protected state")
	}
	got, err := sources.get(previous.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoURL != previous.RepoURL {
		t.Fatalf("source settings changed after failed save: %+v", got)
	}
}

func TestForgetRestoresSourceAndInMemoryJobsWhenProtectedCommitFails(t *testing.T) {
	dir := t.TempDir()
	sources := newSourceStore(dir)
	protected := newProtectedStore(dir)
	settings := SourceSettings{Name: "forget-app", RepoURL: "https://github.com/example/forget-app", Ref: "main"}
	if err := sources.save(settings); err != nil {
		t.Fatal(err)
	}
	settings, err := sources.get(settings.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := protected.update(func(state *protectedState) error {
		state.BuildEnvironment[settings.Name] = map[string]string{"API_TOKEN": "secret"}
		state.Hooks["hook_forget"] = storedHook{Metadata: HookMetadata{ID: "hook_forget", Name: settings.Name}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	protectedBefore, err := os.ReadFile(protected.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(protected.path, 0o644); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		sources: sources, protected: protected,
		jobs: map[string]*jobRecord{
			"job_forget": {Job: Job{ID: "job_forget", Name: settings.Name, State: "QUEUED"}},
		},
		order: []string{"job_forget"}, outstanding: 1,
		dedup:     map[string]dedupRecord{"delivery:hook_forget:one": {JobID: "job_forget"}},
		forgotten: map[string]bool{}, queue: make(chan string, maxOutstandingJobs),
	}
	manager.queue <- "job_forget"

	if err := manager.Forget(settings.Name); err == nil {
		t.Fatal("Forget succeeded while protected state was unsafe to read")
	}
	got, err := sources.get(settings.Name)
	if err != nil {
		t.Fatalf("source settings were not restored: %v", err)
	}
	if got.RepoURL != settings.RepoURL || !got.UpdatedAt.Equal(settings.UpdatedAt) {
		t.Fatalf("restored source settings = %+v, want original %+v", got, settings)
	}
	protectedAfter, err := os.ReadFile(protected.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(protectedBefore, protectedAfter) {
		t.Fatal("protected state changed despite failed Forget")
	}
	if manager.jobs["job_forget"] == nil || len(manager.order) != 1 || manager.order[0] != "job_forget" {
		t.Fatal("in-memory jobs changed despite failed Forget")
	}
	if manager.outstanding != 1 || len(manager.queue) != 1 || manager.forgotten[settings.Name] {
		t.Fatalf("manager state changed despite failed Forget: outstanding=%d queue=%d forgotten=%v", manager.outstanding, len(manager.queue), manager.forgotten)
	}
	if manager.dedup["delivery:hook_forget:one"].JobID != "job_forget" {
		t.Fatal("deduplication state changed despite failed Forget")
	}
}
