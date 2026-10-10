package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlatformRejectsMalformedRequests(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, body := range []string{"", "null", `{"unknown":true}`, `{} {}`, `{} garbage`, strings.Repeat(" ", 65<<10)} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("request %q status=%d", body[:min(len(body), 40)], recorder.Code)
		}
	}
}

func TestPlatformMethodsAndPing(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, command := range []string{"deploy", "apps", "inspect", "logs", "stop"} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/"+command, nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "POST" {
			t.Fatalf("%s status=%d", command, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "pong\n" {
		t.Fatalf("ping status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
