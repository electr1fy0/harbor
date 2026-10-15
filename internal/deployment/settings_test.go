package deployment

import (
	"context"
	"errors"
	"os"
	"paas/internal/contracts"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paas/internal/model"
	"paas/internal/sandbox"
)

func TestConfigureEncryptsSecretsAndRedactsRotationHistory(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first := "first-super-secret"
	empty := ""
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		Environment: map[string]string{"TOKEN": first, "EMPTY_VALUE": empty},
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := h.db.AppSettings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.EncryptedEnvironment), first) || strings.Contains(string(stored.EncryptedRedactionHistory), first) {
		t.Fatal("environment secret was stored in plaintext")
	}
	keyInfo, err := os.Stat(filepath.Join(filepath.Dir(h.dbPath), "data", "environment.key"))
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("environment key permissions = %04o, want 0600", keyInfo.Mode().Perm())
	}

	settings, err := h.service.Settings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(settings.EnvironmentKeys, ",") != "EMPTY_VALUE,TOKEN" {
		t.Fatalf("environment keys = %v", settings.EnvironmentKeys)
	}

	deployed, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(h.sandbox.specs[len(h.sandbox.specs)-1].Env, "TOKEN"); got != first {
		t.Fatalf("runtime TOKEN = %q, want configured value", got)
	}
	if got := envValue(h.sandbox.specs[len(h.sandbox.specs)-1].Env, "EMPTY_VALUE"); got != "" {
		t.Fatalf("runtime empty environment value = %q", got)
	}
	if err := os.WriteFile(deployed.LogPath, []byte("old="+first+" current=second-super-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second := "second-super-secret"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": second}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{RemoveEnvironment: []string{"TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	logs, err := h.service.Logs("demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs, first) || strings.Contains(logs, second) || !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("historical secret leaked from logs: %q", logs)
	}
	settings, err = h.service.Settings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.EnvironmentKeys) != 1 || settings.EnvironmentKeys[0] != "EMPTY_VALUE" {
		t.Fatalf("settings keys after removal = %v", settings.EnvironmentKeys)
	}
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("builder diagnostic contains " + first)})
	failed, deployErr := h.service.Deploy(context.Background(), h.source, "demo")
	if deployErr == nil || strings.Contains(deployErr.Error(), first) || failed == nil || strings.Contains(failed.Error, first) {
		t.Fatalf("failure diagnostics leaked a rotated secret: deployment=%+v error=%v", failed, deployErr)
	}
}

func TestConfiguredResourcesApplyToFutureDeployment(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	memory := "192Mi"
	cpu := 0.75
	tasks := 27
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		MemoryLimit: &memory, CPUQuota: &cpu, TasksLimit: &tasks,
	}); err != nil {
		t.Fatal(err)
	}
	d, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if d.MemoryLimit != memory || d.CPUQuota != cpu || d.TasksLimit != tasks {
		t.Fatalf("deployment resources = %s, %v, %d", d.MemoryLimit, d.CPUQuota, d.TasksLimit)
	}
	settings, err := h.service.Settings("demo")
	if err != nil || settings.MemoryLimit != memory || settings.CPUQuota != cpu || settings.TasksLimit != tasks {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"DATABASE_URL": "nope"}}); err == nil {
		t.Fatal("reserved environment key was accepted")
	}
}

func TestRejectedConfigureDoesNotCreateApplication(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	invalidMemory := "not-a-memory-limit"
	zeroTasks := 0
	for _, change := range []ConfigureOptions{
		{Environment: map[string]string{"PORT": "8080"}},
		{MemoryLimit: &invalidMemory},
		{TasksLimit: &zeroTasks},
	} {
		if _, err := h.service.Configure(context.Background(), "invalid-config", change); err == nil {
			t.Fatalf("Configure(%+v) succeeded", change)
		}
		if _, err := h.db.AppSettings("invalid-config"); err == nil {
			t.Fatalf("rejected Configure(%+v) left an application behind", change)
		}
	}
}

func TestRedactionFailsClosedWhenKeyCannotBeRead(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	secret := "never-return-this-secret"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": secret}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.dbPath), "data", "environment.key"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	redacted, err := h.service.redactText("demo", secret)
	if err == nil || redacted != "" || strings.Contains(redacted, secret) {
		t.Fatalf("redactText leaked secret after key failure: %q, %v", redacted, err)
	}
}

func TestRedactionProcessesOverlappingSecretsLongestFirst(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		Environment: map[string]string{"SHORT": "pass", "LONG": "password"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := h.service.redactText("demo", "password pass")
	if err != nil {
		t.Fatal(err)
	}
	if got != "[REDACTED] [REDACTED]" {
		t.Fatalf("redactText() = %q, want both complete values redacted", got)
	}
}

func TestRedactionCoversBoundDatabaseURLAndPassword(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	app, err := h.db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	service := model.Service{ID: "svc_redact", Name: "db", Type: "postgres", Port: 5432, State: model.ServiceReady, CreatedAt: time.Now().UTC()}
	if err := h.db.CreateService(service); err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{ID: "binding_redact", AppID: app.ID, ServiceID: service.ID, DatabaseName: "appdb", Username: "appuser", Password: "binding-password", EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC()}
	if err := h.db.CreateBinding(binding); err != nil {
		t.Fatal(err)
	}
	databaseURL := "postgres://appuser:binding-password@127.0.0.1:5432/appdb?sslmode=disable"
	got, err := h.service.redactText("demo", "DATABASE_URL="+databaseURL+" password=binding-password")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "binding-password") || strings.Contains(got, databaseURL) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("bound database credentials leaked: %q", got)
	}
}

func envValue(entries []string, key string) string {
	for _, entry := range entries {
		if name, value, ok := strings.Cut(entry, "="); ok && name == key {
			return value
		}
	}
	return "<missing>"
}

func TestLocalRouteSurvivesCandidateFailureAndWithdrawsOnStop(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	active, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("broken candidate")})
	failed, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil || failed.State != model.BuildFailed {
		t.Fatalf("candidate = %+v, %v", failed, err)
	}
	routes, err := h.db.Routes()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"demo.localhost"} {
		if routes[host] != active.ID || routerRequest(h.router, host, "/").Code != 200 {
			t.Fatalf("candidate failure changed active alias %s", host)
		}
	}
	if _, err := h.service.Stop(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	routes, err = h.db.Routes()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"demo.localhost"} {
		if routes[host] != "" || routerRequest(h.router, host, "/").Code != 404 {
			t.Fatalf("stopped app still has alias %s", host)
		}
	}
}

func TestNoopSettingsDoNotInventPendingRuntimeChanges(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": "retained-secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Deploy(context.Background(), h.source, "demo"); err != nil {
		t.Fatal(err)
	}
	before, err := h.service.OperationSettings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": "retained-secret"}}); err != nil {
		t.Fatal(err)
	}
	after, err := h.service.OperationSettings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if after.PendingSettings || after.SettingsRevision != before.SettingsRevision {
		t.Fatalf("noop change advanced runtime revision: before=%+v after=%+v", before, after)
	}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": "new-secret"}}); err != nil {
		t.Fatal(err)
	}
	pending, err := h.service.OperationSettings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !pending.PendingSettings {
		t.Fatal("real environment change was not pending")
	}
}

func TestBuildSecretsRemainRedactedFromRuntimeAndHistoricalLogs(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.DeployWithOptions(context.Background(), h.source, "demo", contracts.DeployOptions{BuildEnvironment: map[string]string{"BUILD_TOKEN": "old-build-secret"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(first.LogPath, []byte("runtime printed old-build-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := h.service.DeployWithOptions(context.Background(), h.source, "demo", contracts.DeployOptions{BuildEnvironment: map[string]string{"BUILD_TOKEN": "new-build-secret"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(second.LogPath, []byte("runtime printed new-build-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		logs, err := h.service.LogsForDeployment("demo", id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs, "old-build-secret") || strings.Contains(logs, "new-build-secret") {
			t.Fatalf("build secret leaked: %s", logs)
		}
	}
	settings, err := h.service.OperationSettings("demo")
	if err != nil {
		t.Fatal(err)
	}
	if settings.PendingSettings {
		t.Fatal("redaction-only history change made runtime configuration pending")
	}
}

func TestConfiguredSecretCrossingLogTailBoundaryIsRedacted(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	secret := "runtime-boundary-secret-value"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		Environment: map[string]string{"TOKEN": secret},
	}); err != nil {
		t.Fatal(err)
	}
	deployment, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	// The final 1 MiB begins 16 bytes before the secret ends, so the old
	// tail-first redaction flow received only a secret suffix it could not
	// match. The lookbehind must preserve the full configured secret.
	logs := "prefix10!!" + secret + strings.Repeat("x", (1<<20)-16)
	if err := os.WriteFile(deployment.LogPath, []byte(logs), 0600); err != nil {
		t.Fatal(err)
	}
	for _, read := range []struct {
		name string
		call func() (string, error)
	}{
		{name: "all logs", call: func() (string, error) {
			return h.service.LogsForDeployment("demo", deployment.ID)
		}},
		{name: "build logs", call: func() (string, error) {
			return h.service.LogsBySource("demo", deployment.ID, "build")
		}},
	} {
		got, err := read.call()
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		if strings.Contains(got, secret) || strings.Contains(got, secret[len(secret)-16:]) {
			t.Fatalf("%s leaked the boundary-spanning secret: suffix=%q", read.name, got[:min(len(got), 96)])
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%s omitted the redaction marker: %q", read.name, got[:min(len(got), 96)])
		}
	}
}

func TestRedactionShrinkCannotExposeSecretFromBeforeLogTail(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	secret := "runtime-boundary-secret-value"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		Environment: map[string]string{"TOKEN": secret},
	}); err != nil {
		t.Fatal(err)
	}
	deployment, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	logs := repeatedSecretTailFixture(secret)
	if err := os.WriteFile(deployment.LogPath, []byte(logs), 0600); err != nil {
		t.Fatal(err)
	}
	for _, read := range []struct {
		name string
		call func() (string, error)
	}{
		{name: "all logs", call: func() (string, error) {
			return h.service.LogsForDeployment("demo", deployment.ID)
		}},
		{name: "build logs", call: func() (string, error) {
			return h.service.LogsBySource("demo", deployment.ID, "build")
		}},
	} {
		got, err := read.call()
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		for _, fragment := range []string{secret, secret[19:], secret[20:]} {
			if strings.Contains(got, fragment) {
				t.Fatalf("%s leaked a secret fragment after redaction shrinkage: %q", read.name, fragment)
			}
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%s omitted redaction marker", read.name)
		}
	}
}

func TestSystemdRuntimeLogTailUsesRedactionLookbehind(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	secret := "runtime-boundary-secret-value"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		Environment: map[string]string{"TOKEN": secret},
	}); err != nil {
		t.Fatal(err)
	}
	deployment, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := sandbox.SystemdUnitName(deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	deployment.SystemdUnit = unit
	deployment.SandboxType = "systemd"
	if err := h.db.UpdateDeployment(*deployment); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	journalPath := filepath.Join(directory, "journal.log")
	if err := os.WriteFile(journalPath, []byte(repeatedSecretTailFixture(secret)), 0600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	journalctl := filepath.Join(binDir, "journalctl")
	if err := os.WriteFile(journalctl, []byte("#!/bin/sh\nexec /bin/cat \"$HARBOR_TEST_JOURNAL_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("HARBOR_TEST_JOURNAL_LOG", journalPath)

	for _, read := range []struct {
		name string
		call func() (string, error)
	}{
		{name: "all logs", call: func() (string, error) {
			return h.service.LogsForDeployment("demo", deployment.ID)
		}},
		{name: "runtime logs", call: func() (string, error) {
			return h.service.LogsBySource("demo", deployment.ID, "runtime")
		}},
	} {
		got, err := read.call()
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		for _, fragment := range []string{secret, secret[19:], secret[20:]} {
			if strings.Contains(got, fragment) {
				t.Fatalf("%s leaked a journal secret fragment: %q", read.name, fragment)
			}
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%s omitted redaction marker", read.name)
		}
	}
}

func TestSystemdRuntimeLogRedactsSecretsAcrossJournalRecordLimit(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	secret := "journal-secret-first\njournal-secret-middle\njournal-secret-last"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{
		Environment: map[string]string{"TOKEN": secret},
	}); err != nil {
		t.Fatal(err)
	}
	deployment, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := sandbox.SystemdUnitName(deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	deployment.SystemdUnit = unit
	deployment.SandboxType = "systemd"
	if err := h.db.UpdateDeployment(*deployment); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	journalPath := filepath.Join(directory, "journal.log")
	journal := secret + "\n" + strings.Repeat("ordinary journal event\n", 499)
	if err := os.WriteFile(journalPath, []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	journalctl := filepath.Join(binDir, "journalctl")
	script := "#!/bin/sh\nrecords=500\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"-n\" ]; then shift; records=\"$1\"; fi\n  shift\ndone\nexec /usr/bin/tail -n \"$records\" \"$HARBOR_TEST_JOURNAL_LOG\"\n"
	if err := os.WriteFile(journalctl, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("HARBOR_TEST_JOURNAL_LOG", journalPath)

	for _, read := range []struct {
		name string
		call func() (string, error)
	}{
		{name: "all logs", call: func() (string, error) {
			return h.service.LogsForDeployment("demo", deployment.ID)
		}},
		{name: "runtime logs", call: func() (string, error) {
			return h.service.LogsBySource("demo", deployment.ID, "runtime")
		}},
	} {
		got, err := read.call()
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		for _, fragment := range []string{secret, "journal-secret-last"} {
			if strings.Contains(got, fragment) {
				t.Fatalf("%s leaked a journal secret fragment: %q", read.name, fragment)
			}
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%s omitted the redaction marker", read.name)
		}
	}
}

func repeatedSecretTailFixture(secret string) string {
	const limit = 1 << 20
	totalBytes := limit + len(secret) - 1 + 20
	logs := strings.Repeat(secret, totalBytes/len(secret))
	if padding := totalBytes - len(logs); padding > 0 {
		logs += strings.Repeat("x", padding)
	}
	return logs
}
