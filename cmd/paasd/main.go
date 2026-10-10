// Command paasd is the local control-plane daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"net"
	"net/http"
	"paas/internal/builder"
	"paas/internal/daemon"
	"paas/internal/deployment"
	"paas/internal/router"
	"paas/internal/sandbox"
	"paas/internal/store"
)

func main() {
	socket := flag.String("socket", daemon.DefaultSocketPath, "Unix socket path")
	data := flag.String("data-dir", "/var/lib/paas", "persistent data directory")
	listen := flag.String("listen", "127.0.0.1:8080", "loopback router address")
	buildUser := flag.String("build-user", "paas-build", "build user when daemon runs as root")
	runtimeUser := flag.String("runtime-user", "paas-runtime", "runtime user when daemon runs as root")
	buildTimeout := flag.Duration("build-timeout", 5*time.Minute, "maximum build duration")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional argument")
	}
	if *buildTimeout <= 0 {
		log.Fatal("build-timeout must be positive")
	}
	if os.Geteuid() == 0 {
		identities := make(map[string]bool)
		for _, name := range []string{*buildUser, *runtimeUser} {
			account, err := user.Lookup(name)
			if err != nil || account.Uid == "0" {
				log.Fatalf("unprivileged user %q required; run scripts/setup-linux.sh", name)
			}
			if identities[account.Uid] {
				log.Fatal("build and runtime users must have distinct non-root UIDs")
			}
			identities[account.Uid] = true
		}
	}
	dataDir, err := filepath.Abs(*data)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatal(err)
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal(err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		log.Fatal("router must bind a numeric loopback address")
	}
	// Acquire both identities before touching persisted deployment state.
	lock, err := os.OpenFile(filepath.Join(dataDir, ".daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatalf("another daemon is using data directory %s: %v", dataDir, err)
	}
	server := daemon.NewServer(*socket)
	listener, err := server.Listen()
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	db, err := store.Open(filepath.Join(dataDir, "paas.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	hostRouter := router.New()
	service, err := deployment.New(db, builder.New(builder.Options{DataDir: dataDir, BuildUser: *buildUser, Timeout: *buildTimeout}), sandbox.NewHost(), hostRouter, deployment.Options{DataDir: dataDir, RuntimeUser: *runtimeUser})
	if err != nil {
		log.Fatal(err)
	}
	routerListener, err := net.Listen("tcp", *listen)
	if err != nil {
		_ = service.Close()
		log.Fatal(err)
	}
	routerServer := &http.Server{Handler: hostRouter, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}

	server = daemon.NewPlatformServer(*socket, service, routerListener.Addr().String())

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	routerDone := make(chan error, 1)
	go func() { routerDone <- routerServer.Serve(routerListener) }()
	log.Printf("paasd ready: socket=%s router=http://%s data=%s", server.SocketPath(), routerListener.Addr(), dataDir)

	select {
	case err := <-serveDone:
		if err != nil {
			log.Printf("daemon: %v", err)
		}
	case err := <-routerDone:
		log.Printf("router: %v", err)
	case sig := <-signals:
		log.Printf("received %s; shutting down", sig)
	}
	if err := service.Close(); err != nil {
		log.Printf("stop runtimes: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown daemon: %v\n", err)
	}
	_ = routerServer.Shutdown(ctx)
}
