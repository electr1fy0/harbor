package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func localDashboardEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PAASD_PUBLIC_ORIGIN", "PAASD_ADMIN_PASSWORD_HASH", "PAASD_SESSION_SECRET"} {
		t.Setenv(name, "")
	}
}

func TestDashboardListenerPrivateAndOptional(t *testing.T) {
	localDashboardEnv(t)
	server, listener, err := listenDashboard("", nil)
	if err != nil || server != nil || listener != nil {
		t.Fatalf("disabled dashboard = %v %v %v", server, listener, err)
	}
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:0", "bad"} {
		_, listener, err := listenDashboard(address, nil)
		if err == nil {
			listener.Close()
			t.Fatalf("accepted non-numeric/public address %q", address)
		}
	}
}

func TestDashboardListenerInProcessAndConfigFailure(t *testing.T) {
	localDashboardEnv(t)
	t.Setenv("PAASD_PUBLIC_ORIGIN", "https://dashboard.example.com")
	if _, listener, err := listenDashboard("127.0.0.1:0", http.NotFoundHandler()); err == nil {
		listener.Close()
		t.Fatal("partial public configuration accepted")
	}
	t.Setenv("PAASD_PUBLIC_ORIGIN", "")
	// The fake handler represents the private control API. No Unix socket is
	// created, so a successful overview proves direct in-process dispatch.
	calls := make(chan string, 4)
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.URL.Path
		if r.URL.Path == "/ping" {
			w.Write([]byte("pong\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apps":
			w.Write([]byte(`{"Apps":[]}`))
		case "/services":
			w.Write([]byte(`{"Services":[]}`))
		default:
			http.NotFound(w, r)
		}
	})
	server, listener, err := listenDashboard("127.0.0.1:0", control)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		server.Shutdown(context.Background())
		if err := <-done; err != http.ErrServerClosed {
			t.Error(err)
		}
	}()
	origin := "http://" + listener.Addr().String()
	req, _ := http.NewRequest(http.MethodPost, origin+"/api/dashboard/getOverview", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var overview struct {
		Connected bool  `json:"connected"`
		Apps      []any `json:"apps"`
		Services  []any `json:"services"`
	}
	if err := json.NewDecoder(response.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !overview.Connected || overview.Apps == nil || overview.Services == nil {
		t.Fatalf("status=%d overview=%+v", response.StatusCode, overview)
	}
	if len(calls) != 3 {
		t.Fatalf("control calls=%d, want ping/apps/services", len(calls))
	}
}

func TestPasswordHashPromptConfirmationAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, password, confirmation string
		valid                        bool
	}{
		{"short", "short", "short", false},
		{"mismatch", "twelve-bytes-long", "different-password", false},
		{"too-long", strings.Repeat("x", 1025), "", false},
		{"valid", "a-long-password", "a-long-password", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := []byte(tc.password), []byte(tc.confirmation)
			calls := 0
			hash, err := passwordHashFromPrompt(func(string) ([]byte, error) {
				calls++
				if calls == 1 {
					return first, nil
				}
				return second, nil
			})
			if tc.valid {
				if err != nil || !strings.HasPrefix(hash, "scrypt$32768$8$1$") {
					t.Fatalf("hash=%q err=%v", hash, err)
				}
			} else if err == nil || hash != "" {
				t.Fatalf("invalid input produced hash=%q err=%v", hash, err)
			}
			for _, value := range first {
				if value != 0 {
					t.Fatal("password buffer was not cleared")
				}
			}
			if calls == 2 {
				for _, value := range second {
					if value != 0 {
						t.Fatal("confirmation buffer was not cleared")
					}
				}
			}
		})
	}
}
