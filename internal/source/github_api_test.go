package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubAPIListsPrivateAccessibleRepositories(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token-secret" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "harbor-user"})
			return
		}
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("visibility") != "all" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected repository request: %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"full_name": "org/private", "name": "private", "default_branch": "main", "private": true}})
	}))
	defer server.Close()
	api := NewGitHubAPI()
	api.BaseURL, api.Client = server.URL, server.Client()
	user, err := api.AuthenticatedUser(context.Background(), "test-token-secret")
	if err != nil || user.Login != "harbor-user" {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	repos, err := api.Repositories(context.Background(), "test-token-secret")
	if err != nil || len(repos) != 1 || !repos[0].Private || repos[0].URL != "https://github.com/org/private" {
		t.Fatalf("repos=%+v err=%v", repos, err)
	}
}

func TestGitHubCredentialStoreEncryptsTokenAndUsesPrivateFiles(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "harbor-user"})
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer apiServer.Close()
	api := NewGitHubAPI()
	api.BaseURL, api.Client = apiServer.URL, apiServer.Client()
	dir := filepath.Join(t.TempDir(), "harbor-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	credentials := NewGitHubCredentialStore(dir, api)
	connection, err := credentials.Connect(context.Background(), "ghp_a-test-token-that-is-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	if !connection.Connected || connection.Login != "harbor-user" {
		t.Fatalf("connection=%+v", connection)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("shared data directory mode changed: info=%v err=%v", info, err)
	}
	for _, name := range []string{githubTokenFile, githubKeyFile} {
		info, err := os.Stat(filepath.Join(credentials.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode=%o", name, info.Mode().Perm())
		}
	}
	data, err := os.ReadFile(filepath.Join(credentials.dir, githubTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghp_a-test-token-that-is-long-enough") {
		t.Fatal("token was written in plaintext")
	}
	stored, err := credentials.Token()
	if err != nil || stored != "ghp_a-test-token-that-is-long-enough" {
		t.Fatalf("token=%q err=%v", stored, err)
	}
	if err := credentials.Disconnect(); err != nil {
		t.Fatal(err)
	}
	meta, err := credentials.Metadata()
	if err != nil || meta.Connected {
		t.Fatalf("metadata=%+v err=%v", meta, err)
	}
}

func TestGitHubCredentialStoreRejectsInsecureKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	credentials := NewGitHubCredentialStore(dir, nil)
	if err := credentials.persist("test-token", GitHubConnection{Login: "harbor-user", ConnectedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(credentials.dir, githubKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(); err == nil {
		t.Fatal("expected unsafe key permissions to fail")
	}
}
