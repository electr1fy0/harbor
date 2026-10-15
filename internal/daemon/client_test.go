package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDecodeExecuteResponseRequiresOneBoundedObject(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "null response", body: `null`},
		{name: "trailing JSON value", body: `{"Error":"first"}{"Error":"second"}`},
		{name: "trailing garbage", body: `{"Error":"first"}garbage`},
		{name: "oversized body", body: `{"Logs":"` + strings.Repeat("x", maxExecuteResponseBytes) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeExecuteResponse(strings.NewReader(test.body)); err == nil {
				t.Fatal("decodeExecuteResponse accepted invalid or oversized response")
			}
		})
	}

	response, err := decodeExecuteResponse(strings.NewReader(` {"Error":"ok"} `))
	if err != nil || response.Error != "ok" {
		t.Fatalf("decodeExecuteResponse() = %+v, %v", response, err)
	}
}

func TestPingRequiresTheDaemonHealthPayload(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
		fail bool
	}{
		{name: "protocol payload", body: "pong\n", want: "pong"},
		{name: "unexpected payload", body: "healthy", fail: true},
		{name: "empty payload", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			socketPath := shortSocketPath(t)
			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = server.Shutdown(ctx)
				_ = os.Remove(socketPath)
			})

			got, err := Ping(context.Background(), socketPath)
			if test.fail {
				if err == nil {
					t.Fatalf("Ping() = %q, nil; want protocol error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("Ping() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestExecuteReportsStatusWhenErrorMessageIsMissing(t *testing.T) {
	socketPath := shortSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"URL":"http://demo.localhost:8080"}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = os.Remove(socketPath)
	})

	response, err := Execute(context.Background(), socketPath, "inspect", Request{Name: "demo"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Execute() error = %v, want an HTTP status fallback", err)
	}
	if response == nil || response.URL != "http://demo.localhost:8080" {
		t.Fatalf("Execute() response = %+v, want the decoded failure response", response)
	}
}
