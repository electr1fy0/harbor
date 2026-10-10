package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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

// Execute performs a deployment operation through the private Unix socket.
// A non-success response can include a deployment with its failure state.
func Execute(ctx context.Context, socketPath, command string, input Request) (*Response, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	defer transport.CloseIdleConnections()
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://paasd/"+command, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s daemon: %w", command, err)
	}
	defer res.Body.Close()
	var output Response
	if err = json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&output); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return &output, fmt.Errorf("%s", output.Error)
	}
	return &output, nil
}
