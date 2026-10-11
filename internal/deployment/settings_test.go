package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paas/internal/model"
	"paas/internal/store"
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

func TestConfigureValidatesDomainsAndUpdatesLiveAliases(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	if _, err := h.service.Deploy(context.Background(), h.source, "demo"); err != nil {
		t.Fatal(err)
	}
	for _, domains := range [][]string{{"bad/path.example"}, {"same.example", "SAME.EXAMPLE"}, {"localhost"}, {"demo.localhost"}} {
		if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Domains: &domains}); err == nil {
			t.Errorf("Configure(%v) unexpectedly succeeded", domains)
		}
	}
	domains := []string{"WWW.Example.com."}
	settings, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Domains: &domains})
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Domains) != 1 || settings.Domains[0] != "www.example.com" {
		t.Fatalf("normalized domains = %v", settings.Domains)
	}
	if !h.service.IsDomainAllowed("WWW.EXAMPLE.COM:443") || h.service.IsDomainAllowed("demo.localhost") {
		t.Fatal("TLS hostname allow-list does not match explicit domains")
	}
	if response := routerRequest(h.router, "www.example.com", "/"); response.Code != 200 {
		t.Fatalf("custom alias returned %d, want 200", response.Code)
	}

	if _, err := h.service.Deploy(context.Background(), h.source, "other"); err != nil {
		t.Fatal(err)
	}
	duplicate := []string{"WWW.EXAMPLE.COM"}
	if _, err := h.service.Configure(context.Background(), "other", ConfigureOptions{Domains: &duplicate}); !errors.Is(err, store.ErrDomainInUse) {
		t.Fatalf("duplicate global domain error = %v", err)
	}
	cleared := []string{}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Domains: &cleared}); err != nil {
		t.Fatal(err)
	}
	if response := routerRequest(h.router, "www.example.com", "/"); response.Code != 404 {
		t.Fatalf("removed alias returned %d, want 404", response.Code)
	}
	if h.service.IsDomainAllowed("www.example.com") {
		t.Fatal("removed domain remained allowed for TLS")
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

func TestCustomAliasesSurviveCandidateFailureAndWithdrawOnStop(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	domains := []string{"api.example.com", "www.example.com"}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Domains: &domains}); err != nil {
		t.Fatal(err)
	}
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
	for _, host := range append(domains, "demo.localhost") {
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
	for _, host := range append(domains, "demo.localhost") {
		if routes[host] != "" || routerRequest(h.router, host, "/").Code != 404 {
			t.Fatalf("stopped app still has alias %s", host)
		}
	}
}
