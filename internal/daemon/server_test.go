package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
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

func TestSocketGroupAccessIsExplicitAndValidated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unavailable on Windows")
	}
	if _, err := NewServerWithSocketGroup(filepath.Join(t.TempDir(), "bad.sock"), "paas-group-that-does-not-exist"); err == nil {
		t.Fatal("unknown socket group was accepted")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Fatal(err)
	}
	shortDir, err := os.MkdirTemp("/tmp", "pg-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shortDir)
	socketPath := filepath.Join(shortDir, "g.sock")
	server, err := NewServerWithSocketGroup(socketPath, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0660 {
		t.Fatalf("group socket permissions = %04o, want 0660", info.Mode().Perm())
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	wantGID, err := strconv.Atoi(current.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || int(sys.Gid) != wantGID {
		t.Fatalf("socket group id = %#v, want %s", info.Sys(), current.Gid)
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
