package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

const (
	SessionCookieName    = "__Host-harbor_session"
	SessionMaxAge        = 8 * time.Hour
	LoginWindow          = 15 * time.Minute
	LoginAttemptLimit    = 5
	ScryptN              = 1 << 15
	ScryptR              = 8
	ScryptP              = 1
	passwordHashPrefix   = "scrypt$32768$8$1$"
	maxPasswordByteCount = 1024
)

var (
	hexDigestPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	hostNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)
)

// Mode describes whether the dashboard is restricted to a local operator or
// protected by the public password login.
type Mode string

const (
	ModeLocal  Mode = "local"
	ModePublic Mode = "public"
)

// Config contains the settings used by the dashboard HTTP wrapper. Public
// configurations use the same encoded scrypt hash and session secret format
// as the former web server implementation.
type Config struct {
	Mode          Mode
	Origin        string
	PasswordHash  string
	SessionSecret []byte
}

// ConfigFromEnv reads the dashboard's authentication settings. Any partial or
// malformed public configuration is an error so callers can fail closed.
func ConfigFromEnv() (Config, error) {
	rawOrigin := strings.TrimSpace(os.Getenv("PAASD_PUBLIC_ORIGIN"))
	rawPasswordHash := strings.TrimSpace(os.Getenv("PAASD_ADMIN_PASSWORD_HASH"))
	rawSessionSecret := strings.TrimSpace(os.Getenv("PAASD_SESSION_SECRET"))
	if rawOrigin == "" && rawPasswordHash == "" && rawSessionSecret == "" {
		return Config{Mode: ModeLocal}, nil
	}

	canonicalOrigin, err := parsePublicOrigin(rawOrigin)
	if err != nil {
		return Config{}, fmt.Errorf("invalid PAASD_PUBLIC_ORIGIN: %w", err)
	}
	if !validPasswordHash(rawPasswordHash) {
		return Config{}, errors.New("PAASD_ADMIN_PASSWORD_HASH must be a scrypt$32768$8$1$ hash with a 32-byte salt and digest")
	}
	if !hexDigestPattern.MatchString(rawSessionSecret) {
		return Config{}, errors.New("PAASD_SESSION_SECRET must be 32 bytes encoded as 64 hexadecimal characters")
	}
	secret, err := hex.DecodeString(rawSessionSecret)
	if err != nil {
		return Config{}, errors.New("PAASD_SESSION_SECRET must be hexadecimal")
	}
	return Config{
		Mode:          ModePublic,
		Origin:        canonicalOrigin,
		PasswordHash:  rawPasswordHash,
		SessionSecret: secret,
	}, nil
}

func validateConfig(config Config) error {
	switch config.Mode {
	case ModeLocal:
		if config.Origin != "" || config.PasswordHash != "" || len(config.SessionSecret) != 0 {
			return errors.New("local dashboard configuration cannot contain public authentication settings")
		}
		return nil
	case ModePublic:
		origin, err := parsePublicOrigin(config.Origin)
		if err != nil || origin != config.Origin {
			return errors.New("public dashboard configuration has an invalid origin")
		}
		if !validPasswordHash(config.PasswordHash) {
			return errors.New("public dashboard configuration has an invalid password hash")
		}
		if len(config.SessionSecret) != 32 {
			return errors.New("public dashboard configuration requires a 32-byte session secret")
		}
		return nil
	default:
		return errors.New("dashboard configuration mode must be local or public")
	}
}

func parsePublicOrigin(value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return "", errors.New("must be a canonical HTTPS origin")
	}
	u, err := url.Parse(value)
	if err != nil || u == nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be a canonical HTTPS origin without a path, credentials, query, or fragment")
	}
	authority, err := canonicalAuthority(u.Host, "https")
	if err != nil {
		return "", errors.New("must contain a valid host")
	}
	canonical := "https://" + authority
	if value != canonical {
		return "", errors.New("must be canonical, for example https://dashboard.example.com")
	}
	return canonical, nil
}

func canonicalAuthority(authority, scheme string) (string, error) {
	if authority == "" || len(authority) > 255 || strings.ContainsAny(authority, "\t\r\n ,/@\\%") {
		return "", errors.New("invalid host authority")
	}
	u, err := url.Parse("http://" + authority)
	if err != nil || u == nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return "", errors.New("invalid host authority")
	}
	hostname := strings.ToLower(u.Hostname())
	if net.ParseIP(hostname) == nil && !hostNamePattern.MatchString(hostname) {
		return "", errors.New("invalid hostname")
	}
	port := u.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", errors.New("invalid port")
		}
		if (scheme == "http" && portNumber == 80) || (scheme == "https" && portNumber == 443) {
			port = ""
		}
	}
	if ip := net.ParseIP(hostname); ip != nil && strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	if port != "" {
		hostname += ":" + port
	}
	return hostname, nil
}

func validPasswordHash(value string) bool {
	if !strings.HasPrefix(strings.ToLower(value), passwordHashPrefix) {
		return false
	}
	parts := strings.Split(value, "$")
	return len(parts) == 6 && strings.EqualFold(parts[0], "scrypt") && parts[1] == "32768" && parts[2] == "8" && parts[3] == "1" && hexDigestPattern.MatchString(parts[4]) && hexDigestPattern.MatchString(parts[5])
}

// GeneratePasswordHash creates a password hash compatible with the previous
// Node scrypt configuration. Passwords are limited to the same 1 KiB input
// bound used by the login endpoint.
func GeneratePasswordHash(password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	if len([]byte(password)) > maxPasswordByteCount {
		return "", fmt.Errorf("password must be at most %d UTF-8 bytes", maxPasswordByteCount)
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	digest, err := scrypt.Key([]byte(password), salt, ScryptN, ScryptR, ScryptP, 32)
	if err != nil {
		return "", fmt.Errorf("derive password hash: %w", err)
	}
	return fmt.Sprintf("%s%s$%s", passwordHashPrefix, hex.EncodeToString(salt), hex.EncodeToString(digest)), nil
}

func verifyPassword(password, encodedHash string) (bool, error) {
	if len([]byte(password)) > maxPasswordByteCount || !validPasswordHash(encodedHash) {
		return false, nil
	}
	parts := strings.Split(encodedHash, "$")
	salt, err := hex.DecodeString(parts[4])
	if err != nil {
		return false, nil
	}
	expected, err := hex.DecodeString(parts[5])
	if err != nil {
		return false, nil
	}
	actual, err := scrypt.Key([]byte(password), salt, ScryptN, ScryptR, ScryptP, 32)
	if err != nil {
		return false, err
	}
	return hmac.Equal(actual, expected), nil
}

type sessionPayload struct {
	Version   int    `json:"version"`
	IssuedAt  int64  `json:"issuedAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Nonce     string `json:"nonce"`
}

func createSessionToken(secret []byte, now time.Time) (string, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("generate session nonce: %w", err)
	}
	issuedAt := now.Unix()
	payload, err := json.Marshal(sessionPayload{
		Version:   1,
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt + int64(SessionMaxAge/time.Second),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes),
	})
	if err != nil {
		return "", fmt.Errorf("encode session: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(encodedPayload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encodedPayload + "." + signature, nil
}

func verifySessionToken(token string, secret []byte, now time.Time) bool {
	if token == "" || len(token) > 1024 {
		return false
	}
	pieces := strings.Split(token, ".")
	if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" {
		return false
	}
	receivedSignature, err := base64.RawURLEncoding.DecodeString(pieces[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(pieces[0]))
	if !hmac.Equal(receivedSignature, mac.Sum(nil)) {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(pieces[0])
	if err != nil {
		return false
	}
	var payload sessionPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return false
	}
	current := now.Unix()
	return payload.Version == 1 && len(payload.Nonce) >= 16 && payload.IssuedAt <= current+60 && payload.ExpiresAt > current && payload.ExpiresAt > payload.IssuedAt && payload.ExpiresAt-payload.IssuedAt <= int64(SessionMaxAge/time.Second)
}

func sessionCookieValue(r *http.Request) string {
	if len(r.Header.Get("Cookie")) > 8192 {
		return ""
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || len(cookie.Value) > 1024 {
		return ""
	}
	return cookie.Value
}
