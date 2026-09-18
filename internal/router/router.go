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
	upstream *url.URL
	proxy    *httputil.ReverseProxy
}

// HostRouter maps normalized Host headers to reverse-proxy targets. Route
// replacement is atomic from a request's perspective and safe to perform
// while requests are in flight.
type HostRouter struct {
	mu     sync.RWMutex
	routes map[string]route
}

// New creates an empty hostname router.
func New() *HostRouter {
	return &HostRouter{routes: make(map[string]route)}
}

// Set adds or replaces a route. The interface intentionally has no error
// return, so invalid routes are ignored. Call SetRoute when validation errors
// need to be surfaced to a caller.
func (r *HostRouter) Set(host string, upstream string) {
	_ = r.SetRoute(host, upstream)
}

// SetRoute validates and atomically installs a route.
func (r *HostRouter) SetRoute(host string, upstream string) error {
	host = normalizeHost(host)
	if host == "" {
		return fmt.Errorf("host must not be empty")
	}
	target, err := url.Parse(upstream)
	if err != nil {
		return fmt.Errorf("parse upstream: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("upstream scheme must be http or https")
	}
	if target.Host == "" {
		return fmt.Errorf("upstream host must not be empty")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	r.mu.Lock()
	r.routes[host] = route{upstream: target, proxy: proxy}
	r.mu.Unlock()
	return nil
}

// Delete removes a route if it exists.
func (r *HostRouter) Delete(host string) {
	r.mu.Lock()
	delete(r.routes, normalizeHost(host))
	r.mu.Unlock()
}

// ServeHTTP routes the request based on the normalized Host header.
func (r *HostRouter) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	host := normalizeHost(request.Host)
	r.mu.RLock()
	entry, ok := r.routes[host]
	r.mu.RUnlock()
	if !ok {
		http.NotFound(w, request)
		return
	}

	entry.proxy.ServeHTTP(w, request)
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
