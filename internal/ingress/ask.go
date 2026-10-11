// Package ingress contains the authorization endpoint used by Caddy's
// on-demand TLS flow.
package ingress

import (
	"net"
	"net/http"
	"strings"
)

// DomainAuthorizer is implemented by the deployment service. Only domains
// explicitly configured on a Harbor application may obtain a certificate.
type DomainAuthorizer interface {
	IsDomainAllowed(host string) bool
}

// NewAskHandler creates the Caddy on-demand TLS authorization handler. The
// listener should bind to loopback; the handler also rejects non-loopback
// callers so accidental public binding does not expose an authorization API.
func NewAskHandler(authorizer DomainAuthorizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isLoopbackRemote(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		query := r.URL.Query()
		domains, ok := query["domain"]
		if len(query) != 1 || !ok || len(domains) != 1 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		domain, ok := normalizeDomain(domains[0])
		if !ok || authorizer == nil || !authorizer.IsDomainAllowed(domain) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func isLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.IsLoopback()
	}
	return ip.IsLoopback()
}

func normalizeDomain(value string) (string, bool) {
	if value == "" || len(value) > 253 || value != strings.TrimSpace(value) {
		return "", false
	}
	if strings.ContainsAny(value, ":/@\\%\x00\r\n\t") || strings.Contains(value, "..") {
		return "", false
	}
	domain := strings.ToLower(strings.TrimSuffix(value, "."))
	if domain == "" || net.ParseIP(domain) != nil || !strings.Contains(domain, ".") {
		return "", false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, character := range label {
			if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
				return "", false
			}
		}
	}
	return domain, true
}
