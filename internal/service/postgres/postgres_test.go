package postgres

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"paas/internal/contracts"
	"paas/internal/runtime"
)

func TestProbePostgresWaitsForReadyForQuery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var length [4]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			serverDone <- err
			return
		}
		payload := make([]byte, binary.BigEndian.Uint32(length[:])-4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			serverDone <- err
			return
		}
		// PostgreSQL AuthenticationOk, followed by ReadyForQuery (idle).
		if _, err := conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0, 'Z', 0, 0, 0, 5, 'I'}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var port int
	for _, char := range portText {
		port = port*10 + int(char-'0')
	}
	if err := probePostgres(context.Background(), port); err != nil {
		t.Fatalf("probePostgres() error = %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestProbePostgresRejectsTCPOnlyListenerAndHonorsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-serverDone
		}
	}()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var port int
	for _, char := range portText {
		port = port*10 + int(char-'0')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = probePostgres(ctx, port)
	close(serverDone)
	if err == nil {
		t.Fatal("TCP-only listener was accepted as a ready PostgreSQL server")
	}
	if time.Since(started) > time.Second {
		t.Fatalf("probe ignored cancellation for %s", time.Since(started))
	}
}

type postgresTestSandbox struct {
	info    *contracts.SandboxInfo
	stopErr error
	stops   int
}

func (*postgresTestSandbox) Kind() string { return "test" }
func (*postgresTestSandbox) Start(context.Context, contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	return nil, nil
}
func (s *postgresTestSandbox) Stop(context.Context, string) error {
	s.stops++
	return s.stopErr
}
func (s *postgresTestSandbox) Inspect(context.Context, string) (*contracts.SandboxInfo, error) {
	return s.info, nil
}

func TestStartRejectsMismatchedOwnedIdentity(t *testing.T) {
	sandbox := &postgresTestSandbox{info: &contracts.SandboxInfo{ID: "different-id", Running: true}}
	provider := NewProvider(t.TempDir(), runtime.NewManager(t.TempDir()), sandbox)
	err := provider.Start(context.Background(), &contracts.Service{ID: "expected-id", Port: 5432})
	if err == nil || !strings.Contains(err.Error(), "mismatched identity") {
		t.Fatalf("Start() error = %v; want identity mismatch", err)
	}
	if sandbox.stops != 0 {
		t.Fatalf("Start() stopped mismatched sandbox %d times", sandbox.stops)
	}
}

func TestStartPreservesCleanupErrorWhenPostgresIsUnready(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	// Accept the startup packet but never reply as PostgreSQL.
	serverDone := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-serverDone
		}
	}()
	defer close(serverDone)
	stopFailure := errors.New("sandbox stop failed")
	sandbox := &postgresTestSandbox{info: &contracts.SandboxInfo{ID: "owned-id", Running: true}, stopErr: stopFailure}
	provider := NewProvider(t.TempDir(), runtime.NewManager(t.TempDir()), sandbox)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = provider.Start(ctx, &contracts.Service{ID: "owned-id", Port: port})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, stopFailure) || !strings.Contains(err.Error(), "stop unready postgres sandbox") {
		t.Fatalf("Start() error = %v; want readiness and cleanup diagnostics", err)
	}
	if sandbox.stops != 1 {
		t.Fatalf("unready sandbox stop calls = %d, want 1", sandbox.stops)
	}
}
