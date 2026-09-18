package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPingOverUnixSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "paasd.sock")
	server := NewServer(socketPath)
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	result, err := Ping(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if result != "pong" {
		t.Fatalf("ping = %q, want pong", result)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket still exists or stat failed: %v", err)
	}
}

func TestServerDoesNotOverwriteRegularFile(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "paasd.sock")
	if err := os.WriteFile(socketPath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(socketPath)
	if _, err := server.Listen(); err == nil {
		t.Fatal("Listen unexpectedly overwrote a regular file")
	}
	body, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep me" {
		t.Fatalf("regular file changed to %q", body)
	}
}

func TestPingMethodNotAllowed(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "paasd.sock")
	server := NewServer(socketPath)
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	dialer := &net.Dialer{}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socketPath)
	}}
	client := &http.Client{Transport: transport}
	request, err := http.NewRequest(http.MethodPost, "http://paasd/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	transport.CloseIdleConnections()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /ping status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestPingMissingSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Ping(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("Ping unexpectedly succeeded against a missing socket")
	}
}
