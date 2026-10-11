// Package router contains the hostname reverse-proxy implementation.
package router

import (
	"fmt"
	"log"
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
	updateMu sync.Mutex
	mu       sync.Mutex
	routes   map[string]*route
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
	return r.SwitchRoutes([]string{host}, upstream, commit)
}

// SwitchRoutes atomically replaces a group of hostname mappings with one
// upstream. The returned channel closes after requests using any old mapping
// finish, which lets callers drain every alias before retiring a deployment.
func (r *HostRouter) SwitchRoutes(hosts []string, upstream string, commit func() error) (<-chan struct{}, error) {
	hosts, err := normalizedHosts(hosts)
	if err != nil {
		return nil, err
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
		log.Printf("proxy hosts %v to %s: %v", hosts, target.Redacted(), err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	// Serialize mutations across the durable commit and route replacement,
	// while continuing to serve and drain requests on the current routes.
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	if commit != nil {
		if err := commit(); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var draining []<-chan struct{}
	for _, host := range hosts {
		old := r.routes[host]
		r.routes[host] = &route{proxy: proxy, drained: make(chan struct{})}
		if old != nil {
			r.retire(old)
			draining = append(draining, old.drained)
		}
	}
	if len(draining) == 0 {
		done := make(chan struct{})
		close(done)
		return done, nil
	}
	drained := make(chan struct{})
	go func() {
		for _, done := range draining {
			<-done
		}
		close(drained)
	}()
	return drained, nil
}

// UpdateRoutes commits a durable route update and then atomically applies the
// desired mappings in memory. remove may overlap hosts; the new mappings win.
func (r *HostRouter) UpdateRoutes(hosts, remove []string, upstream string, commit func() error) error {
	setHosts, err := normalizedHosts(hosts)
	if err != nil {
		return err
	}
	removeHosts, err := normalizedHosts(remove)
	if err != nil {
		return err
	}
	var proxy *httputil.ReverseProxy
	if len(setHosts) > 0 {
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
		proxy = httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("proxy to %s: %v", target.Redacted(), err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
	}
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, host := range removeHosts {
		if old := r.routes[host]; old != nil {
			r.retire(old)
			delete(r.routes, host)
		}
	}
	for _, host := range setHosts {
		old := r.routes[host]
		r.routes[host] = &route{proxy: proxy, drained: make(chan struct{})}
		if old != nil {
			r.retire(old)
		}
	}
	return nil
}

// RemoveRoutes removes a group of hostname mappings after commit succeeds.
func (r *HostRouter) RemoveRoutes(hosts []string, commit func() error) error {
	return r.UpdateRoutes(nil, hosts, "", commit)
}

func normalizedHosts(hosts []string) ([]string, error) {
	normalized := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		host = normalizeHost(host)
		if host == "" {
			return nil, fmt.Errorf("host must not be empty")
		}
		if !seen[host] {
			seen[host] = true
			normalized = append(normalized, host)
		}
	}
	return normalized, nil
}

// Delete removes a route if it exists.
func (r *HostRouter) Delete(host string) {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
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
