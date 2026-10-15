package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckHealthyHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	port := portFromAddress(t, server.Listener.Addr())
	if err := Check(context.Background(), port, "/ready"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckUnhealthyHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port := portFromAddress(t, server.Listener.Addr())
	if err := Check(context.Background(), port, "/ready"); err == nil {
		t.Fatal("expected unhealthy status to fail")
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("got %d requests, want %d", got, maxAttempts)
	}
}

func TestCheckTCPReadiness(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := portFromAddress(t, listener.Addr())
	if err := Check(context.Background(), port, ""); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port := portFromAddress(t, server.Listener.Addr())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := Check(ctx, port, "/ready")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
}

func TestCheckRedirectIsHealthyAndNotFollowed(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		foreignRequests.Add(1)
	}))
	defer foreign.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/target", http.StatusFound)
	}))
	defer redirect.Close()
	port := portFromAddress(t, redirect.Listener.Addr())
	if err := Check(context.Background(), port, "/redirect"); err != nil {
		t.Fatal(err)
	}
	if got := foreignRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}

func portFromAddress(t *testing.T, address net.Addr) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCheckRejectsNilContext(t *testing.T) {
	if err := Check(nil, 8080, ""); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestProbeEnforcesMinimumTimeout(t *testing.T) {
	if err := Probe(context.Background(), 8080, "", time.Nanosecond); err == nil || err.Error() != "health probe timeout must be between 1ms and 30s" {
		t.Fatalf("invalid timeout error=%v", err)
	}
}
