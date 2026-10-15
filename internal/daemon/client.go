package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"paas/internal/contracts"
)

const (
	maxExecuteRequestBytes  = contracts.MaxControlRequestBytes
	maxPingResponseBytes    = 1024
	maxExecuteResponseBytes = 4 << 20
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

	body, err := io.ReadAll(io.LimitReader(response.Body, maxPingResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read ping response: %w", err)
	}
	if len(body) > maxPingResponseBytes {
		return "", fmt.Errorf("read ping response: response exceeds %d bytes", maxPingResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ping daemon: status %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	result := strings.TrimSpace(string(body))
	if result != "pong" {
		return "", errors.New("ping daemon: unexpected health response")
	}
	return result, nil
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
	if err := validateControlRequestBodySize(len(body)); err != nil {
		return nil, fmt.Errorf("encode daemon request: %w", err)
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
	output, err := decodeExecuteResponse(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		message := strings.TrimSpace(output.Error)
		if message == "" {
			return output, fmt.Errorf("%s daemon: unexpected HTTP response %s", command, res.Status)
		}
		return output, errors.New(message)
	}
	return output, nil
}

// ValidateRequestSize checks the serialized request envelope before a caller
// attempts to send it. Individual fields can fit their own limits while the
// complete JSON request exceeds the daemon's control body limit.
func ValidateRequestSize(input Request) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode daemon request: %w", err)
	}
	return validateControlRequestBodySize(len(body))
}

func validateControlRequestBodySize(size int) error {
	if size > maxExecuteRequestBytes {
		return fmt.Errorf("request exceeds %d bytes", maxExecuteRequestBytes)
	}
	return nil
}

func decodeExecuteResponse(reader io.Reader) (*Response, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxExecuteResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read daemon response: %w", err)
	}
	if len(body) > maxExecuteResponseBytes {
		return nil, fmt.Errorf("decode daemon response: response exceeds %d bytes", maxExecuteResponseBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var output *Response
	if err = decoder.Decode(&output); err != nil || output == nil {
		if err == nil {
			err = fmt.Errorf("response must be a JSON object")
		}
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("response contains trailing JSON data")
		}
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	return output, nil
}
