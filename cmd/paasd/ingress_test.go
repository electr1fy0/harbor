package main

import (
	"context"
	"net/http"
	"testing"
)

type testDomains map[string]bool

func (d testDomains) IsDomainAllowed(domain string) bool { return d[domain] }

func TestTLSAskListenerPrivateAndOptional(t *testing.T) {
	server, listener, err := listenTLSAsk("", nil)
	if err != nil || server != nil || listener != nil {
		t.Fatalf("disabled listener = %v, %v, %v", server, listener, err)
	}
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:0", "bad"} {
		if _, listener, err := listenTLSAsk(address, nil); err == nil {
			listener.Close()
			t.Fatalf("accepted non-numeric or public address %q", address)
		}
	}
	server, listener, err = listenTLSAsk("127.0.0.1:0", testDomains{"app.example.com": true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		_ = server.Shutdown(context.Background())
		if err := <-done; err != http.ErrServerClosed {
			t.Errorf("serve TLS authorization: %v", err)
		}
	}()
	for path, want := range map[string]int{
		"/tls/ask?domain=app.example.com":   200,
		"/tls/ask?domain=other.example.com": 403,
		"/unknown?domain=app.example.com":   404,
	} {
		response, err := http.Get("http://" + listener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("%s status = %d, want %d", path, response.StatusCode, want)
		}
	}
}
