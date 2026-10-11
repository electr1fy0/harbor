package main

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"paas/internal/ingress"
)

// An empty address keeps TLS authorization disabled for local development.
func listenTLSAsk(address string, authorizer ingress.DomainAuthorizer) (*http.Server, net.Listener, error) {
	if address == "" {
		return nil, nil, nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS ask listener: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, nil, fmt.Errorf("TLS ask listener must bind a numeric loopback address")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS ask listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/tls/ask", ingress.NewAskHandler(authorizer))
	return &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}, listener, nil
}
