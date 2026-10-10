// Package router contains the hostname reverse-proxy implementation.
package router

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"paas/internal/contracts"
)

var _ contracts.Router = (*HostRouter)(nil)

type route struct {
	proxy    *httputil.ReverseProxy
	inflight int
	retired  bool
	drained  chan struct{}
}

// HostRouter maps normalized Host headers to reverse-proxy targets. Route
// replacement is atomic from a request's perspective and safe to perform
// while requests are in flight.
type HostRouter struct {
	mu     sync.Mutex
	routes map[string]*route
}

// New creates an empty hostname router.
func New() *HostRouter {
	return &HostRouter{routes: make(map[string]*route)}
}

// Set adds or replaces a route. The interface intentionally has no error
// return, so invalid routes are ignored. Call SetRoute when validation errors
// need to be surfaced to a caller.
func (r *HostRouter) Set(host string, upstream string) {
	_ = r.SetRoute(host, upstream)
}

// SetRoute validates and atomically installs a route.
func (r *HostRouter) SetRoute(host string, upstream string) error {
	_, err := r.SwitchRoute(host, upstream, nil)
	return err
}

// SwitchRoute validates a replacement before committing durable state. Requests
// cannot observe the replacement until commit succeeds. The returned channel
// closes when every request assigned to the old route has finished.
// commit must not call back into the router.
func (r *HostRouter) SwitchRoute(host, upstream string, commit func() error) (<-chan struct{}, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("host must not be empty")
	}
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("upstream scheme must be http or https")
	}
	if target.Host == "" {
		return nil, fmt.Errorf("upstream host must not be empty")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if commit != nil {
		if err := commit(); err != nil {
			return nil, err
		}
	}
	old := r.routes[host]
	r.routes[host] = &route{proxy: proxy, drained: make(chan struct{})}
	if old == nil {
		done := make(chan struct{})
		close(done)
		return done, nil
	}
	r.retire(old)
	return old.drained, nil
}

// Delete removes a route if it exists.
func (r *HostRouter) Delete(host string) {
	r.mu.Lock()
	host = normalizeHost(host)
	if old := r.routes[host]; old != nil {
		r.retire(old)
	}
	delete(r.routes, host)
	r.mu.Unlock()
}

// ServeHTTP routes the request based on the normalized Host header.
func (r *HostRouter) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	host := normalizeHost(request.Host)
	r.mu.Lock()
	entry, ok := r.routes[host]
	if ok {
		entry.inflight++
	}
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, request)
		return
	}

	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		entry.inflight--
		if entry.retired && entry.inflight == 0 {
			close(entry.drained)
		}
	}()
	entry.proxy.ServeHTTP(w, request)
}

// retire is called under mu, once for each route entry.
func (r *HostRouter) retire(entry *route) {
	entry.retired = true
	if entry.inflight == 0 {
		close(entry.drained)
	}
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsedHost, "[]")
	}
	return strings.Trim(host, "[]")
}
