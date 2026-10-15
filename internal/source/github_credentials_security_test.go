package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGitHubCredentialMetadataNeverContainsPAT(t *testing.T) {
	const token = "ghp_security_metadata_token_0123456789"
	store := newConnectedSecurityCredential(t, token)

	data, err := os.ReadFile(filepath.Join(store.dir, githubTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("encrypted credential metadata contains the personal access token")
	}
	var record encryptedGitHubCredential
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("credential metadata is not valid JSON: %v", err)
	}
	if record.Login != "harbor-user" || record.Ciphertext == "" || record.Nonce == "" {
		t.Fatalf("credential metadata is incomplete: %+v", record)
	}

	metadata, err := store.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.Connected || metadata.Login != "harbor-user" {
		t.Fatalf("safe connection metadata = %+v", metadata)
	}
}

func TestGitHubCredentialReconnectFailurePreservesPriorCredential(t *testing.T) {
	const (
		oldToken = "ghp_previous_credential_0123456789"
		newToken = "ghp_replacement_credential_0123456789"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "harbor-user"})
			return
		}
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer "+newToken {
			http.Error(w, "replacement token lacks repository access", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	t.Cleanup(server.Close)

	api := NewGitHubAPI()
	api.BaseURL, api.Client = server.URL, server.Client()
	store := NewGitHubCredentialStore(filepath.Join(t.TempDir(), "harbor-data"), api)
	if _, err := store.Connect(context.Background(), oldToken); err != nil {
		t.Fatalf("initial connect: %v", err)
	}

	credentialPath := filepath.Join(store.dir, githubTokenFile)
	before, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Connect(context.Background(), newToken); err == nil {
		t.Fatal("expected reconnect to fail when repository access is denied")
	}
	after, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed reconnect changed the previously stored credential")
	}
	stored, err := store.Token()
	if err != nil {
		t.Fatal(err)
	}
	if stored != oldToken {
		t.Fatal("failed reconnect did not preserve the prior credential")
	}
}

func TestGitHubCredentialStoreFailsClosedOnCorruptCiphertext(t *testing.T) {
	store := newConnectedSecurityCredential(t, "ghp_corruption_test_token_0123456789")
	credentialPath := filepath.Join(store.dir, githubTokenFile)
	data, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	var record encryptedGitHubCredential
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Ciphertext = base64.RawStdEncoding.EncodeToString([]byte("tampered ciphertext"))
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if token, err := store.Token(); err == nil || token != "" {
		t.Fatalf("corrupt credential returned token %q with error %v", token, err)
	}
	if metadata, err := store.Metadata(); err == nil || metadata.Connected {
		t.Fatalf("corrupt credential returned metadata %+v with error %v", metadata, err)
	}
}

func TestGitHubCredentialStoreFailsClosedOnInsecureCredentialPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced consistently on Windows")
	}
	store := newConnectedSecurityCredential(t, "ghp_insecure_mode_test_token_0123456789")
	credentialPath := filepath.Join(store.dir, githubTokenFile)
	if err := os.Chmod(credentialPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if token, err := store.Token(); err == nil || token != "" {
		t.Fatalf("insecure credential file returned token %q with error %v", token, err)
	}
	if metadata, err := store.Metadata(); err == nil || metadata.Connected {
		t.Fatalf("insecure credential file returned metadata %+v with error %v", metadata, err)
	}
}

func TestGitHubCredentialDisconnectRemovesStoredFiles(t *testing.T) {
	store := newConnectedSecurityCredential(t, "ghp_disconnect_test_token_0123456789")
	if err := store.Disconnect(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{githubTokenFile, githubKeyFile} {
		if _, err := os.Lstat(filepath.Join(store.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists or could not be checked: %v", name, err)
		}
	}
	if err := store.Disconnect(); err != nil {
		t.Fatalf("disconnect should be idempotent: %v", err)
	}
}

func TestGitHubCredentialStoreWithEmptyDataDirectoryNeverUsesWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	for _, name := range []string{githubTokenFile, githubKeyFile} {
		if err := os.WriteFile(filepath.Join(workingDir, name), []byte("leave me alone"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	store := NewGitHubCredentialStore("", nil)
	if _, err := store.Token(); err == nil {
		t.Fatal("Token unexpectedly read credentials from the working directory")
	}
	if err := store.Disconnect(); err == nil {
		t.Fatal("Disconnect unexpectedly succeeded without a data directory")
	}
	for _, name := range []string{githubTokenFile, githubKeyFile} {
		if contents, err := os.ReadFile(name); err != nil || string(contents) != "leave me alone" {
			t.Fatalf("working-directory file %q was changed: contents=%q err=%v", name, contents, err)
		}
	}
}

func TestGitHubCredentialStorePreservesSharedDataDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits are not enforced consistently on Windows")
	}
	dataDir := filepath.Join(t.TempDir(), "harbor-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(dataDir, "runtime-artifact.txt")
	if err := os.WriteFile(artifactPath, []byte("runtime artifact"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "harbor-user"})
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	t.Cleanup(server.Close)
	api := NewGitHubAPI()
	api.BaseURL, api.Client = server.URL, server.Client()
	store := NewGitHubCredentialStore(dataDir, api)
	if _, err := store.Connect(context.Background(), "ghp_parent_directory_test_token_0123456789"); err != nil {
		t.Fatalf("connect credential: %v", err)
	}

	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("shared data directory mode = %04o, want 0755", info.Mode().Perm())
	}
	contents, err := os.ReadFile(artifactPath)
	if err != nil || string(contents) != "runtime artifact" {
		t.Fatalf("shared runtime artifact contents = %q, err = %v", contents, err)
	}
}

func TestGitHubAPIDoesNotForwardCredentialAcrossRedirect(t *testing.T) {
	const token = "ghp_redirect_test_token_0123456789"
	targetRequests := 0
	targetAuthorization := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		targetAuthorization = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "unexpected"})
	}))
	t.Cleanup(target.Close)

	firstRequests := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("first API request authorization = %q", r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, target.URL+"/user", http.StatusFound)
	}))
	t.Cleanup(first.Close)

	api := NewGitHubAPI()
	api.BaseURL = first.URL // Keep the production client's no-follow redirect policy.
	if _, err := api.AuthenticatedUser(context.Background(), token); err == nil {
		t.Fatal("expected a redirect response to be rejected")
	}
	if firstRequests != 1 {
		t.Fatalf("first endpoint requests = %d, want 1", firstRequests)
	}
	if targetRequests != 0 || targetAuthorization != "" {
		t.Fatalf("redirect target received %d requests and authorization %q", targetRequests, targetAuthorization)
	}
}

func newConnectedSecurityCredential(t *testing.T, token string) *GitHubCredentialStore {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "harbor-user"})
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	t.Cleanup(server.Close)

	api := NewGitHubAPI()
	api.BaseURL, api.Client = server.URL, server.Client()
	store := NewGitHubCredentialStore(filepath.Join(t.TempDir(), "harbor-data"), api)
	if _, err := store.Connect(context.Background(), token); err != nil {
		t.Fatalf("connect test credential: %v", err)
	}
	return store
}
