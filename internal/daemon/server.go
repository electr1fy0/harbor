// Package daemon implements the small local control-plane endpoint used by
// the CLI. It intentionally uses HTTP semantics over a Unix socket so the
// protocol can grow without introducing a second transport abstraction.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"paas/internal/onboarding"
)

// DefaultSocketPath is the target Linux control-plane socket from the build
// plan. Tests and local development should override it with a temp path.
const DefaultSocketPath = "/run/paas/paasd.sock"

// Server serves the daemon API over a Unix-domain socket.
type Server struct {
	socketPath     string
	socketGID      int
	http           *http.Server
	onboardingJobs *onboarding.Manager
}

// NewServer creates a daemon server. An empty path uses DefaultSocketPath.
func NewServer(socketPath string) *Server {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pong\n"))
	})

	return &Server{
		socketPath: socketPath,
		socketGID:  -1,
		http:       &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second},
	}
}

// NewServerWithSocketGroup enables owner-and-group access after validating an
// existing system group. An empty group retains the owner-only default.
func NewServerWithSocketGroup(socketPath, group string) (*Server, error) {
	server := NewServer(socketPath)
	if group == "" {
		return server, nil
	}
	entry, err := user.LookupGroup(group)
	if err != nil {
		return nil, fmt.Errorf("look up socket group %q: %w", group, err)
	}
	gid, err := strconv.Atoi(entry.Gid)
	if err != nil || gid < 0 {
		return nil, fmt.Errorf("socket group %q has an invalid group id", group)
	}
	server.socketGID = gid
	return server, nil
}

// SocketPath returns the configured Unix socket path.
func (s *Server) SocketPath() string { return s.socketPath }

// Listen creates the Unix listener and removes only a stale socket at the
// exact configured path. A non-socket file is never overwritten.
func (s *Server) Listen() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return nil, fmt.Errorf("create daemon socket directory: %w", err)
	}

	if info, err := os.Lstat(s.socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("daemon socket path exists and is not a socket: %s", s.socketPath)
		}
		conn, dialErr := net.DialTimeout("unix", s.socketPath, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("daemon already listening on %s", s.socketPath)
		}
		if !errors.Is(dialErr, os.ErrNotExist) && !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot verify stale daemon socket: %w", dialErr)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return nil, fmt.Errorf("remove stale daemon socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect daemon socket path: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon socket: %w", err)
	}
	mode := os.FileMode(0600)
	if s.socketGID >= 0 {
		if err := os.Chown(s.socketPath, -1, s.socketGID); err != nil {
			_ = listener.Close()
			_ = os.Remove(s.socketPath)
			return nil, fmt.Errorf("set daemon socket group: %w", err)
		}
		mode = 0660
	}
	if err := os.Chmod(s.socketPath, mode); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.socketPath)
		return nil, err
	}
	return listener, nil
}

// Serve runs the HTTP server on listener. The socket is removed when serving
// finishes so a stopped daemon does not leave an ambiguous endpoint behind.
func (s *Server) Serve(listener net.Listener) error {
	defer func() { _ = os.Remove(s.socketPath) }()
	if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	var jobErr error
	if s.onboardingJobs != nil {
		jobErr = s.onboardingJobs.Close(ctx)
	}
	return errors.Join(jobErr, s.http.Shutdown(ctx))
}
