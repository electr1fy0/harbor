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
	"sync"
	"syscall"
	"testing"
	"time"
)

type delayedUnixListener struct {
	*net.UnixListener
	closed        chan struct{}
	continueClose chan struct{}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ps-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "s.sock")
}

func (l *delayedUnixListener) Close() error {
	err := l.UnixListener.Close()
	close(l.closed)
	<-l.continueClose
	return err
}

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

func TestConcurrentListenersSerializeStaleSocketReplacement(t *testing.T) {
	socketPath := shortSocketPath(t)
	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	server := NewServer(socketPath)
	const callers = 8
	listeners := make(chan net.Listener, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			listener, err := server.Listen()
			if err == nil {
				listeners <- listener
			}
		}()
	}
	wg.Wait()
	close(listeners)
	var winners []net.Listener
	for listener := range listeners {
		winners = append(winners, listener)
	}
	if len(winners) != 1 {
		for _, listener := range winners {
			listener.Close()
		}
		t.Fatalf("concurrent Listen calls created %d listeners, want exactly one", len(winners))
	}
	if err := winners[0].Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSocketLockLivesThroughCloseAndUnlink(t *testing.T) {
	socketPath := shortSocketPath(t)
	server := NewServer(socketPath)
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	owned := listener.(*socketListener)
	unixListener := owned.Listener.(*net.UnixListener)
	owned.Listener = &delayedUnixListener{UnixListener: unixListener, closed: make(chan struct{}), continueClose: make(chan struct{})}
	closeDone := make(chan error, 1)
	go func() { closeDone <- owned.Close() }()
	delayed := owned.Listener.(*delayedUnixListener)
	<-delayed.closed // The old socket is closed but its stale path still exists.

	listenDone := make(chan error, 1)
	go func() {
		candidate, listenErr := NewServer(socketPath).Listen()
		if candidate != nil {
			_ = candidate.Close()
		}
		listenDone <- listenErr
	}()
	select {
	case err := <-listenDone:
		if err == nil {
			t.Fatal("a second listener acquired the path before old close/unlink completed")
		}
	case <-time.After(time.Second):
		t.Fatal("competing Listen did not fail promptly while the socket lock was held")
	}
	close(delayed.continueClose)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}

	newListener, err := NewServer(socketPath).Listen()
	if err != nil {
		t.Fatalf("Listen after old close completed: %v", err)
	}
	if err := newListener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListenRejectsSymlinkedOrPublicSocketLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(root, "symlink.sock")
	if err := os.Symlink(target, socketPath+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(socketPath).Listen(); err == nil {
		t.Fatal("Listen accepted a symlinked lock file")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("lock target changed to %q, err=%v", data, err)
	}
	if err := os.Remove(socketPath + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(socketPath).Listen(); err == nil {
		t.Fatal("Listen accepted a publicly accessible lock file")
	}
}

func TestServeDoesNotRemoveReplacementSocketPath(t *testing.T) {
	socketPath := shortSocketPath(t)
	server := NewServer(socketPath)
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	if _, err := Ping(context.Background(), socketPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(socketPath, socketPath+".original"); err != nil {
		t.Fatal(err)
	}
	const replacement = "replacement belongs to another process"
	if err = os.WriteFile(socketPath, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = <-serveDone; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(socketPath)
	if err != nil || string(data) != replacement {
		t.Fatalf("replacement path content = %q, err=%v", data, err)
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
