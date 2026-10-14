package main

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"
	"paas/internal/dashboard"
	webassets "paas/web"
)

func listenDashboard(address string, control http.Handler) (*http.Server, net.Listener, error) {
	if address == "" {
		return nil, nil, nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard listener: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, nil, fmt.Errorf("dashboard listener must bind a numeric loopback address")
	}
	config, err := dashboard.ConfigFromEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard configuration: %w", err)
	}
	assets, err := fs.Sub(webassets.Assets, "dist")
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard assets: %w", err)
	}
	handler, err := dashboard.NewHandler(dashboard.NewAPI(control), assets, config)
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard handler: %w", err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard listener: %w", err)
	}
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}, listener, nil
}

func printPasswordHash() error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("password hashing requires an interactive terminal: %w", err)
	}
	defer tty.Close()
	state, err := term.GetState(int(tty.Fd()))
	if err != nil {
		return fmt.Errorf("read terminal state: %w", err)
	}
	// Restore terminal echo even when the operator interrupts a hidden prompt.
	interrupts := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	defer close(done)
	go func() {
		select {
		case <-interrupts:
			_ = term.Restore(int(tty.Fd()), state)
			fmt.Fprintln(tty)
			os.Exit(130)
		case <-done:
		}
	}()
	hash, err := passwordHashFromPrompt(func(prompt string) ([]byte, error) {
		fmt.Fprint(tty, prompt)
		password, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		return password, err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, hash)
	return err
}

func passwordHashFromPrompt(read func(string) ([]byte, error)) (string, error) {
	password, err := read("New administrator password (12+ bytes): ")
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	defer clear(password)
	if len(password) < 12 || len(password) > 1024 {
		return "", fmt.Errorf("administrator password must contain 12 to 1024 UTF-8 bytes")
	}
	confirmation, err := read("Confirm administrator password: ")
	if err != nil {
		return "", fmt.Errorf("confirm password: %w", err)
	}
	defer clear(confirmation)
	if string(password) != string(confirmation) {
		return "", fmt.Errorf("the passwords did not match")
	}
	return dashboard.GeneratePasswordHash(string(password))
}
