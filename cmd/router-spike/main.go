// Command router-spike demonstrates hostname routing with two local backend
// processes. It is intentionally standalone so it can be used before the
// deployment orchestrator exists.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"paas/internal/router"
)

func main() {
	listenAddress := flag.String("listen", "127.0.0.1:8080", "router listen address")
	demo := flag.Bool("demo", false, "start two demo backends")
	flag.Parse()

	hostRouter := router.New()
	var backendServers []*http.Server
	if *demo {
		for _, backend := range []struct {
			host string
			body string
		}{
			{host: "a.localhost", body: "server A\n"},
			{host: "b.localhost", body: "server B\n"},
		} {
			listener, server, err := startBackend(backend.body)
			if err != nil {
				log.Fatal(err)
			}
			backendServers = append(backendServers, server)
			if err := hostRouter.SetRoute(backend.host, "http://"+listener.Addr().String()); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s -> %s\n", backend.host, listener.Addr())
		}
	} else {
		fmt.Println("router started without demo routes")
	}

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: hostRouter}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("router: %v", err)
		}
	}()
	fmt.Printf("router listening on http://%s\n", listener.Addr())

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	signal.Stop(signals)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	for _, backend := range backendServers {
		_ = backend.Shutdown(ctx)
	}
}

func startBackend(body string) (net.Listener, *http.Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = server.Serve(listener) }()
	return listener, server, nil
}
