package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type explainRoundTripper func(*http.Request) (*http.Response, error)

func (fn explainRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type explainDiagnosticsFixture struct {
	mu         sync.Mutex
	name       string
	id         string
	state      string
	production bool
	excerpt    string
	summary    string
	callNotify chan struct{}
	calls      atomic.Int32
}

func (fixture *explainDiagnosticsFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/diagnostics" {
		http.NotFound(w, r)
		return
	}
	fixture.calls.Add(1)
	if fixture.callNotify != nil {
		select {
		case fixture.callNotify <- struct{}{}:
		default:
		}
	}
	var request struct {
		Name string `json:"Name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid input", http.StatusBadRequest)
		return
	}
	fixture.mu.Lock()
	name, id, state, excerpt, summary, production := fixture.name, fixture.id, fixture.state, fixture.excerpt, fixture.summary, fixture.production
	fixture.mu.Unlock()
	if request.Name != name {
		http.Error(w, "application not found", http.StatusNotFound)
		return
	}
	latest := map[string]any{
		"ID": id, "AppName": name, "State": state, "CreatedAt": "2026-10-14T10:00:00Z",
		"Port": 8080, "HealthPath": "/ready", "ReadinessKnown": true, "MemoryLimit": "256Mi", "CPUQuota": 1.5, "TasksLimit": 64,
		"Error": "/srv/private/raw error password=private-error", "LogPath": "/var/lib/paas/private.log",
		"SystemdUnit": "private.service", "PID": 42, "SandboxID": "private-sandbox",
	}
	var active any
	if production {
		active = map[string]any{"ID": "dep_previous", "AppName": name, "State": "RUNNING"}
	}
	dashboardJSON(w, http.StatusOK, map[string]any{"diagnostics": map[string]any{
		"production": active, "latestAttempt": latest, "summary": summary, "excerpt": excerpt,
		"actions": []any{},
	}})
}

func explainDiagnostics(name, id, state string, production bool) *explainDiagnosticsFixture {
	return &explainDiagnosticsFixture{
		name: name, id: id, state: state, production: production,
		excerpt: "compiler failed: undefined symbol\ngo build ./private/source\npassword=hunter2 /Users/ayush/private.txt ghp_12345678901234567890 upstream-api-secret\n",
		summary: "The latest build attempt failed; review the bounded log excerpt for build output.",
	}
}

func explainResponse(status, structured string) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"id": "resp_test", "object": "response", "created_at": 1, "model": failureExplanationModel,
		"status": status,
		"output": []any{map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": structured, "annotations": []any{}}},
		}},
	})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}

func explanationJSON(explanation string, evidenceIDs []string, nextStep string) string {
	body, _ := json.Marshal(map[string]any{"explanation": explanation, "evidenceIDs": evidenceIDs, "nextStep": nextStep})
	return string(body)
}

func explainAPIRequest(api http.Handler, name, id string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"name": name, "deploymentID": id})
	return dashboardRequest(api, http.MethodPost, "explainFailure", string(body))
}

func newExplainTestAPI(control http.Handler, apiKey string, transport http.RoundTripper) http.Handler {
	client := &http.Client{Transport: transport}
	return newAPI(control, newFailureExplainer(apiKey, client))
}

func TestExplainFailureUsesFreshRedactedEvidenceAndCachesByFingerprint(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_current", "BUILD_FAILED", true)
	var providerCalls atomic.Int32
	var firstProviderRequest map[string]any
	transport := explainRoundTripper(func(request *http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		if request.URL.String() != failureExplanationEndpoint || request.Method != http.MethodPost {
			t.Errorf("request target = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer upstream-api-secret" {
			t.Errorf("Authorization header was not set for the OpenAI request")
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", request.Header.Get("Content-Type"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode OpenAI request: %v", err)
			return nil, err
		}
		firstProviderRequest = payload
		inputs := payload["input"].([]any)
		userMessage := inputs[1].(map[string]any)
		content := userMessage["content"].([]any)[0].(map[string]any)
		promptText := content["text"].(string)
		for _, forbidden := range []string{"hunter2", "/Users/ayush", "ghp_12345678901234567890", "upstream-api-secret", "/var/lib/paas", "private-sandbox", "private.service", "raw error"} {
			if strings.Contains(promptText, forbidden) {
				t.Errorf("untrusted or private diagnostic value %q reached OpenAI", forbidden)
			}
		}
		return explainResponse("completed", explanationJSON("The build failed while resolving a symbol.", []string{"E01", "E03"}, "Review the compiler output and correct the unresolved reference.")), nil
	})
	api := newExplainTestAPI(fixture, "upstream-api-secret", transport)

	first := explainAPIRequest(api, "demo", "dep_current")
	if first.Code != http.StatusOK {
		t.Fatalf("first call status=%d body=%s", first.Code, first.Body)
	}
	firstResult := decodeDashboardResponse(t, first.Body.Bytes())
	if firstResult["deploymentID"] != "dep_current" || firstResult["model"] != failureExplanationModel || firstResult["cached"] != false {
		t.Fatalf("first result = %#v", firstResult)
	}
	if firstResult["explanation"] != "The build failed while resolving a symbol." || firstResult["nextStep"] != "Review the compiler output and correct the unresolved reference." {
		t.Fatalf("unexpected generated explanation: %#v", firstResult)
	}
	evidence, ok := firstResult["evidence"].([]any)
	if !ok || len(evidence) != 2 || evidence[0] != "The latest deployment attempt ended in state BUILD_FAILED." || !strings.Contains(evidence[1].(string), "diagnostic summary") {
		t.Fatalf("model-selected evidence was not projected from server evidence: %#v", firstResult["evidence"])
	}
	for _, forbidden := range []string{"upstream-api-secret", "/Users/", "hunter2", "ghp_", "/var/lib/paas"} {
		if strings.Contains(first.Body.String(), forbidden) {
			t.Fatalf("response exposed private value %q: %s", forbidden, first.Body)
		}
	}
	if fixture.calls.Load() != 1 || providerCalls.Load() != 1 {
		t.Fatalf("diagnostics calls=%d provider calls=%d", fixture.calls.Load(), providerCalls.Load())
	}

	if firstProviderRequest["model"] != failureExplanationModel || firstProviderRequest["store"] != false || firstProviderRequest["max_output_tokens"] != float64(2000) {
		t.Fatalf("unexpected Responses request fields: %#v", firstProviderRequest)
	}
	if firstProviderRequest["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("reasoning effort = %#v", firstProviderRequest["reasoning"])
	}
	format := firstProviderRequest["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true {
		t.Fatalf("response format is not strict JSON schema: %#v", format)
	}
	schema := format["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatalf("schema allows arbitrary properties: %#v", schema)
	}

	second := explainAPIRequest(api, "demo", "dep_current")
	if second.Code != http.StatusOK || decodeDashboardResponse(t, second.Body.Bytes())["cached"] != true {
		t.Fatalf("cache hit status=%d body=%s", second.Code, second.Body)
	}
	if fixture.calls.Load() != 2 || providerCalls.Load() != 1 {
		t.Fatalf("cache hit did not refresh diagnostics: diagnostics=%d provider=%d", fixture.calls.Load(), providerCalls.Load())
	}

	fixture.mu.Lock()
	fixture.excerpt = "compiler failed: unresolved package name\n"
	fixture.mu.Unlock()
	changed := explainAPIRequest(api, "demo", "dep_current")
	if changed.Code != http.StatusOK || decodeDashboardResponse(t, changed.Body.Bytes())["cached"] != false {
		t.Fatalf("changed evidence did not invalidate cache: status=%d body=%s", changed.Code, changed.Body)
	}
	if providerCalls.Load() != 2 {
		t.Fatalf("provider calls after evidence change=%d", providerCalls.Load())
	}
}

func TestExplainFailureRequiresLatestFailedAttemptAndExactInput(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_latest", "BUILD_FAILED", false)
	var providerCalls atomic.Int32
	transport := explainRoundTripper(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return explainResponse("completed", explanationJSON("failed", []string{"E01"}, "review logs")), nil
	})
	api := newExplainTestAPI(fixture, "key", transport)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"stale deployment", `{"name":"demo","deploymentID":"dep_old"}`, http.StatusConflict},
		{"cross app deployment", `{"name":"demo","deploymentID":"dep_other"}`, http.StatusConflict},
		{"latest is running", `{"name":"demo","deploymentID":"dep_latest"}`, http.StatusConflict},
		{"extra field", `{"name":"demo","deploymentID":"dep_latest","extra":true}`, http.StatusBadRequest},
		{"missing id", `{"name":"demo"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.mu.Lock()
			fixture.state = "BUILD_FAILED"
			if tc.name == "latest is running" {
				fixture.state = "RUNNING"
			}
			fixture.mu.Unlock()
			response := dashboardRequest(api, http.MethodPost, "explainFailure", tc.body)
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body, tc.want)
			}
		})
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("invalid or stale attempt reached OpenAI %d times", providerCalls.Load())
	}
}

func TestExplainFailureRequiresLatestAttemptBelongToRequestedApp(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dashboardJSON(w, http.StatusOK, map[string]any{"diagnostics": map[string]any{
			"latestAttempt": map[string]any{"ID": "dep_current", "AppName": "other", "State": "BUILD_FAILED"},
			"summary":       "Build failed.",
		}})
	})
	api := newExplainTestAPI(control, "key", explainRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("mismatched app attempt reached OpenAI")
		return nil, errors.New("unexpected call")
	}))
	response := explainAPIRequest(api, "demo", "dep_current")
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestExplainFailureAcceptsOnlySupportedFailureStates(t *testing.T) {
	for _, state := range []string{"BUILD_FAILED", "START_FAILED", "UNHEALTHY", "CRASHED", "OOM_KILLED"} {
		t.Run(state, func(t *testing.T) {
			fixture := explainDiagnostics("demo", "dep_1", state, false)
			transport := explainRoundTripper(func(*http.Request) (*http.Response, error) {
				return explainResponse("completed", explanationJSON("The attempt failed.", []string{"E01"}, "Inspect its diagnostics.")), nil
			})
			response := explainAPIRequest(newExplainTestAPI(fixture, "key", transport), "demo", "dep_1")
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}

func TestExplainFailureDoesNotAssumeLegacyReadinessConfiguration(t *testing.T) {
	latest := map[string]any{
		"ID": "dep_legacy", "State": "UNHEALTHY", "HealthPath": "/private-health", "ReadinessKnown": false,
	}
	evidence := buildFailureEvidence(latest, map[string]any{"summary": "Readiness settings were not recorded."})
	var hasUnknown, hasHealthPath bool
	for _, item := range evidence {
		hasUnknown = hasUnknown || strings.Contains(item.Text, "configuration is unknown")
		hasHealthPath = hasHealthPath || strings.Contains(item.Text, "/private-health")
	}
	if !hasUnknown || hasHealthPath {
		t.Fatalf("legacy readiness evidence made an unsupported claim: %#v", evidence)
	}
}

func TestExplainFailureDisabledWithoutServerKey(t *testing.T) {
	var diagnosticsCalls atomic.Int32
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		diagnosticsCalls.Add(1)
		dashboardJSON(w, http.StatusOK, map[string]any{})
	})
	api := newExplainTestAPI(control, "", explainRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("OpenAI was called without a key")
		return nil, errors.New("unexpected call")
	}))
	response := explainAPIRequest(api, "demo", "dep_1")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "OPENAI_API_KEY") || diagnosticsCalls.Load() != 0 {
		t.Fatalf("disabled response status=%d diagnostics=%d body=%s", response.Code, diagnosticsCalls.Load(), response.Body)
	}
}

func TestExplainFailurePreservesPackageManagerErrors(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_1", "BUILD_FAILED", false)
	fixture.excerpt = "npm ERR! code ENOENT\nnpm ERR! Missing package.json\nyarn error Couldn't find a package.json file\n"
	transport := explainRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(fixture.excerpt), "\n") {
			if !strings.Contains(string(body), line) {
				t.Errorf("package manager diagnostic %q was omitted from provider evidence", line)
			}
		}
		return explainResponse("completed", explanationJSON("The package manifest may be missing.", []string{"E01"}, "Check the repository root directory.")), nil
	})
	response := explainAPIRequest(newExplainTestAPI(fixture, "key", transport), "demo", "dep_1")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestExplainFailureRejectsMalformedRefusalAndUpstreamErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"refusal", http.StatusOK, `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`},
		{"incomplete", http.StatusOK, `{"status":"incomplete","output":[]}`},
		{"malformed response", http.StatusOK, `not-json`},
		{"invalid evidence id", http.StatusOK, ""},
		{"upstream error", http.StatusInternalServerError, `{"error":{"message":"upstream-api-secret private detail"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := explainDiagnostics("demo", "dep_1", "CRASHED", false)
			transport := explainRoundTripper(func(*http.Request) (*http.Response, error) {
				if tc.name == "invalid evidence id" {
					return explainResponse("completed", explanationJSON("not safe", []string{"E99"}, "retry")), nil
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			response := explainAPIRequest(newExplainTestAPI(fixture, "upstream-api-secret", transport), "demo", "dep_1")
			if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "upstream-api-secret") || strings.Contains(response.Body.String(), "private detail") {
				t.Fatalf("unsafe failure response status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}

func TestExplainFailureTimeoutIsBoundedAndNotCached(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_1", "CRASHED", false)
	var providerCalls atomic.Int32
	transport := explainRoundTripper(func(request *http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := &http.Client{Timeout: 25 * time.Millisecond, Transport: transport}
	api := newAPI(fixture, newFailureExplainer("key", client))
	for i := 0; i < 2; i++ {
		response := explainAPIRequest(api, "demo", "dep_1")
		if response.Code != http.StatusBadGateway {
			t.Fatalf("timeout status=%d body=%s", response.Code, response.Body)
		}
	}
	if providerCalls.Load() != 2 {
		t.Fatalf("failed provider calls were cached: got %d calls", providerCalls.Load())
	}
}

func TestExplainFailureCoalescesDuplicateRequests(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_1", "CRASHED", false)
	fixture.callNotify = make(chan struct{}, 2)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var providerCalls atomic.Int32
	transport := explainRoundTripper(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		started <- struct{}{}
		<-release
		return explainResponse("completed", explanationJSON("runtime exited", []string{"E01"}, "inspect the runtime logs")), nil
	})
	api := newExplainTestAPI(fixture, "key", transport)
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- explainAPIRequest(api, "demo", "dep_1") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}
	select {
	case <-fixture.callNotify:
	case <-time.After(time.Second):
		t.Fatal("first diagnostics call was not recorded")
	}
	go func() { results <- explainAPIRequest(api, "demo", "dep_1") }()
	select {
	case <-fixture.callNotify:
	case <-time.After(time.Second):
		t.Fatal("duplicate request did not refresh diagnostics")
	}
	close(release)
	first, second := <-results, <-results
	if first.Code != http.StatusOK || second.Code != http.StatusOK || providerCalls.Load() != 1 || fixture.calls.Load() != 2 {
		t.Fatalf("coalesced status=(%d,%d), provider=%d diagnostics=%d", first.Code, second.Code, providerCalls.Load(), fixture.calls.Load())
	}
	if !strings.Contains(first.Body.String()+second.Body.String(), "\"cached\":false") || !strings.Contains(first.Body.String()+second.Body.String(), "\"cached\":true") {
		t.Fatalf("expected one fresh and one coalesced response: %s %s", first.Body, second.Body)
	}
}

func TestExplainFailureCapsConcurrentProviderRequests(t *testing.T) {
	started := make(chan struct{}, failureExplanationSlots)
	release := make(chan struct{})
	var providerCalls atomic.Int32
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Name string `json:"Name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		dashboardJSON(w, http.StatusOK, map[string]any{"diagnostics": map[string]any{
			"latestAttempt": map[string]any{"ID": "dep_" + input.Name, "AppName": input.Name, "State": "CRASHED"},
			"summary":       "Runtime exited.", "excerpt": "exit status 1",
		}})
	})
	transport := explainRoundTripper(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		started <- struct{}{}
		<-release
		return explainResponse("completed", explanationJSON("runtime exited", []string{"E01"}, "inspect logs")), nil
	})
	api := newExplainTestAPI(control, "key", transport)
	results := make(chan *httptest.ResponseRecorder, failureExplanationSlots)
	for index := 0; index < failureExplanationSlots; index++ {
		name := "app-" + itoa(index)
		go func() { results <- explainAPIRequest(api, name, "dep_"+name) }()
	}
	for index := 0; index < failureExplanationSlots; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("provider did not start all bounded slots")
		}
	}
	fifth := explainAPIRequest(api, "app-4", "dep_app-4")
	if fifth.Code != http.StatusServiceUnavailable {
		close(release)
		t.Fatalf("fifth simultaneous request status=%d body=%s", fifth.Code, fifth.Body)
	}
	close(release)
	for index := 0; index < failureExplanationSlots; index++ {
		if result := <-results; result.Code != http.StatusOK {
			t.Fatalf("bounded request status=%d body=%s", result.Code, result.Body)
		}
	}
	if providerCalls.Load() != failureExplanationSlots {
		t.Fatalf("provider calls=%d, want %d", providerCalls.Load(), failureExplanationSlots)
	}
}

func TestExplainFailureWaiterCanCancel(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_1", "CRASHED", false)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	transport := explainRoundTripper(func(request *http.Request) (*http.Response, error) {
		started <- struct{}{}
		select {
		case <-release:
			return explainResponse("completed", explanationJSON("runtime exited", []string{"E01"}, "inspect logs")), nil
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	})
	api := newExplainTestAPI(fixture, "key", transport)
	firstDone := make(chan struct{})
	go func() {
		_ = explainAPIRequest(api, "demo", "dep_1")
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, apiPrefix+"explainFailure", strings.NewReader(`{"name":"demo","deploymentID":"dep_1"}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("canceled waiter status=%d body=%s", response.Code, response.Body)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("leader request did not finish")
	}
}

func TestExplainFailureLeaderCancellationDoesNotCancelSharedCall(t *testing.T) {
	fixture := explainDiagnostics("demo", "dep_1", "CRASHED", false)
	fixture.callNotify = make(chan struct{}, 2)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	transport := explainRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return explainResponse("completed", explanationJSON("runtime exited", []string{"E01"}, "inspect logs")), nil
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	})
	api := newExplainTestAPI(fixture, "key", transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, apiPrefix+"explainFailure", strings.NewReader(`{"name":"demo","deploymentID":"dep_1"}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		results <- response
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}
	<-fixture.callNotify
	cancel()
	go func() { results <- explainAPIRequest(api, "demo", "dep_1") }()
	select {
	case <-fixture.callNotify:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shared caller did not refresh diagnostics")
	}
	close(release)
	for index := 0; index < 2; index++ {
		select {
		case result := <-results:
			if result.Code != http.StatusOK {
				t.Fatalf("leader cancellation affected shared result: status=%d body=%s", result.Code, result.Body)
			}
		case <-time.After(time.Second):
			t.Fatal("shared request did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d, want 1", calls.Load())
	}
}
