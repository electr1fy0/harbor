package router

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyRebuildsForwardedIdentity(t *testing.T) {
	var forwarded http.Header
	var host string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Clone()
		host = r.Host
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	router := New()
	if err := router.SetRoute("demo.localhost", upstream.URL); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://demo.localhost/", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("X-Forwarded-For", "attacker")
	request.Header.Set("X-Forwarded-Host", "attacker.example")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Forwarded", "for=attacker")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "ok" {
		t.Fatalf("response %d: %s", response.Code, response.Body)
	}
	if forwarded.Get("X-Forwarded-For") != "192.0.2.1" || forwarded.Get("X-Forwarded-Host") != "demo.localhost" || forwarded.Get("X-Forwarded-Proto") != "http" || forwarded.Get("Forwarded") != "" {
		t.Fatalf("untrusted forwarding headers: %v", forwarded)
	}
	if host != "demo.localhost" {
		t.Fatalf("virtual host changed to %q", host)
	}
}

func TestInvalidUpstreamDoesNotCommitOrReplaceRoute(t *testing.T) {
	for _, target := range []string{"http://:80", "http://localhost:0", "http://localhost:65536"} {
		for _, operation := range []string{"switch", "update"} {
			t.Run(operation+target, func(t *testing.T) {
				router := New()
				if err := router.SetRoute("demo.localhost", "http://127.0.0.1:8000"); err != nil {
					t.Fatal(err)
				}
				committed := false
				commit := func() error { committed = true; return nil }
				var err error
				if operation == "switch" {
					_, err = router.SwitchRoute("demo.localhost", target, commit)
				} else {
					err = router.UpdateRoutes([]string{"demo.localhost"}, nil, target, commit)
				}
				if err == nil || committed || router.routes["demo.localhost"].upstream != "http://127.0.0.1:8000" {
					t.Fatalf("invalid target committed: %v %v", err, committed)
				}
			})
		}
	}
}

func TestAbsoluteDNSHostRoutes(t *testing.T) {
	router := New()
	router.SetTrafficPolicy("demo.localhost", TrafficPolicy{Maintenance: true})
	if err := router.SetRoute("demo.localhost", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://demo.localhost/", nil)
	request.Host = "DEMO.LOCALHOST.:8080"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatalf("absolute DNS host status=%d", response.Code)
	}
	router.Delete("demo.localhost.")
	if len(router.routes) != 0 {
		t.Fatal("delete failed to normalize absolute DNS name")
	}
}

func TestTrafficWriterRecordsUpgradeAsFinalStatus(t *testing.T) {
	writer := &trafficWriter{ResponseWriter: httptest.NewRecorder()}
	writer.WriteHeader(http.StatusSwitchingProtocols)
	if writer.status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status=%d", writer.status)
	}
	writer.WriteHeader(500)
	if writer.status != http.StatusSwitchingProtocols {
		t.Fatal("upgrade status overwritten")
	}
}

type failingFlusher struct {
	*httptest.ResponseRecorder
	err error
}

func (w *failingFlusher) FlushError() error { return w.err }
func TestTrafficWriterPropagatesFlushErrors(t *testing.T) {
	want := errors.New("disconnected")
	underlying := &failingFlusher{httptest.NewRecorder(), want}
	writer := &trafficWriter{ResponseWriter: underlying}
	if err := http.NewResponseController(writer).Flush(); !errors.Is(err, want) {
		t.Fatalf("flush error=%v", err)
	}
}
