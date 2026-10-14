package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicHooksKeepHostBoundaryAndDashboardAuth(t *testing.T) {
	called := false
	hooks := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(202) })
	h, err := NewHandlerWithHooks(http.NotFoundHandler(), testAssets(), testConfig(ModePublic), hooks)
	if err != nil {
		t.Fatal(err)
	}
	w := request(h, "POST", "https://dashboard.example.test/api/hooks/deploy/hook_test", "dashboard.example.test", "198.51.100.1:55", nil, "{}")
	if w.Code != 202 || !called {
		t.Fatalf("hook=%d called=%v", w.Code, called)
	}
	called = false
	w = request(h, "POST", "https://evil.example/api/hooks/deploy/hook_test", "evil.example", "198.51.100.1:55", nil, "{}")
	if w.Code != 403 || called {
		t.Fatal("hook bypassed trusted host")
	}
	w = request(h, "POST", "https://dashboard.example.test/api/dashboard/observe", "dashboard.example.test", "198.51.100.1:55", map[string]string{"Origin": "https://dashboard.example.test"}, "{}")
	if w.Code != 401 {
		t.Fatal("feature bypassed session authentication")
	}
}
func TestFeatureGatewayStrictHandlersAndDeleteOrdering(t *testing.T) {
	calls := []string{}
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/observe" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"Invalid settings."}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	api := NewAPI(control)
	invoke := func(name, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("POST", "/api/dashboard/"+name, strings.NewReader(body)))
		return w
	}
	w := invoke("observe", `{"action":"configure"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Invalid settings.") {
		t.Fatal("handler error not preserved")
	}
	calls = nil
	w = invoke("operations", `{"action":"delete","appName":"demo","preserveData":true}`)
	if w.Code != 200 || len(calls) != 2 || calls[0] != "/automation" || calls[1] != "/operations" {
		t.Fatalf("delete calls=%v code=%d", calls, w.Code)
	}
	calls = nil
	w = invoke("operations", `{"action":"delete","appName":"demo"}`)
	if w.Code != 400 || len(calls) != 0 {
		t.Fatal("delete lacked explicit data choice")
	}
}
