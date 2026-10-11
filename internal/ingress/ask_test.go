package ingress

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type authorizer map[string]bool

func (a authorizer) IsDomainAllowed(host string) bool { return a[host] }

func askRequest(handler http.Handler, remote, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestAskHandlerAllowsOnlyConfiguredDomainsFromLoopback(t *testing.T) {
	handler := NewAskHandler(authorizer{"app.example.com": true})

	allowed := askRequest(handler, "127.0.0.1:44123", "/tls/ask?domain=APP.Example.com")
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed request status = %d, want %d", allowed.Code, http.StatusOK)
	}

	denied := askRequest(handler, "127.0.0.1:44123", "/tls/ask?domain=other.example.com")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unconfigured domain status = %d, want %d", denied.Code, http.StatusForbidden)
	}

	remote := askRequest(handler, "203.0.113.5:44123", "/tls/ask?domain=app.example.com")
	if remote.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status = %d, want %d", remote.Code, http.StatusForbidden)
	}
}

func TestAskHandlerRejectsAmbiguousAndInvalidDomains(t *testing.T) {
	handler := NewAskHandler(authorizer{"app.example.com": true})
	for _, test := range []struct {
		target string
		status int
	}{
		{target: "/tls/ask", status: http.StatusBadRequest},
		{target: "/tls/ask?domain=app.example.com&domain=other.example.com", status: http.StatusBadRequest},
		{target: "/tls/ask?domain=app.example.com&other=value", status: http.StatusBadRequest},
		{target: "/tls/ask?domain=127.0.0.1", status: http.StatusForbidden},
		{target: "/tls/ask?domain=*.example.com", status: http.StatusForbidden},
		{target: "/tls/ask?domain=app.example.com:443", status: http.StatusForbidden},
		{target: "/tls/ask?domain=app%2fexample.com", status: http.StatusForbidden},
	} {
		response := askRequest(handler, "[::1]:44123", test.target)
		if response.Code != test.status {
			t.Errorf("target %q status = %d, want %d", test.target, response.Code, test.status)
		}
	}
}

func TestAskHandlerAllowsIPv6LoopbackAndRejectsOtherMethods(t *testing.T) {
	handler := NewAskHandler(authorizer{"app.example.com": true})
	allowed := askRequest(handler, "[::1]:44123", "/tls/ask?domain=app.example.com")
	if allowed.Code != http.StatusOK {
		t.Fatalf("IPv6 loopback status = %d, want %d", allowed.Code, http.StatusOK)
	}

	r := httptest.NewRequest(http.MethodPost, "/tls/ask?domain=app.example.com", nil)
	r.RemoteAddr = "127.0.0.1:44123"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
