package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentRequestEnvelopeOversizeIsRejectedBeforeControl(t *testing.T) {
	for _, tc := range []struct {
		name, function string
		request        map[string]any
	}{
		{
			name:     "configure app",
			function: "configureApp",
			request: map[string]any{
				"name":        "demo",
				"environment": map[string]string{"TOKEN": strings.Repeat("x", 65_500)},
			},
		},
		{
			name:     "submit onboarding job",
			function: "submitOnboardingJob",
			request: map[string]any{
				"name":        "demo",
				"repoURL":     "https://github.com/octo/demo",
				"environment": map[string]string{"TOKEN": strings.Repeat("x", 65_500)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("oversized control request reached daemon")
			})
			body, err := json.Marshal(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			response := dashboardRequest(NewAPI(control), http.MethodPost, tc.function, string(body))
			if response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status=%d want=%d body=%s", response.Code, http.StatusRequestEntityTooLarge, response.Body)
			}
		})
	}
}

func TestSynchronousDeployAndRollbackUseCallerCancellation(t *testing.T) {
	for _, tc := range []struct{ name, function, body string }{
		{"deploy", "deployApp", `{"name":"demo","repoURL":"https://github.com/octo/demo"}`},
		{"direct rollback", "rollbackApp", `{"name":"demo","releaseID":"rel_old"}`},
		{"operations rollback", "operations", `{"action":"rollback","appName":"demo","releaseId":"rel_old"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan context.Context, 1)
			control := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				observed <- r.Context()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, apiPrefix+tc.function, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				NewAPI(control).ServeHTTP(response, request)
				close(done)
			}()

			var controlContext context.Context
			select {
			case controlContext = <-observed:
			case <-time.After(time.Second):
				t.Fatal("control request did not start")
			}
			if _, hasDeadline := controlContext.Deadline(); hasDeadline {
				cancel()
				<-done
				t.Fatal("synchronous deployment control request has a fixed deadline")
			}
			cancel()
			select {
			case <-controlContext.Done():
			case <-time.After(time.Second):
				t.Fatal("caller cancellation did not reach the daemon request")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("dashboard request did not finish after caller cancellation")
			}
		})
	}
}

func TestValidGitHubRepoURLAcceptsEquivalentHTTPSAuthority(t *testing.T) {
	for _, value := range []string{
		"https://github.com/owner/repo",
		"HTTPS://GITHUB.COM/owner/repo",
		"https://github.com:443/owner/repo.git",
	} {
		if _, _, ok := validGitHubRepoURL(value); !ok {
			t.Errorf("validGitHubRepoURL(%q) rejected an equivalent GitHub URL", value)
		}
	}
	for _, value := range []string{
		"https://github.com.evil.test/owner/repo",
		"https://github.com:444/owner/repo",
		"https://github.com/owner/%72epo",
		"https://github.com/owner/re%70o",
		"https://user:password@github.com/owner/repo",
		"https://github.com/owner/repo?tab=readme",
		"https://github.com/owner/repo#readme",
	} {
		if _, _, ok := validGitHubRepoURL(value); ok {
			t.Errorf("validGitHubRepoURL(%q) accepted an untrusted or non-canonical URL", value)
		}
	}
}

func TestGitHubRepositoryValidationAcceptsLeadingDotNames(t *testing.T) {
	for _, value := range []string{
		"https://github.com/owner/.github",
		"https://github.com/owner/.github.git",
	} {
		if _, _, ok := validGitHubRepoURL(value); !ok {
			t.Errorf("validGitHubRepoURL(%q) rejected a repository name accepted by the source checkout validator", value)
		}
	}
	if _, ok := mapGitHubRepo(map[string]any{"fullName": "owner/.github", "name": ".github"}); !ok {
		t.Fatal("mapGitHubRepo rejected a leading-dot repository returned by GitHub")
	}
	for _, value := range []string{
		"https://github.com/owner/.",
		"https://github.com/owner/..",
		"https://github.com/owner/.git",
	} {
		if _, _, ok := validGitHubRepoURL(value); ok {
			t.Errorf("validGitHubRepoURL(%q) accepted an empty or traversal-like repository name", value)
		}
	}
}

func TestAPIStrictJSONKeysAndNullRollbackNeverReachControl(t *testing.T) {
	control := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input reached the control API") })
	api := NewAPI(control)
	for _, tc := range []struct{ name, operation, body string }{
		{"removed domains", "configureApp", `{"name":"demo","domains":["example.com"],"memoryLimit":"128Mi"}`},
		{"null release", "rollbackApp", `{"name":"demo","releaseID":null}`},
		{"case folded name", "getApp", `{"Name":"demo"}`},
		{"case folded map", "configureApp", `{"name":"demo","Environment":{"TOKEN":null},"memoryLimit":"128Mi"}`},
		{"case folded option", "configureApp", `{"name":"demo","CpuQuota":null,"memoryLimit":"128Mi"}`},
		{"nested case folded config", "inspectProject", `{"repoURL":"https://github.com/owner/repo","config":{"Runtime":"go"}}`},
		{"case folded environment submission", "submitOnboardingJob", `{"name":"demo","repoURL":"https://github.com/owner/repo","Environment":{"TOKEN":null}}`},
		{"duplicate root field", "getApp", `{"name":"demo","name":"other"}`},
		{"duplicate nested environment merge", "configureApp", `{"name":"demo","environment":{"TOKEN":null},"environment":{"SAFE":"ok"}}`},
		{"duplicate environment key", "configureApp", `{"name":"demo","environment":{"TOKEN":"first","TOKEN":"second"}}`},
		{"duplicate nested config merge", "inspectProject", `{"repoURL":"https://github.com/owner/repo","config":{"runtime":null},"config":{"startCommand":"run"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := dashboardRequest(api, http.MethodPost, tc.operation, tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}

func TestAPIInspectProjectUsesCheckoutReferenceRules(t *testing.T) {
	var gotRef string
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode project inspection: %v", err)
		}
		gotRef, _ = input["ref"].(string)
		dashboardJSON(w, http.StatusOK, map[string]any{"inspection": map[string]any{"config": map[string]any{"runtime": "go"}}})
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "inspectProject", `{"repoURL":"https://github.com/octo/demo","ref":"修复/naïve"}`)
	if response.Code != http.StatusOK || gotRef != "修复/naïve" {
		t.Fatalf("Unicode ref status=%d forwarded=%q body=%s", response.Code, gotRef, response.Body)
	}
	for _, body := range []string{
		`{"repoURL":"https://github.com/octo/demo","ref":"bad^ref"}`,
		`{"repoURL":"https://github.com/octo/demo","rootDir":".git"}`,
	} {
		t.Run(strings.TrimSpace(body), func(t *testing.T) {
			invalidControl := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid checkout request reached control API") })
			got := dashboardRequest(NewAPI(invalidControl), http.MethodPost, "inspectProject", body)
			if got.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", got.Code, got.Body)
			}
		})
	}
}
