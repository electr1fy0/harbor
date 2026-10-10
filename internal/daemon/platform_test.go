package daemon

import (
	"bytes"
	"errors"
	"log"
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

type failedResponseWriter struct {
	*httptest.ResponseRecorder
}

func (failedResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestPlatformLogsResponseWriteFailure(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	server := NewPlatformServer("", nil)
	writer := failedResponseWriter{httptest.NewRecorder()}
	server.http.Handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/rollback", nil))
	if !strings.Contains(output.String(), "write rollback response: client disconnected") {
		t.Fatalf("response write failure lost: %q", output.String())
	}
}

func TestPlatformMethodsAndPing(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, command := range []string{"deploy", "apps", "inspect", "logs", "stop", "add", "link", "services"} {
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
