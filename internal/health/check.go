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
	return CheckWithLiveness(ctx, port, path, nil)
}

// CheckWithLiveness stops retrying readiness when the caller can confirm that
// the runtime has exited. A nil callback preserves Check's polling behavior.
// The callback is consulted after a failed probe, while ctx is still active.
func CheckWithLiveness(ctx context.Context, port int, path string, liveness func(context.Context) error) error {
	if ctx == nil {
		return errors.New("health check context must not be nil")
	}
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
		if liveness != nil {
			if liveErr := liveness(ctx); liveErr != nil {
				return fmt.Errorf("readiness failed: %w", errors.Join(liveErr, lastErr))
			}
		}
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

// Probe performs one bounded readiness-style request. Runtime supervision uses
// this separately from Check, which retries during startup before publication.
func Probe(ctx context.Context, port int, path string, timeout time.Duration) error {
	if ctx == nil {
		return errors.New("health probe context must not be nil")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid health check port %d", port)
	}
	if timeout < time.Millisecond || timeout > 30*time.Second {
		return errors.New("health probe timeout must be between 1ms and 30s")
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if path == "" {
		connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return err
		}
		return connection.Close()
	}
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
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
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
