package source

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	githubTokenFile = "github-connection.json"
	githubKeyFile   = "github-connection.key"
	githubAAD       = "harbor-github-token-v1"
)

type GitHubConnection struct {
	Connected   bool      `json:"connected"`
	Login       string    `json:"login,omitempty"`
	ConnectedAt time.Time `json:"connectedAt,omitempty"`
}

type encryptedGitHubCredential struct {
	Login       string    `json:"login"`
	ConnectedAt time.Time `json:"connectedAt"`
	Nonce       string    `json:"nonce"`
	Ciphertext  string    `json:"ciphertext"`
}

// GitHubCredentialStore keeps the PAT encrypted on disk and returns only safe
// account metadata to callers. The encryption key is stored in a separate
// 0600 file inside the private Harbor data directory.
type GitHubCredentialStore struct {
	dir string
	api *GitHubAPI
	mu  sync.Mutex
}

func NewGitHubCredentialStore(dataDir string, api *GitHubAPI) *GitHubCredentialStore {
	if api == nil {
		api = NewGitHubAPI()
	}
	dir := ""
	if dataDir != "" {
		dir = filepath.Join(dataDir, "github")
	}
	return &GitHubCredentialStore{dir: dir, api: api}
}

func (s *GitHubCredentialStore) Connect(ctx context.Context, token string) (GitHubConnection, error) {
	token = strings.TrimSpace(token)
	if len(token) < 10 || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
		return GitHubConnection{}, errors.New("enter a valid GitHub personal access token")
	}
	user, err := s.api.AuthenticatedUser(ctx, token)
	if err != nil {
		return GitHubConnection{}, err
	}
	// Verify repository metadata access before replacing an existing token.
	if _, err := s.api.Repositories(ctx, token); err != nil {
		return GitHubConnection{}, err
	}
	connection := GitHubConnection{Connected: true, Login: user.Login, ConnectedAt: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(token, connection); err != nil {
		return GitHubConnection{}, err
	}
	return connection, nil
}

func (s *GitHubCredentialStore) Metadata() (GitHubConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		return GitHubConnection{}, nil
	}
	if err != nil {
		return GitHubConnection{}, err
	}
	return GitHubConnection{Connected: true, Login: credential.Login, ConnectedAt: credential.ConnectedAt}, nil
}

func (s *GitHubCredentialStore) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return credential.token, nil
}

func (s *GitHubCredentialStore) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures []error
	for _, name := range []string{githubTokenFile, githubKeyFile} {
		path := filepath.Join(s.dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

type storedCredential struct {
	Login       string
	ConnectedAt time.Time
	token       string
}

func (s *GitHubCredentialStore) persist(token string, connection GitHubConnection) error {
	if err := s.ensureDirectory(); err != nil {
		return err
	}
	key, err := s.loadOrCreateKey()
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return errors.New("could not initialize GitHub credential encryption")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return errors.New("could not initialize GitHub credential encryption")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return errors.New("could not encrypt GitHub credential")
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(token), []byte(githubAAD))
	data, err := json.Marshal(encryptedGitHubCredential{
		Login: connection.Login, ConnectedAt: connection.ConnectedAt,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		return errors.New("could not encode GitHub credential")
	}
	return writePrivateAtomic(filepath.Join(s.dir, githubTokenFile), data)
}

func (s *GitHubCredentialStore) read() (storedCredential, error) {
	path := filepath.Join(s.dir, githubTokenFile)
	info, err := os.Lstat(path)
	if err != nil {
		return storedCredential{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 16<<10 {
		return storedCredential{}, errors.New("GitHub credential file permissions or size are unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return storedCredential{}, errors.New("could not read GitHub credential")
	}
	var record encryptedGitHubCredential
	if err := json.Unmarshal(data, &record); err != nil || !validGitHubLogin(record.Login) || record.ConnectedAt.IsZero() {
		return storedCredential{}, errors.New("stored GitHub credential is invalid")
	}
	key, err := os.ReadFile(filepath.Join(s.dir, githubKeyFile))
	if err != nil || len(key) != 32 {
		return storedCredential{}, errors.New("GitHub credential key is unavailable")
	}
	keyInfo, err := os.Lstat(filepath.Join(s.dir, githubKeyFile))
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o077 != 0 {
		return storedCredential{}, errors.New("GitHub credential key permissions are unsafe")
	}
	nonce, err := base64.RawStdEncoding.DecodeString(record.Nonce)
	if err != nil {
		return storedCredential{}, errors.New("stored GitHub credential is invalid")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(record.Ciphertext)
	if err != nil {
		return storedCredential{}, errors.New("stored GitHub credential is invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return storedCredential{}, errors.New("stored GitHub credential is invalid")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return storedCredential{}, errors.New("stored GitHub credential is invalid")
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(githubAAD))
	if err != nil || len(plaintext) == 0 || len(plaintext) > 4096 || strings.ContainsAny(string(plaintext), "\r\n\x00") {
		return storedCredential{}, errors.New("stored GitHub credential is invalid")
	}
	return storedCredential{Login: record.Login, ConnectedAt: record.ConnectedAt.UTC(), token: string(plaintext)}, nil
}

func (s *GitHubCredentialStore) ensureDirectory() error {
	if s.dir == "" {
		return errors.New("Harbor data directory is unavailable")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return errors.New("could not secure GitHub credential storage")
	}
	info, err := os.Lstat(s.dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("GitHub credential directory is unsafe")
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return errors.New("could not secure GitHub credential storage")
	}
	return nil
}

func (s *GitHubCredentialStore) loadOrCreateKey() ([]byte, error) {
	path := filepath.Join(s.dir, githubKeyFile)
	key, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || len(key) != 32 {
			return nil, errors.New("GitHub credential key permissions are unsafe")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("could not read GitHub credential key")
	}
	key = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, errors.New("could not generate GitHub credential key")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.loadOrCreateKey()
		}
		return nil, errors.New("could not secure GitHub credential key")
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, errors.New("could not write GitHub credential key")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, errors.New("could not sync GitHub credential key")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, errors.New("could not close GitHub credential key")
	}
	return key, nil
}

func writePrivateAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".github-credential-*")
	if err != nil {
		return errors.New("could not save GitHub credential")
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("could not secure GitHub credential")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return errors.New("could not save GitHub credential")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("could not sync GitHub credential")
	}
	if err := file.Close(); err != nil {
		return errors.New("could not close GitHub credential")
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("could not replace GitHub credential: %w", err)
	}
	return nil
}
