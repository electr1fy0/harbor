package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Ping calls the daemon's health endpoint over its Unix socket.
func Ping(ctx context.Context, socketPath string) (string, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://paasd/ping", nil)
	if err != nil {
		return "", fmt.Errorf("create ping request: %w", err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return "", fmt.Errorf("ping daemon: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read ping response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ping daemon: status %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	return strings.TrimSpace(string(body)), nil
}
