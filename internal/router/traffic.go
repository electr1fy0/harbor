package router

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"time"
)

// TrafficPolicy applies only to a configured route, never to an unknown host.
type TrafficPolicy struct {
	Maintenance bool   `json:"maintenance"`
	Message     string `json:"message"`
}

// Called under r.mu. The stable app hostname owns routing policy so fresh
// aliases and restarted runtimes receive it before they can serve traffic.
func (r *HostRouter) inheritTrafficPolicy(hosts []string) {
	for _, anchor := range hosts {
		if !strings.HasSuffix(anchor, ".localhost") {
			continue
		}
		policy := r.policies[anchor]
		for _, host := range hosts {
			if policy == (TrafficPolicy{}) {
				delete(r.policies, host)
			} else {
				r.policies[host] = policy
			}
		}
		return
	}
}

type TrafficStats struct {
	Host           string         `json:"host"`
	Requests       uint64         `json:"requests"`
	StatusCodes    map[int]uint64 `json:"statusCodes"`
	Bytes          uint64         `json:"bytes"`
	TotalLatencyMS float64        `json:"totalLatencyMs"`
	MaxLatencyMS   float64        `json:"maxLatencyMs"`
}

func (r *HostRouter) SetTrafficPolicy(host string, policy TrafficPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	host = normalizeHost(host)
	if policy == (TrafficPolicy{}) {
		delete(r.policies, host)
	} else {
		r.policies[host] = policy
	}
}

// UpdateTrafficPolicies changes one application's aliases atomically and
// avoids modifying a hostname already reassigned to another live app.
func (r *HostRouter) UpdateTrafficPolicies(anchor string, hosts, previous []string, policy TrafficPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	anchor = normalizeHost(anchor)
	keep := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		keep[normalizeHost(host)] = true
	}
	owned := func(host string) bool {
		entry := r.routes[host]
		owner := r.routes[anchor]
		return host == anchor || entry == nil || owner != nil && (owner.proxy == entry.proxy || owner.upstream != "" && owner.upstream == entry.upstream)
	}
	for _, host := range previous {
		host = normalizeHost(host)
		if !keep[host] && owned(host) {
			delete(r.policies, host)
		}
	}
	for _, host := range hosts {
		host = normalizeHost(host)
		if !owned(host) {
			continue
		}
		if policy == (TrafficPolicy{}) {
			delete(r.policies, host)
		} else {
			r.policies[host] = policy
		}
	}
}

func (r *HostRouter) Traffic() []TrafficStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]TrafficStats, 0, len(r.traffic))
	for _, item := range r.traffic {
		copy := item
		copy.StatusCodes = make(map[int]uint64, len(item.StatusCodes))
		for code, count := range item.StatusCodes {
			copy.StatusCodes[code] = count
		}
		result = append(result, copy)
	}
	return result
}

func (r *HostRouter) recordTraffic(host string, status int, bytes int64, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Unrecognized Host headers must not create an unbounded metrics map.
	if _, known := r.routes[host]; !known {
		return
	}
	stat := r.traffic[host]
	stat.Host = host
	stat.Requests++
	stat.Bytes += uint64(bytes)
	if status == 0 {
		status = http.StatusOK
	}
	if stat.StatusCodes == nil {
		stat.StatusCodes = make(map[int]uint64)
	}
	stat.StatusCodes[status]++
	ms := float64(elapsed) / float64(time.Millisecond)
	stat.TotalLatencyMS += ms
	if ms > stat.MaxLatencyMS {
		stat.MaxLatencyMS = ms
	}
	r.traffic[host] = stat
}

type trafficWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *trafficWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *trafficWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil && w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, buffer, err
}
func (w *trafficWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *trafficWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}
func (w *trafficWriter) Flush() { _ = w.FlushError() }
func (w *trafficWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
