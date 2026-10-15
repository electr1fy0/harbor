package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	frontend "paas/web"
)

const (
	loginBodyLimit     = 4096
	dashboardBodyLimit = 1 << 20
	maxHashingLogins   = 2
)

var dashboardFunctionName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

type attemptLimiter struct {
	mu       sync.Mutex
	attempts int
	resetAt  time.Time
}

func (l *attemptLimiter) consume(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.resetAt) {
		l.attempts = 0
		l.resetAt = now.Add(LoginWindow)
	}
	if l.attempts >= LoginAttemptLimit {
		seconds := int(l.resetAt.Sub(now).Seconds())
		if l.resetAt.Sub(now)%time.Second != 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		return false, seconds
	}
	l.attempts++
	return true, 0
}

func (l *attemptLimiter) reset() {
	l.mu.Lock()
	l.attempts = 0
	l.resetAt = time.Time{}
	l.mu.Unlock()
}

type handler struct {
	api       http.Handler
	hooks     http.Handler
	assets    fs.FS
	static    http.Handler
	config    Config
	limiter   attemptLimiter
	hashSlots chan struct{}
	now       func() time.Time
}

// NewHandlerWithHooks exposes credential-authenticated deployment callbacks
// under the same trusted dashboard host, without requiring a browser session.
func NewHandlerWithHooks(api http.Handler, assets fs.FS, config Config, hooks http.Handler) (http.Handler, error) {
	h, err := NewHandler(api, assets, config)
	if err != nil {
		return nil, err
	}
	h.(*handler).hooks = hooks
	return h, nil
}

// NewHandler wraps the dashboard API and embedded frontend in host, origin,
// and session checks. assets contains the embedded browser assets.
func NewHandler(api http.Handler, assets fs.FS, config Config) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("dashboard API handler is required")
	}
	if assets == nil {
		return nil, errors.New("dashboard frontend assets are required")
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	for _, name := range []string{"assets/app.js", "assets/app.css"} {
		info, err := fs.Stat(assets, name)
		if err != nil || info.IsDir() {
			return nil, errors.New("dashboard browser asset is missing: " + name)
		}
	}
	return &handler{
		api:       api,
		assets:    assets,
		static:    http.FileServer(http.FS(assets)),
		config:    config,
		hashSlots: make(chan struct{}, maxHashingLogins),
		now:       time.Now,
	}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if !requestHasTrustedHost(r, h.config) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if h.config.Mode == ModeLocal && !remotePeerIsLoopback(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/hooks/") && h.hooks != nil {
		h.hooks.ServeHTTP(w, r)
		return
	}

	switch r.URL.Path {
	case "/api/auth/status":
		h.authStatus(w, r)
		return
	case "/api/auth/login":
		h.login(w, r)
		return
	case "/api/auth/logout":
		h.logout(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/dashboard/") {
		h.dashboardAPI(w, r)
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.serveFrontend(w, r)
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
}

func (h *handler) authStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":          h.config.Mode,
		"authenticated": h.authenticated(r),
	})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !requestHasTrustedOrigin(r, h.config) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	password, status, ok := readLoginBody(w, r)
	if !ok {
		writeError(w, status, "Invalid sign-in request.")
		return
	}
	if h.config.Mode != ModePublic {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Sign-in is unavailable."})
		return
	}
	select {
	case h.hashSlots <- struct{}{}:
		defer func() { <-h.hashSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "Sign-in is busy. Try again shortly.")
		return
	}

	allowed, retryAfter := h.limiter.consume(h.now())
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		minutes := (retryAfter + 59) / 60
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "Too many sign-in attempts. Try again in " + strconv.Itoa(minutes) + " minutes.",
		})
		return
	}

	verified, err := verifyPassword(password, h.config.PasswordHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sign-in failed")
		return
	}
	if !verified {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "The password is incorrect."})
		return
	}
	h.limiter.reset()
	token, err := createSessionToken(h.config.SessionSecret, h.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sign-in failed")
		return
	}
	h.setSessionCookie(w, token, SessionMaxAge)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func readLoginBody(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	if r.ContentLength > loginBodyLimit {
		return "", http.StatusRequestEntityTooLarge, false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", http.StatusUnsupportedMediaType, false
	}
	limited := http.MaxBytesReader(w, r.Body, loginBodyLimit)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var body struct {
		Password string `json:"password"`
	}
	if err := decoder.Decode(&body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return "", http.StatusRequestEntityTooLarge, false
		}
		return "", http.StatusBadRequest, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return "", http.StatusRequestEntityTooLarge, false
		}
		return "", http.StatusBadRequest, false
	}
	if body.Password == "" || len([]byte(body.Password)) > maxPasswordByteCount {
		return "", http.StatusBadRequest, false
	}
	return body.Password, http.StatusOK, true
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !requestHasTrustedOrigin(r, h.config) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := validateLogoutBody(w, r); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	h.setExpiredSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func validateLogoutBody(w http.ResponseWriter, r *http.Request) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	if r.ContentLength > loginBodyLimit {
		return errors.New("request body too large")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, loginBodyLimit))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload map[string]json.RawMessage
	if err := decoder.Decode(&payload); err != nil || payload == nil || len(payload) != 0 {
		return errors.New("logout body must be an empty object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("logout body has trailing data")
	}
	return nil
}

func (h *handler) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	function := strings.TrimPrefix(r.URL.Path, "/api/dashboard/")
	if !dashboardFunctionName.MatchString(function) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !requestHasTrustedOrigin(r, h.config) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !h.authenticated(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.ContentLength > dashboardBodyLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, dashboardBodyLimit)
	h.api.ServeHTTP(w, r)
}

func (h *handler) authenticated(r *http.Request) bool {
	if h.config.Mode == ModeLocal {
		return true
	}
	return verifySessionToken(sessionCookieValue(r), h.config.SessionSecret, h.now())
}

func (h *handler) setSessionCookie(w http.ResponseWriter, value string, age time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(age / time.Second),
	})
}

func (h *handler) setExpiredSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0).UTC(),
	})
}

func (h *handler) serveFrontend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}
	requestPath := r.URL.Path
	if requestPath == "/" || requestPath == "/index.html" {
		h.servePage(w, r, requestPath)
		return
	}
	if !strings.HasPrefix(requestPath, "/") || strings.ContainsAny(requestPath, "\\\x00") {
		http.NotFound(w, r)
		return
	}
	assetName := strings.TrimPrefix(requestPath, "/")
	if !fs.ValidPath(assetName) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(h.assets, assetName)
	if err == nil && !info.IsDir() {
		if assetName == "index.html" {
			h.servePage(w, r, requestPath)
			return
		}
		if strings.HasPrefix(assetName, "assets/") {
			w.Header().Set("Cache-Control", "no-cache")
			if r.URL.Query().Get("v") == assetVersion(h.assets, assetName) {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		copyRequest := r.Clone(r.Context())
		copyURL := *r.URL
		copyURL.Path = "/" + path.Clean(assetName)
		copyURL.RawPath = ""
		copyRequest.URL = &copyURL
		h.static.ServeHTTP(w, copyRequest)
		return
	}
	if isPageRoute(requestPath) {
		h.servePage(w, r, requestPath)
		return
	}
	http.NotFound(w, r)
}

func (h *handler) servePage(w http.ResponseWriter, r *http.Request, requestPath string) {
	if h.config.Mode == ModePublic && requestPath != "/login" && !h.authenticated(r) {
		w.Header().Set("Location", "/login")
		w.WriteHeader(http.StatusFound)
		return
	}
	h.renderPage(w, r)
}

func (h *handler) renderPage(w http.ResponseWriter, r *http.Request) {
	page := h.frontendPage(r)
	var content bytes.Buffer
	if err := frontend.Dashboard(page).Render(r.Context(), &content); err != nil {
		writeError(w, http.StatusInternalServerError, "Dashboard could not be rendered.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if page.Status != http.StatusOK {
		w.WriteHeader(page.Status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(content.Bytes())
		}
		return
	}
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(content.Bytes()))
}

func isPageRoute(requestPath string) bool {
	switch requestPath {
	case "/login", "/capacity", "/deployments", "/new", "/services", "/settings":
		return true
	}
	if !strings.HasPrefix(requestPath, "/projects/") {
		return false
	}
	name := strings.TrimPrefix(requestPath, "/projects/")
	return name != "" && !strings.Contains(name, "/") && fs.ValidPath(name)
}

func requestHasTrustedHost(r *http.Request, config Config) bool {
	authority, err := canonicalAuthority(r.Host, "http")
	if err != nil {
		return false
	}
	switch config.Mode {
	case ModeLocal:
		parsed, err := url.Parse("http://" + r.Host)
		if err != nil || parsed == nil {
			return false
		}
		hostname := strings.ToLower(parsed.Hostname())
		return hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
	case ModePublic:
		origin, err := parsePublicOrigin(config.Origin)
		if err != nil {
			return false
		}
		trustedURL, _ := url.Parse(origin)
		trustedAuthority, err := canonicalAuthority(trustedURL.Host, "https")
		return err == nil && authority == trustedAuthority
	default:
		return false
	}
}

func requestHasTrustedOrigin(r *http.Request, config Config) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] == "" || !requestHasTrustedHost(r, config) {
		return false
	}
	origin := origins[0]
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	var expected string
	if config.Mode == ModePublic {
		expected = config.Origin
	} else if config.Mode == ModeLocal {
		authority, err := canonicalAuthority(r.Host, scheme)
		if err != nil {
			return false
		}
		expected = scheme + "://" + authority
	} else {
		return false
	}
	if origin != expected {
		return false
	}
	fetchSites := r.Header.Values("Sec-Fetch-Site")
	return len(fetchSites) == 0 || (len(fetchSites) == 1 && (fetchSites[0] == "" || fetchSites[0] == "same-origin"))
}

func remotePeerIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
