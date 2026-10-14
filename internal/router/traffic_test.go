package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrafficPoliciesMetricsAndUnknownHosts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); io.WriteString(w, "hello") }))
	defer upstream.Close()
	router := New()
	if err := router.SetRoute("alias.example.com", upstream.URL); err != nil {
		t.Fatal(err)
	}
	call := func(host, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("GET", "http://"+host+path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, request)
		return w
	}
	router.SetTrafficPolicy("alias.example.com", TrafficPolicy{Maintenance: true, Message: "Back soon"})
	if response := call("alias.example.com", "/"); response.Code != 503 || response.Header().Get("Retry-After") != "60" {
		t.Fatalf("maintenance=%+v", response)
	}
	router.SetTrafficPolicy("alias.example.com", TrafficPolicy{})
	if response := call("alias.example.com", "/"); response.Code != 201 || response.Body.String() != "hello" {
		t.Fatalf("proxy=%+v", response)
	}
	call("unknown.example.com", "/")
	stats := router.Traffic()
	if len(stats) != 1 || stats[0].Requests != 2 || stats[0].StatusCodes[201] != 1 || stats[0].StatusCodes[503] != 1 || stats[0].Bytes < 5 {
		t.Fatalf("stats=%+v", stats)
	}
	stats[0].StatusCodes[201] = 100
	if router.Traffic()[0].StatusCodes[201] != 1 {
		t.Fatal("metrics map aliases internal state")
	}
}
func TestTrafficWriterPreservesResponseController(t *testing.T) {
	response := httptest.NewRecorder()
	writer := &trafficWriter{ResponseWriter: response}
	if err := http.NewResponseController(writer).Flush(); err != nil {
		t.Fatal(err)
	}
	if !response.Flushed || writer.status != 200 {
		t.Fatal("flush not forwarded")
	}
	writer.WriteHeader(500)
	writer.Write([]byte("body"))
	if response.Code != 200 || writer.bytes != 4 {
		t.Fatal("status accounting changed after flush")
	}
}

func TestMaintenanceSurvivesStopAndNewAliases(t *testing.T) {
	r := New()
	r.SetTrafficPolicy("demo.localhost", TrafficPolicy{Maintenance: true, Message: "Maintenance"})
	if _, err := r.SwitchRoutes([]string{"demo.localhost", "demo.example.com"}, "http://127.0.0.1:1", nil); err != nil {
		t.Fatal(err)
	}
	assertMaint := func(host string) {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"/", nil))
		if w.Code != 503 {
			t.Fatalf("host %s status=%d", host, w.Code)
		}
	}
	assertMaint("demo.example.com")
	if err := r.RemoveRoutes([]string{"demo.localhost", "demo.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SwitchRoutes([]string{"demo.localhost", "fresh.example.com"}, "http://127.0.0.1:1", nil); err != nil {
		t.Fatal(err)
	}
	assertMaint("demo.localhost")
	assertMaint("fresh.example.com")
	if err := r.UpdateRoutes([]string{"demo.localhost", "new.example.com"}, []string{"demo.localhost", "fresh.example.com"}, "http://127.0.0.1:1", nil); err != nil {
		t.Fatal(err)
	}
	assertMaint("new.example.com")
	if _, err := r.SwitchRoutes([]string{"other.localhost", "fresh.example.com"}, "http://127.0.0.1:1", nil); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "http://fresh.example.com/", nil))
	if w.Code == 503 {
		t.Fatal("reassigned alias inherited previous app policy")
	}
}

func TestPolicyRefreshDoesNotClearReassignedAlias(t *testing.T) {
	r := New()
	r.SetTrafficPolicy("old.localhost", TrafficPolicy{Maintenance: true, Message: "Old"})
	r.SwitchRoutes([]string{"old.localhost", "alias.example.com"}, "http://127.0.0.1:1", nil)
	r.SetTrafficPolicy("new.localhost", TrafficPolicy{Maintenance: true, Message: "New"})
	r.SwitchRoutes([]string{"new.localhost", "alias.example.com"}, "http://127.0.0.1:2", nil)
	r.UpdateTrafficPolicies("old.localhost", []string{"old.localhost"}, []string{"old.localhost", "alias.example.com"}, TrafficPolicy{Maintenance: true, Message: "Old"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "http://alias.example.com/", nil))
	if w.Code != 503 || w.Body.String() != "New\n" {
		t.Fatalf("reassigned alias policy=%d %s", w.Code, w.Body.String())
	}
}
