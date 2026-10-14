package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const testPasswordHash = "scrypt$32768$8$1$" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + "$" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func testConfig(mode Mode) Config {
	if mode == ModeLocal {
		return Config{Mode: ModeLocal}
	}
	return Config{
		Mode:          ModePublic,
		Origin:        "https://dashboard.example.test",
		PasswordHash:  testPasswordHash,
		SessionSecret: []byte(strings.Repeat("s", 32)),
	}
}

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>Harbor</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('harbor')")},
		"favicon.svg":   &fstest.MapFile{Data: []byte("<svg></svg>")},
		"robots.txt":    &fstest.MapFile{Data: []byte("User-agent: *\nDisallow:\n")},
	}
}

func newTestHandler(t *testing.T, config Config, api http.Handler) http.Handler {
	t.Helper()
	if api == nil {
		api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		})
	}
	handler, err := NewHandler(api, testAssets(), config)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func request(handler http.Handler, method, target, host, remote string, headers map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = host
	r.RemoteAddr = remote
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestConfigFromEnvFailsClosedOnPartialPublicSettings(t *testing.T) {
	t.Setenv("PAASD_PUBLIC_ORIGIN", "")
	t.Setenv("PAASD_ADMIN_PASSWORD_HASH", "")
	t.Setenv("PAASD_SESSION_SECRET", "")
	config, err := ConfigFromEnv()
	if err != nil || config.Mode != ModeLocal {
		t.Fatalf("ConfigFromEnv() = (%+v, %v), want local config", config, err)
	}

	t.Setenv("PAASD_PUBLIC_ORIGIN", "https://dashboard.example.test")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv accepted partial public auth settings")
	}
	t.Setenv("PAASD_ADMIN_PASSWORD_HASH", testPasswordHash)
	t.Setenv("PAASD_SESSION_SECRET", strings.Repeat("0", 64))
	config, err = ConfigFromEnv()
	if err != nil || config.Mode != ModePublic || config.Origin != "https://dashboard.example.test" || len(config.SessionSecret) != 32 {
		t.Fatalf("ConfigFromEnv() = (%+v, %v), want valid public config", config, err)
	}
	t.Setenv("PAASD_PUBLIC_ORIGIN", "https://dashboard.example.test/")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv accepted a noncanonical public origin")
	}
}

func TestNewHandlerRequiresBuiltFrontendIndex(t *testing.T) {
	_, err := NewHandler(http.NotFoundHandler(), fstest.MapFS{
		".gitkeep": &fstest.MapFile{Data: []byte("compile scaffold")},
	}, testConfig(ModeLocal))
	if err == nil || !strings.Contains(err.Error(), "build web/dist") {
		t.Fatalf("NewHandler error = %v, want a clear frontend-build error", err)
	}
}

func TestLocalModeChecksHostAndPeerAndSetsSecurityHeaders(t *testing.T) {
	handler := newTestHandler(t, testConfig(ModeLocal), nil)
	response := request(handler, http.MethodGet, "http://localhost:5173/api/auth/status", "localhost:5173", "127.0.0.1:1234", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("local status returned %d, want 200", response.Code)
	}
	var status map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["mode"] != "local" || status["authenticated"] != true {
		t.Fatalf("unexpected local status: %v", status)
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing security headers: %v", response.Header())
	}

	for _, test := range []struct {
		name, host, remote string
	}{
		{name: "non-loopback peer", host: "localhost:5173", remote: "192.0.2.3:1234"},
		{name: "spoofed host", host: "attacker.example", remote: "127.0.0.1:1234"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := request(handler, http.MethodGet, "http://localhost:5173/", test.host, test.remote, map[string]string{"X-Forwarded-Host": "localhost:5173"}, "")
			if got.Code != http.StatusForbidden {
				t.Fatalf("request returned %d, want 403", got.Code)
			}
		})
	}
}

func TestPublicStaticRoutesAndMissingFiles(t *testing.T) {
	handler := newTestHandler(t, testConfig(ModePublic), nil)
	for _, test := range []struct {
		path, wantLocation string
		wantStatus         int
	}{
		{path: "/", wantStatus: http.StatusFound, wantLocation: "/login"},
		{path: "/projects/demo", wantStatus: http.StatusFound, wantLocation: "/login"},
		{path: "/projects/app.name", wantStatus: http.StatusFound, wantLocation: "/login"},
		{path: "/login", wantStatus: http.StatusOK},
		{path: "/assets/app.js", wantStatus: http.StatusOK},
		{path: "/assets/missing.js", wantStatus: http.StatusNotFound},
		{path: "/favicon.ico", wantStatus: http.StatusNotFound},
		{path: "/api/auth/unknown", wantStatus: http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			got := request(handler, http.MethodGet, "http://dashboard.example.test"+test.path, "dashboard.example.test", "203.0.113.7:5000", nil, "")
			if got.Code != test.wantStatus {
				t.Fatalf("request returned %d, want %d; body=%s", got.Code, test.wantStatus, got.Body.String())
			}
			if test.wantLocation != "" && got.Header().Get("Location") != test.wantLocation {
				t.Fatalf("Location = %q, want %q", got.Header().Get("Location"), test.wantLocation)
			}
			if test.path == "/login" && !strings.Contains(got.Body.String(), "Harbor") {
				t.Fatalf("login page did not serve index.html: %s", got.Body.String())
			}
			if strings.HasPrefix(test.path, "/assets/") && test.wantStatus == http.StatusOK && got.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
				t.Fatalf("hashed asset cache policy = %q", got.Header().Get("Cache-Control"))
			}
		})
	}
	spoofed := request(handler, http.MethodGet, "http://dashboard.example.test/login", "attacker.example", "203.0.113.7:5000", map[string]string{
		"X-Forwarded-Host": "dashboard.example.test",
	}, "")
	if spoofed.Code != http.StatusForbidden {
		t.Fatalf("spoofed Host returned %d, want 403", spoofed.Code)
	}

	traversal := request(handler, http.MethodGet, "http://dashboard.example.test/%2e%2e/index.html", "dashboard.example.test", "203.0.113.7:5000", nil, "")
	if traversal.Code != http.StatusNotFound {
		t.Fatalf("traversal request returned %d, want 404", traversal.Code)
	}
}

func TestPublicAuthLoginSessionDashboardAndLogout(t *testing.T) {
	password := "correct horse battery staple"
	passwordHash, err := GeneratePasswordHash(password)
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(ModePublic)
	config.PasswordHash = passwordHash
	called := false
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	handler := newTestHandler(t, config, api)
	postHeaders := map[string]string{
		"Content-Type":   "application/json",
		"Origin":         config.Origin,
		"Sec-Fetch-Site": "same-origin",
	}

	crossOrigin := request(handler, http.MethodPost, "http://dashboard.example.test/api/auth/login", "dashboard.example.test", "203.0.113.7:5000", map[string]string{
		"Content-Type": "application/json",
		"Origin":       "https://attacker.example",
	}, `{"password":"`+password+`"}`)
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login returned %d, want 403", crossOrigin.Code)
	}

	login := request(handler, http.MethodPost, "http://dashboard.example.test/api/auth/login", "dashboard.example.test", "203.0.113.7:5000", postHeaders, `{"password":"`+password+`"}`)
	if login.Code != http.StatusOK || !strings.Contains(login.Body.String(), `"ok":true`) {
		t.Fatalf("login returned %d %s", login.Code, login.Body.String())
	}
	setCookie := login.Header().Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, SessionCookieName+"=") || !strings.Contains(setCookie, "Path=/") || !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "Secure") || !strings.Contains(setCookie, "SameSite=Lax") || !strings.Contains(setCookie, "Max-Age=28800") {
		t.Fatalf("session cookie lacks required attributes: %q", setCookie)
	}
	cookie := strings.SplitN(setCookie, ";", 2)[0]
	token := strings.TrimPrefix(cookie, SessionCookieName+"=")
	if !verifySessionToken(token, config.SessionSecret, time.Now()) {
		t.Fatal("login issued a session token that does not verify")
	}

	status := request(handler, http.MethodGet, "http://dashboard.example.test/api/auth/status", "dashboard.example.test", "203.0.113.7:5000", map[string]string{"Cookie": cookie}, "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"authenticated":true`) {
		t.Fatalf("authenticated status returned %d %s", status.Code, status.Body.String())
	}
	dashboard := request(handler, http.MethodPost, "http://dashboard.example.test/api/dashboard/getOverview", "dashboard.example.test", "203.0.113.7:5000", map[string]string{
		"Cookie":         cookie,
		"Origin":         config.Origin,
		"Sec-Fetch-Site": "same-origin",
		"Content-Type":   "application/json",
	}, `{}`)
	if dashboard.Code != http.StatusOK || !called {
		t.Fatalf("authenticated dashboard returned %d, called=%v", dashboard.Code, called)
	}
	unauthorized := request(handler, http.MethodPost, "http://dashboard.example.test/api/dashboard/getOverview", "dashboard.example.test", "203.0.113.7:5000", postHeaders, `{}`)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous dashboard returned %d, want 401", unauthorized.Code)
	}

	logout := request(handler, http.MethodPost, "http://dashboard.example.test/api/auth/logout", "dashboard.example.test", "203.0.113.7:5000", map[string]string{
		"Cookie":         cookie,
		"Origin":         config.Origin,
		"Sec-Fetch-Site": "same-origin",
		"Content-Type":   "application/json",
	}, `{}`)
	if logout.Code != http.StatusOK || !strings.Contains(logout.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout returned %d and cookie %q", logout.Code, logout.Header().Get("Set-Cookie"))
	}
}

func TestLoginBodyAndDashboardBodyBounds(t *testing.T) {
	handler := newTestHandler(t, testConfig(ModePublic), nil)
	headers := map[string]string{"Content-Type": "application/json", "Origin": "https://dashboard.example.test"}
	oversizedLogin := request(handler, http.MethodPost, "http://dashboard.example.test/api/auth/login", "dashboard.example.test", "203.0.113.7:5000", headers, `{"password":"`+strings.Repeat("x", 5000)+`"}`)
	if oversizedLogin.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized login returned %d, want 413", oversizedLogin.Code)
	}

	local := newTestHandler(t, testConfig(ModeLocal), nil)
	oversizedDashboard := request(local, http.MethodPost, "http://localhost/api/dashboard/getOverview", "localhost", "127.0.0.1:5000", map[string]string{
		"Origin": "http://localhost",
	}, strings.Repeat("x", dashboardBodyLimit+1))
	if oversizedDashboard.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized dashboard returned %d, want 413", oversizedDashboard.Code)
	}
}

func TestLoginAttemptLimiterWindow(t *testing.T) {
	var limiter attemptLimiter
	now := time.Unix(1000, 0)
	for i := 0; i < LoginAttemptLimit; i++ {
		if allowed, _ := limiter.consume(now); !allowed {
			t.Fatalf("attempt %d was rejected unexpectedly", i+1)
		}
	}
	if allowed, retry := limiter.consume(now); allowed || retry != int(LoginWindow/time.Second) {
		t.Fatalf("limit result = (%v, %d), want (false, %d)", allowed, retry, int(LoginWindow/time.Second))
	}
	if allowed, _ := limiter.consume(now.Add(LoginWindow + time.Second)); !allowed {
		t.Fatal("limiter did not reset at window expiration")
	}
}

func TestSessionTokenTamperingAndExpiry(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	issued := time.Unix(1_800_000_000, 0)
	token, err := createSessionToken(secret, issued)
	if err != nil {
		t.Fatal(err)
	}
	if !verifySessionToken(token, secret, issued.Add(time.Minute)) {
		t.Fatal("fresh session token did not verify")
	}
	if verifySessionToken(token+"x", secret, issued.Add(time.Minute)) {
		t.Fatal("tampered session token verified")
	}
	if verifySessionToken(token, secret, issued.Add(SessionMaxAge+time.Second)) {
		t.Fatal("expired session token verified")
	}
}

func TestLoginConcurrencyLimitRejectsBeforeHashing(t *testing.T) {
	wrapped := newTestHandler(t, testConfig(ModePublic), nil)
	h := wrapped.(*handler)
	for i := 0; i < maxHashingLogins; i++ {
		h.hashSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxHashingLogins; i++ {
			<-h.hashSlots
		}
	}()
	response := request(h, http.MethodPost, "http://dashboard.example.test/api/auth/login", "dashboard.example.test", "127.0.0.1:1234", map[string]string{"Content-Type": "application/json", "Origin": h.config.Origin}, `{"password":"test-password"}`)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("concurrent hash limit returned %d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
	if h.limiter.attempts != 0 {
		t.Fatal("busy hashing slot consumed a password attempt")
	}
}
