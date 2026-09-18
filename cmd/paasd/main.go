// Command paasd is the local control-plane daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"paas/internal/daemon"
)

func main() {
	socket := flag.String("socket", daemon.DefaultSocketPath, "Unix socket path")
	flag.Parse()

	server := daemon.NewServer(*socket)
	listener, err := server.Listen()
	if err != nil {
		log.Fatal(err)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case err := <-serveDone:
		if err != nil {
			log.Fatal(err)
		}
	case sig := <-signals:
		log.Printf("received %s; shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "shutdown daemon: %v\n", err)
			os.Exit(1)
		}
		if err := <-serveDone; err != nil {
			log.Fatal(err)
		}
	}
}
