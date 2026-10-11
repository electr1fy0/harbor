package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"paas/internal/model"
	"paas/internal/store"
)

func TestDiagnosticsSeparateHealthyProductionFromFailedCandidate(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	production, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	firstSecret := "first-runtime-secret"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": firstSecret}}); err != nil {
		t.Fatal(err)
	}
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compiler failed")})
	candidate, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil || candidate.State != model.BuildFailed {
		t.Fatalf("candidate = %+v, error = %v; want BUILD_FAILED", candidate, err)
	}
	if err := os.WriteFile(candidate.LogPath, []byte(strings.Repeat("build detail ", 500)+"\ncompiler output\nsecret="+firstSecret), 0600); err != nil {
		t.Fatal(err)
	}
	secondSecret := "rotated-runtime-secret"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": secondSecret}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{RemoveEnvironment: []string{"TOKEN"}}); err != nil {
		t.Fatal(err)
	}

	diagnostics, err := h.service.Diagnostics("demo", false)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Production == nil || diagnostics.Production.ID != production.ID || diagnostics.Production.State != model.Running {
		t.Fatalf("production = %+v, want original healthy deployment %s", diagnostics.Production, production.ID)
	}
	if diagnostics.LatestAttempt == nil || diagnostics.LatestAttempt.ID != candidate.ID || diagnostics.LatestAttempt.State != model.BuildFailed {
		t.Fatalf("latest attempt = %+v, want failed candidate %s", diagnostics.LatestAttempt, candidate.ID)
	}
	if !strings.Contains(diagnostics.Summary, "Production remains healthy") || !strings.Contains(diagnostics.Summary, "build attempt failed") {
		t.Fatalf("summary = %q, want production and failed attempt details", diagnostics.Summary)
	}
	if !strings.Contains(diagnostics.Excerpt, "compiler output") || strings.Contains(diagnostics.Excerpt, firstSecret) || strings.Contains(diagnostics.Excerpt, secondSecret) {
		t.Fatalf("diagnostic excerpt is missing useful output or leaked a rotated secret: %q", diagnostics.Excerpt)
	}
	if len([]rune(diagnostics.Excerpt)) > diagnosticExcerptRunes {
		t.Fatalf("diagnostic excerpt has %d runes, limit is %d", len([]rune(diagnostics.Excerpt)), diagnosticExcerptRunes)
	}
	if action := diagnosticAction(diagnostics, "rollback_release"); action.Available {
		t.Fatal("diagnostics offered rollback while healthy production remains active")
	}
	if action := diagnosticAction(diagnostics, "retry_github_deploy"); action.Available || action.Reason == "" {
		t.Fatalf("retry action should explain that no persisted GitHub source is available: %+v", action)
	}

	logs, err := h.service.LogsForDeployment("demo", candidate.ID)
	if err != nil || strings.Contains(logs, firstSecret) || strings.Contains(logs, secondSecret) {
		t.Fatalf("attempt logs were unavailable or leaked a rotated secret: err=%v logs=%q", err, logs)
	}
	foreign, err := h.service.Deploy(context.Background(), h.source, "foreign")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.LogsForDeployment("demo", foreign.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-application deployment logs error = %v, want not found", err)
	}

	h.builder.setBehavior("demo", nil)
	deployed, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err = h.service.Diagnostics("demo", false)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Production == nil || diagnostics.LatestAttempt == nil || diagnostics.Production.ID != deployed.ID || diagnostics.LatestAttempt.ID != deployed.ID {
		t.Fatalf("successful deployment did not become production and latest attempt: %+v", diagnostics)
	}
	if strings.Contains(diagnostics.Summary, "failed") || !strings.Contains(diagnostics.Summary, "healthy") {
		t.Fatalf("later successful attempt did not clear failure attention: %q", diagnostics.Summary)
	}
}

func TestDiagnosticsReportRuntimeExitWithoutInventingEnforcement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		oom   bool
		state model.State
	}{
		{name: "crash", state: model.Crashed},
		{name: "oom", oom: true, state: model.OOMKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServiceHarness(t, testConfig)
			deployed, err := h.service.Deploy(context.Background(), h.source, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if tc.oom {
				h.sandbox.markOOM(deployed.ID)
			} else {
				h.sandbox.markExited(deployed.ID, 1)
			}
			waitFor(t, "runtime failure state", func() bool {
				latest, err := h.db.LatestDeployment("demo")
				return err == nil && latest.State == tc.state && latest.StoppedAt != nil
			})
			diagnostics, err := h.service.Diagnostics("demo", false)
			if err != nil {
				t.Fatal(err)
			}
			if diagnostics.Production != nil || diagnostics.LatestAttempt == nil || diagnostics.LatestAttempt.State != tc.state {
				t.Fatalf("diagnostics production=%+v latest=%+v, want no route and %s attempt", diagnostics.Production, diagnostics.LatestAttempt, tc.state)
			}
			if tc.oom && (!strings.Contains(diagnostics.Summary, "does not enforce") || strings.Contains(diagnostics.Summary, "recorded enforced memory ceiling")) {
				t.Fatalf("host backend OOM summary claimed unsupported enforcement: %q", diagnostics.Summary)
			}
			if !tc.oom && !strings.Contains(diagnostics.Summary, "Inspect runtime logs") {
				t.Fatalf("crash summary did not direct the user to runtime logs: %q", diagnostics.Summary)
			}
		})
	}
}

func TestDiagnosticsOfferOnlyTheLastGoodReleaseForRollback(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	changedConfig := strings.Replace(testConfig, "fake-server", "fake-server-v2", 1)
	if err := os.WriteFile(filepath.Join(h.source, "paas.yaml"), []byte(changedConfig), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if first.ReleaseID == second.ReleaseID {
		t.Fatalf("test requires different immutable releases, got %q for both deployments", first.ReleaseID)
	}
	h.sandbox.markExited(second.ID, 1)
	waitFor(t, "second deployment crash", func() bool {
		latest, inspectErr := h.db.LatestDeployment("demo")
		return inspectErr == nil && latest.ID == second.ID && latest.State == model.Crashed
	})

	diagnostics, err := h.service.Diagnostics("demo", false)
	if err != nil {
		t.Fatal(err)
	}
	action := diagnosticAction(diagnostics, "rollback_release")
	if !action.Available || action.ReleaseID != first.ReleaseID {
		t.Fatalf("rollback action = %+v, want previous promoted release %q", action, first.ReleaseID)
	}
}

func TestDiagnosticStateSummariesAreSpecificAndEvidenceBased(t *testing.T) {
	for _, tc := range []struct {
		state model.State
		want  string
	}{
		{model.BuildFailed, "bounded log excerpt"},
		{model.StartFailed, "configured start command"},
		{model.Unhealthy, "injected PORT"},
		{model.Crashed, "runtime logs"},
		{model.OOMKilled, "does not enforce"},
	} {
		deployment := &model.Deployment{State: tc.state, Error: "untrusted root cause"}
		got := attemptSummary(deployment, true)
		if !strings.Contains(got, tc.want) || strings.Contains(got, deployment.Error) {
			t.Errorf("attemptSummary(%s) = %q, want %q and no unsupported error", tc.state, got, tc.want)
		}
	}
	if got := attemptSummary(&model.Deployment{State: model.OOMKilled, SandboxType: "systemd", MemoryLimit: "128Mi"}, true); !strings.Contains(got, "recorded enforced memory ceiling of 128Mi") || !strings.Contains(got, "Increase memory, then redeploy") {
		t.Fatalf("enforced OOM summary = %q", got)
	}
	if got := attemptSummary(&model.Deployment{State: model.Stopped}, false); !strings.Contains(got, "daemon restarts will keep it stopped") {
		t.Fatalf("stopped summary = %q", got)
	}
}

func TestDiagnosticsFailClosedWhenLogSecretsCannotBeRedacted(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	secret := "diagnostics-secret-value"
	if _, err := h.service.Configure(context.Background(), "demo", ConfigureOptions{Environment: map[string]string{"TOKEN": secret}}); err != nil {
		t.Fatal(err)
	}
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compiler failed")})
	failed, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil || failed.State != model.BuildFailed {
		t.Fatalf("candidate = %+v, %v; want BUILD_FAILED", failed, err)
	}
	if err := os.WriteFile(failed.LogPath, []byte("private output: "+secret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.dbPath), "data", "environment.key"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := h.service.Diagnostics("demo", false)
	if err == nil || diagnostics != nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("diagnostics returned after redaction failure: result=%+v error=%v", diagnostics, err)
	}
}

func diagnosticAction(diagnostics *model.Diagnostics, id string) model.DiagnosticAction {
	for _, action := range diagnostics.Actions {
		if action.ID == id {
			return action
		}
	}
	return model.DiagnosticAction{ID: id, Reason: "missing action"}
}

func TestDiagnosticExcerptIsBoundedAtUTF8RuneBoundary(t *testing.T) {
	input := strings.Repeat("界", diagnosticExcerptRunes+100)
	got := boundedExcerpt(input)
	if len([]rune(got)) != diagnosticExcerptRunes || !strings.HasPrefix(got, "界") {
		t.Fatalf("bounded excerpt length/prefix = %d/%q", len([]rune(got)), got[:3])
	}
}
