package onboarding

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paas/internal/deployment"
)

func TestSignedPushWebhookPinsCommitAndSuppressesDuplicates(t *testing.T) {
	manager, cleanup := newAutomationTestManager(t)
	defer cleanup()
	selection := ConfigSelection{BuildCommand: `go build -o "$PAAS_OUT/server" .`, StartCommand: "/app/server", HealthPath: "/"}
	buildEnvironment := map[string]string{"PRIVATE_TOKEN": "hook-build-secret"}
	if err := manager.UpdateSource(context.Background(), SourceSettings{Name: "api", RepoURL: "https://github.com/acme/mono", Ref: "main", RootDir: "services/api", Selection: selection,
		IncludePaths: []string{"services/api/**"}, IgnorePaths: []string{"services/api/docs/**"}, BuildTimeoutSeconds: 240}, &buildEnvironment); err != nil {
		t.Fatalf("UpdateSource(): %v", err)
	}
	hook, secret, err := manager.CreateHook(context.Background(), "api", "main")
	if err != nil {
		t.Fatalf("CreateHook(): %v", err)
	}
	if hook.ID == "" || secret == "" {
		t.Fatalf("CreateHook() returned empty credentials: %+v", hook)
	}

	sha := strings.Repeat("a", 40)
	payload := map[string]any{
		"ref": "refs/heads/main", "after": sha,
		"repository":  map[string]string{"html_url": "https://github.com/acme/mono", "clone_url": "https://github.com/acme/mono.git"},
		"head_commit": map[string]any{"added": []string{"services/api/new.go"}, "modified": []string{}, "removed": []string{}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	call := func(delivery string, requestBody []byte, signingSecret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/hooks/github/"+hook.ID, bytes.NewReader(requestBody))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", githubTestSignature(signingSecret, requestBody))
		response := httptest.NewRecorder()
		manager.PublicHandler().ServeHTTP(response, req)
		return response
	}

	bad := httptest.NewRequest(http.MethodPost, "/api/hooks/github/"+hook.ID, bytes.NewReader(body))
	bad.Header.Set("X-GitHub-Event", "push")
	bad.Header.Set("X-GitHub-Delivery", "bad-signature")
	bad.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	badResponse := httptest.NewRecorder()
	manager.PublicHandler().ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnauthorized || len(manager.queue) != 0 {
		t.Fatalf("bad signature response = %d, queue=%d", badResponse.Code, len(manager.queue))
	}

	response := call("delivery-1", body, secret)
	if response.Code != http.StatusAccepted {
		t.Fatalf("push response = %d: %s", response.Code, response.Body.String())
	}
	var accepted struct {
		Accepted  bool `json:"accepted"`
		Duplicate bool `json:"duplicate"`
		Job       Job  `json:"job"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.Duplicate || accepted.Job.ID == "" {
		t.Fatalf("push response = %+v", accepted)
	}
	if len(manager.queue) != 1 {
		t.Fatalf("queued jobs = %d, want 1", len(manager.queue))
	}

	state, err := manager.protected.read()
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := state.Jobs[accepted.Job.ID]
	if !ok || saved.Request.Ref != sha || !saved.RequireCommitMatch || !saved.PreserveSettings {
		t.Fatalf("durable webhook request did not pin the commit: %+v", saved)
	}
	stateBytes, err := os.ReadFile(manager.protected.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateBytes, []byte("hook-build-secret")) {
		t.Fatal("protected automation state contains a plaintext build secret")
	}

	duplicateDelivery := call("delivery-1", body, secret)
	if err := json.Unmarshal(duplicateDelivery.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || !accepted.Duplicate || len(manager.queue) != 1 {
		t.Fatalf("delivery duplicate response=%s queued=%d", duplicateDelivery.Body.String(), len(manager.queue))
	}
	duplicateCommit := call("delivery-2", body, secret)
	if err := json.Unmarshal(duplicateCommit.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || !accepted.Duplicate || len(manager.queue) != 1 {
		t.Fatalf("commit duplicate response=%s queued=%d", duplicateCommit.Body.String(), len(manager.queue))
	}

	wrongBranch := map[string]any{"ref": "refs/heads/release", "after": strings.Repeat("b", 40), "repository": payload["repository"], "head_commit": payload["head_commit"]}
	wrongBody, _ := json.Marshal(wrongBranch)
	ignored := call("delivery-3", wrongBody, secret)
	var ignoredResult map[string]any
	_ = json.Unmarshal(ignored.Body.Bytes(), &ignoredResult)
	if ignored.Code != http.StatusAccepted || ignoredResult["accepted"] != false || len(manager.queue) != 1 {
		t.Fatalf("wrong branch response=%s queued=%d", ignored.Body.String(), len(manager.queue))
	}

	// Reopening the encrypted store reconstructs the queued request and result.
	reopened := newProtectedStore(filepath.Dir(manager.protected.path))
	restored, err := reopened.read()
	if err != nil {
		t.Fatalf("read durable state after restart: %v", err)
	}
	recovery := &Manager{protected: reopened, jobs: make(map[string]*jobRecord), dedup: make(map[string]dedupRecord)}
	recovery.restoreJobs(restored)
	recovered, err := recovery.Get(accepted.Job.ID)
	if err != nil || recovered.State != "QUEUED" {
		t.Fatalf("recovered queued job=%+v err=%v", recovered, err)
	}
	if keys, err := manager.ListHooks("api"); err != nil || len(keys) != 1 || keys[0].ID != hook.ID {
		t.Fatalf("ListHooks leaked or lost metadata: %+v err=%v", keys, err)
	}
	if err := manager.RevokeHook(hook.ID); err != nil {
		t.Fatal(err)
	}
	revoked := call("delivery-4", body, secret)
	if revoked.Code != http.StatusNotFound {
		t.Fatalf("revoked hook response=%d", revoked.Code)
	}
}

func TestBuildOptionsReachLifecycleAndSourceSecretsStayEncrypted(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "build-options", `go build -o "$PAAS_OUT/server" .`, "/app/server", "128Mi")
	h.manager.checkout = singleFixtureCheckout(t, fixture)
	request := deployRequest("build-options", "https://github.com/acme/build-options", authoritativeSelection(`go build -o "$PAAS_OUT/server" .`, "/app/server", "128Mi"))
	request.BuildEnvironment = map[string]string{"PRIVATE_TOKEN": "redact-this-token"}
	request.BuildTimeoutSeconds = 45
	request.ClearCache = true
	job, err := h.manager.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	if got := awaitJobTerminal(t, h.manager, job.ID); got.State != "SUCCEEDED" {
		t.Fatalf("deployment result = %+v", got)
	}
	requests := h.builder.Requests()
	if len(requests) != 1 {
		t.Fatalf("builder requests = %d", len(requests))
	}
	got := requests[0]
	if got.BuildTimeout != 45*time.Second || !got.ClearCache || len(got.BuildEnvironment) != 1 || got.BuildEnvironment[0] != "PRIVATE_TOKEN=redact-this-token" {
		t.Fatalf("builder did not receive selected options: %+v", got)
	}
	state, err := h.manager.protected.read()
	if err != nil {
		t.Fatal(err)
	}
	if state.BuildEnvironment[request.Name]["PRIVATE_TOKEN"] != "redact-this-token" {
		t.Fatal("build environment was not persisted for redeploy")
	}
	data, err := os.ReadFile(h.manager.protected.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("redact-this-token")) {
		t.Fatal("persisted build environment is not encrypted")
	}

}

func TestGenericDeployHookUsesSavedBranchBearerAndRevokesOnRotation(t *testing.T) {
	manager, cleanup := newAutomationTestManager(t)
	defer cleanup()
	selection := ConfigSelection{BuildCommand: `go build -o "$PAAS_OUT/server" .`, StartCommand: "/app/server", HealthPath: "/"}
	if err := manager.UpdateSource(context.Background(), SourceSettings{Name: "api", RepoURL: "https://github.com/acme/mono", Ref: "release", RootDir: "services/api", Selection: selection}, nil); err != nil {
		t.Fatalf("UpdateSource(): %v", err)
	}
	if _, _, err := manager.CreateHook(context.Background(), "api", strings.Repeat("a", 40)); err == nil {
		t.Fatal("commit SHA was accepted as a hook branch")
	}
	hook, secret, err := manager.CreateHook(context.Background(), "api", "main")
	if err != nil {
		t.Fatalf("CreateHook(): %v", err)
	}
	endpoint := "/api/hooks/deploy/" + hook.ID
	call := func(token, delivery string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"branch":"attacker-controlled"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if delivery != "" {
			req.Header.Set("Idempotency-Key", delivery)
		}
		response := httptest.NewRecorder()
		manager.PublicHandler().ServeHTTP(response, req)
		return response
	}
	if response := call("", "build-42"); response.Code != http.StatusUnauthorized || len(manager.queue) != 0 {
		t.Fatalf("missing token response=%d queue=%d", response.Code, len(manager.queue))
	}
	queryToken := httptest.NewRequest(http.MethodPost, endpoint+"?token="+secret, nil)
	queryResponse := httptest.NewRecorder()
	manager.PublicHandler().ServeHTTP(queryResponse, queryToken)
	if queryResponse.Code != http.StatusUnauthorized || len(manager.queue) != 0 {
		t.Fatalf("query-string token was accepted: response=%d queue=%d", queryResponse.Code, len(manager.queue))
	}
	response := call(secret, "build-42")
	if response.Code != http.StatusAccepted {
		t.Fatalf("deploy hook response=%d: %s", response.Code, response.Body.String())
	}
	var accepted struct {
		Accepted  bool `json:"accepted"`
		Duplicate bool `json:"duplicate"`
		Job       Job  `json:"job"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.Duplicate || accepted.Job.ID == "" {
		t.Fatalf("deploy hook response=%+v", accepted)
	}
	saved, ok := manager.jobs[accepted.Job.ID]
	if !ok || saved.request.Ref != "main" || !saved.request.preserveSettings {
		t.Fatalf("deploy hook did not use its saved branch: %+v", saved)
	}
	duplicate := call(secret, "build-42")
	if err := json.Unmarshal(duplicate.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if duplicate.Code != http.StatusAccepted || !accepted.Duplicate || len(manager.queue) != 1 {
		t.Fatalf("duplicate response=%s queue=%d", duplicate.Body.String(), len(manager.queue))
	}
	_, replacement, err := manager.RegenerateHook(hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response := call(secret, "old-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("old secret response after rotation=%d", response.Code)
	}
	if response := call(replacement, "build-43"); response.Code != http.StatusAccepted {
		t.Fatalf("rotated secret response=%d: %s", response.Code, response.Body.String())
	}
	if err := manager.RevokeHook(hook.ID); err != nil {
		t.Fatal(err)
	}
	if response := call(replacement, "build-44"); response.Code != http.StatusNotFound {
		t.Fatalf("revoked hook response=%d", response.Code)
	}
}

func newAutomationTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{service: &deployment.Service{}, ctx: ctx, cancel: cancel, queue: make(chan string, maxOutstandingJobs), jobs: make(map[string]*jobRecord),
		order: []string{}, sources: newSourceStore(dataDir), protected: newProtectedStore(dataDir), dedup: make(map[string]dedupRecord), forgotten: make(map[string]bool), buildUser: "test-builder"}
	if _, err := m.protected.read(); err != nil {
		t.Fatalf("initialize test automation store: %v", err)
	}
	return m, cancel
}

func githubTestSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
