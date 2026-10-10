// Package health probes an application's loopback listener.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	maxAttempts    = 10
	requestTimeout = time.Second
	retryDelay     = 500 * time.Millisecond
)

// Check polls a loopback TCP port or HTTP path until it responds or attempts
// are exhausted. Redirect responses are accepted without following them.
func Check(ctx context.Context, port int, path string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid health check port %d", port)
	}
	var target *url.URL
	if path != "" {
		parsed, err := url.Parse(path)
		if err != nil {
			return fmt.Errorf("invalid health check path: %w", err)
		}
		if parsed.IsAbs() || parsed.Host != "" {
			return errors.New("health check path must be relative to loopback")
		}
		if parsed.Path == "" {
			parsed.Path = "/"
		}
		parsed.Scheme = "http"
		parsed.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		parsed.Fragment = ""
		target = parsed
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if target == nil {
			err = probeTCP(ctx, port)
		} else {
			err = probeHTTP(ctx, client, target)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		if attempt+1 == maxAttempts {
			break
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("health check failed after %d attempts: %w", maxAttempts, lastErr)
}

func probeTCP(ctx context.Context, port int) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(requestCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return connection.Close()
}

func probeHTTP(ctx context.Context, client *http.Client, target *url.URL) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= 400 {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}
