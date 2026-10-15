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
	"paas/internal/contracts"
	"paas/internal/daemon"
	"paas/internal/deployment"
	"paas/internal/router"
	"paas/internal/sandbox"
	"paas/internal/service"
	"paas/internal/store"
)

func main() {
	socket := flag.String("socket", daemon.DefaultSocketPath, "Unix socket path")
	socketGroup := flag.String("socket-group", "", "existing group granted access to the daemon socket")
	data := flag.String("data-dir", "/var/lib/paas", "persistent data directory")
	listen := flag.String("listen", "127.0.0.1:8080", "loopback router address")
	webListen := flag.String("web-listen", "127.0.0.1:3000", "numeric loopback dashboard address; empty disables the dashboard")
	hashPassword := flag.Bool("hash-password", false, "generate a dashboard password hash using a hidden terminal prompt, then exit")
	buildUser := flag.String("build-user", "paas-build", "build user when daemon runs as root")
	runtimeUser := flag.String("runtime-user", "paas-runtime", "runtime user when daemon runs as root")
	buildTimeout := flag.Duration("build-timeout", 5*time.Minute, "maximum build duration")
	sandboxType := flag.String("sandbox", "default", "sandbox implementation: default, systemd, bubblewrap, host")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional argument")
	}
	if *hashPassword {
		if err := printPasswordHash(); err != nil {
			log.Fatal(err)
		}
		return
	}
	// Catch termination before restoring services or recovering applications;
	// those phases can each run for minutes and must remain cancellable.
	startupCtx, stopSignals := startupSignalContext()
	defer stopSignals()
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
	server, err := daemon.NewServerWithSocketGroup(*socket, *socketGroup)
	if err != nil {
		log.Fatal(err)
	}
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
	var sb contracts.Sandbox
	switch *sandboxType {
	case "host":
		sb = sandbox.NewHost()
	case "bubblewrap":
		sb, err = sandbox.NewBubblewrap()
	case "systemd":
		sb, err = sandbox.NewSystemd()
	case "default", "":
		sb, err = sandbox.NewDefault()
	default:
		log.Fatalf("unknown sandbox type %q: must be default, systemd, bubblewrap, or host", *sandboxType)
	}
	if err != nil {
		log.Fatalf("initialize %s sandbox: %v", *sandboxType, err)
	}
	log.Printf("runtime sandbox=%s", sb.Kind())
	deploySvc, err := deployment.NewWithContext(startupCtx, db, builder.New(builder.Options{DataDir: dataDir, BuildUser: *buildUser, Timeout: *buildTimeout}), sb, hostRouter, deployment.Options{DataDir: dataDir, RuntimeUser: *runtimeUser})
	if err != nil {
		if startupCtx.Err() != nil {
			log.Printf("shutdown requested during startup; shutting down")
			return
		}
		log.Fatal(err)
	}
	routerListener, err := net.Listen("tcp", *listen)
	if err != nil {
		_ = deploySvc.Close()
		log.Fatal(err)
	}
	svcManager := service.NewManager(db, dataDir)
	server = daemon.NewPlatformServerWithServicesAndBuildUser(*socket, deploySvc, svcManager, *buildUser, routerListener.Addr().String())
	webServer, webListener, err := listenDashboard(*webListen, server.Handler(), server.PublicHooks())
	if err != nil {
		_ = routerListener.Close()
		_ = deploySvc.Close()
		log.Fatal(err)
	}
	if webListener != nil {
		defer webListener.Close()
	}
	serviceRestoreErr, recoveryErr := restoreAndRecover(startupCtx, svcManager.Restore, deploySvc.Recover)
	if serviceRestoreErr != nil {
		log.Printf("restore managed services: %v", serviceRestoreErr)
	}
	if recoveryErr != nil {
		log.Printf("recover applications: %v", recoveryErr)
	}
	routerServer := &http.Server{Handler: hostRouter, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}

	if startupCtx.Err() == nil {
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		routerDone := make(chan error, 1)
		go func() { routerDone <- routerServer.Serve(routerListener) }()
		var webDone chan error
		if webServer != nil {
			webDone = make(chan error, 1)
			go func() { webDone <- webServer.Serve(webListener) }()
			log.Printf("dashboard ready: http://%s", webListener.Addr())
		}
		log.Printf("paasd ready: socket=%s router=http://%s data=%s", server.SocketPath(), routerListener.Addr(), dataDir)

		select {
		case err := <-serveDone:
			if err != nil {
				log.Printf("daemon: %v", err)
			}
		case err := <-routerDone:
			log.Printf("router: %v", err)
		case err := <-webDone:
			log.Printf("dashboard: %v", err)
		case <-startupCtx.Done():
			log.Printf("shutdown requested; shutting down")
		}
	} else {
		log.Printf("shutdown requested during startup; shutting down")
	}
	// Stop accepting dashboard requests before closing the deployment engine.
	// Close also cancels active requests, including synchronous deployment calls.
	if webServer != nil {
		_ = webServer.Close()
	}
	if err := deploySvc.Close(); err != nil {
		log.Printf("stop runtimes: %v", err)
	}
	serviceCloseCtx, serviceCloseCancel := context.WithTimeout(context.Background(), time.Minute)
	if err := svcManager.Close(serviceCloseCtx); err != nil {
		log.Printf("stop managed services: %v", err)
	}
	serviceCloseCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown daemon: %v\n", err)
	}
	_ = routerServer.Shutdown(ctx)
}

func startupSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func restoreAndRecover(ctx context.Context, restore func(context.Context) error, recover func(context.Context) error) (error, error) {
	serviceCtx, serviceCancel := context.WithTimeout(ctx, time.Minute)
	serviceRestoreErr := restore(serviceCtx)
	serviceCancel()
	if ctx.Err() != nil {
		return serviceRestoreErr, ctx.Err()
	}

	// Recover every independent app even when a managed-service restore failed.
	// Deployment checks each binding's persisted READY state, so dependent apps
	// keep desired_running set and record a failed recovery candidate for retry.
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 10*time.Minute)
	recoveryErr := recover(recoveryCtx)
	recoveryCancel()
	return serviceRestoreErr, recoveryErr
}
