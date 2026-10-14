package deployment

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"paas/internal/config"
	"paas/internal/model"
	managedservice "paas/internal/service"
)

var environmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const maxRedactionHistoryBytes = 1 << 20

// ConfigureOptions contains optional runtime settings changes.
type ConfigureOptions struct {
	Environment       map[string]string
	RemoveEnvironment []string
	MemoryLimit       *string
	CPUQuota          *float64
	TasksLimit        *int
}

type redactedError struct {
	message string
	cause   error
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.cause }

// Settings returns configuration metadata. Environment values are decrypted
// only to derive key names and are never included in the returned model.
func (s *Service) Settings(name string) (*model.Settings, error) {
	stored, err := s.db.AppSettings(name)
	if err != nil {
		return nil, err
	}
	environment, err := s.decryptEnvironment(stored.AppID, stored.EncryptedEnvironment)
	if err != nil {
		return nil, err
	}
	result := &model.Settings{
		EnvironmentKeys: sortedKeys(environment),
		MemoryLimit:     stored.MemoryLimit,
		CPUQuota:        stored.CPUQuota,
		TasksLimit:      stored.TasksLimit,
	}
	if result.MemoryLimit == "" || result.CPUQuota <= 0 || result.TasksLimit <= 0 {
		if latest, latestErr := s.db.LatestDeployment(name); latestErr == nil {
			if result.MemoryLimit == "" {
				result.MemoryLimit = latest.MemoryLimit
			}
			if result.CPUQuota <= 0 {
				result.CPUQuota = latest.CPUQuota
			}
			if result.TasksLimit <= 0 {
				result.TasksLimit = latest.TasksLimit
			}
		}
	}
	if result.MemoryLimit == "" {
		result.MemoryLimit = config.DefaultMemory
	}
	if result.CPUQuota <= 0 {
		result.CPUQuota = config.DefaultCPU
	}
	if result.TasksLimit <= 0 {
		result.TasksLimit = config.DefaultTasks
	}
	return result, nil
}

// Configure updates persisted application settings and returns metadata only.
func (s *Service) Configure(ctx context.Context, name string, change ConfigureOptions) (*model.Settings, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.ctx.Err() != nil {
		return nil, errors.New("daemon shutting down")
	}
	if s.stopping[name] > 0 {
		return nil, fmt.Errorf("application %s is stopping", name)
	}
	app, err := s.db.CreateApp(name)
	if err != nil {
		return nil, err
	}
	stored, err := s.db.AppSettings(name)
	if err != nil {
		return nil, err
	}
	environment, err := s.decryptEnvironment(app.ID, stored.EncryptedEnvironment)
	if err != nil {
		return nil, err
	}
	originalEnvironment := maps.Clone(environment)
	history, err := s.decryptRedactionHistory(app.ID, stored.EncryptedRedactionHistory)
	if err != nil {
		return nil, err
	}
	originalHistory := append([]string{}, history...)
	sort.Strings(originalHistory)
	historySet := make(map[string]bool, len(history)+len(environment)+len(change.Environment))
	for _, value := range history {
		if value != "" {
			historySet[value] = true
		}
	}
	for _, value := range environment {
		if value != "" {
			historySet[value] = true
		}
	}
	bindings, err := s.db.GetBindingsForApp(app.ID)
	if err != nil {
		return nil, err
	}
	reserved := map[string]bool{"PORT": true, "PATH": true, "HOME": true, "DATABASE_URL": true}
	for _, binding := range bindings {
		reserved[binding.EnvKey] = true
	}
	for key, value := range change.Environment {
		if !environmentKeyPattern.MatchString(key) || reserved[key] || strings.HasPrefix(key, "PAAS_") {
			return nil, fmt.Errorf("invalid or reserved environment key %q", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment value for %q contains a NUL byte", key)
		}
		if value != "" {
			historySet[value] = true
		}
		environment[key] = value
	}
	for _, key := range change.RemoveEnvironment {
		if !environmentKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid environment key %q", key)
		}
		delete(environment, key)
	}

	if change.MemoryLimit != nil {
		if _, err := config.ParseMemoryBytes(*change.MemoryLimit); err != nil {
			return nil, fmt.Errorf("invalid memory limit: %w", err)
		}
		stored.MemoryLimit = *change.MemoryLimit
	}
	if change.CPUQuota != nil {
		if *change.CPUQuota <= 0 || math.IsNaN(*change.CPUQuota) || math.IsInf(*change.CPUQuota, 0) {
			return nil, errors.New("CPU quota must be a finite positive number")
		}
		stored.CPUQuota = *change.CPUQuota
	}
	if change.TasksLimit != nil {
		if *change.TasksLimit < 1 {
			return nil, errors.New("tasks limit must be a positive integer")
		}
		stored.TasksLimit = *change.TasksLimit
	}
	if !maps.Equal(originalEnvironment, environment) {
		stored.EncryptedEnvironment, err = s.encryptEnvironment(app.ID, environment)
		if err != nil {
			return nil, err
		}
	}
	history = make([]string, 0, len(historySet))
	for value := range historySet {
		history = append(history, value)
	}
	sort.Strings(history)
	historyData, err := json.Marshal(history)
	if err != nil {
		return nil, errors.New("encode environment redaction history")
	}
	if len(historyData) > maxRedactionHistoryBytes {
		return nil, errors.New("environment redaction history limit reached")
	}
	if !slices.Equal(originalHistory, history) {
		stored.EncryptedRedactionHistory, err = s.encryptData(app.ID, "history", historyData)
		if err != nil {
			return nil, err
		}
	}
	if err := s.db.UpdateAppSettings(name, *stored); err != nil {
		return nil, err
	}
	return s.Settings(name)
}

func (s *Service) runtimeEnvironment(appID string) (map[string]string, error) {
	stored, err := s.db.AppSettingsByID(appID)
	if err != nil {
		return nil, err
	}
	return s.decryptEnvironment(appID, stored.EncryptedEnvironment)
}

func (s *Service) encryptEnvironment(appID string, values map[string]string) ([]byte, error) {
	if len(values) == 0 {
		return []byte{}, nil
	}
	plain, err := json.Marshal(values)
	if err != nil {
		return nil, errors.New("encode environment configuration")
	}
	return s.encryptData(appID, "environment", plain)
}

func (s *Service) encryptData(appID, purpose string, plain []byte) ([]byte, error) {
	key, err := s.environmentKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("initialize environment encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize environment encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("generate environment encryption nonce")
	}
	version := "env1:"
	if purpose == "history" {
		version = "hist1:"
	}
	ciphertext := aead.Seal(nil, nonce, plain, []byte("harbor-"+purpose+"-v1:"+appID))
	return append([]byte(version), append(nonce, ciphertext...)...), nil
}

func (s *Service) decryptEnvironment(appID string, encrypted []byte) (map[string]string, error) {
	values := make(map[string]string)
	if len(encrypted) == 0 {
		return values, nil
	}
	plain, err := s.decryptData(appID, "environment", encrypted)
	if err != nil || json.Unmarshal(plain, &values) != nil {
		return nil, errors.New("stored environment configuration cannot be decrypted")
	}
	return values, nil
}

func (s *Service) decryptData(appID, purpose string, encrypted []byte) ([]byte, error) {
	version := "env1:"
	if purpose == "history" {
		version = "hist1:"
	}
	if !strings.HasPrefix(string(encrypted), version) {
		return nil, errors.New("stored secret data cannot be decrypted")
	}
	key, err := s.environmentKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("initialize secret decryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize secret decryption")
	}
	data := encrypted[len(version):]
	if len(data) < aead.NonceSize() {
		return nil, errors.New("stored secret data cannot be decrypted")
	}
	nonce, ciphertext := data[:aead.NonceSize()], data[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, []byte("harbor-"+purpose+"-v1:"+appID))
	if err != nil {
		return nil, errors.New("stored secret data cannot be decrypted")
	}
	return plain, nil
}

func (s *Service) environmentKey() ([]byte, error) {
	path := filepath.Join(s.opts.DataDir, "environment.key")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("generate environment encryption key")
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return s.environmentKey()
		}
		if err != nil {
			return nil, errors.New("create environment encryption key")
		}
		if err := file.Chmod(0600); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, errors.New("secure environment encryption key permissions")
		}
		if _, err := file.Write(key); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, errors.New("write environment encryption key")
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, errors.New("persist environment encryption key")
		}
		if err := file.Close(); err != nil {
			return nil, errors.New("close environment encryption key")
		}
		return key, nil
	}
	if err != nil {
		return nil, errors.New("inspect environment encryption key")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("environment encryption key must be a regular file")
	}
	if info.Mode().Perm() != 0600 {
		if err := os.Chmod(path, 0600); err != nil {
			return nil, errors.New("secure environment encryption key permissions")
		}
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		return nil, errors.New("environment encryption key is invalid")
	}
	return key, nil
}

func (s *Service) redactText(name, value string) (string, error) {
	stored, err := s.db.AppSettings(name)
	if err != nil {
		return "", errors.New("configured secrets could not be loaded")
	}
	environment, err := s.decryptEnvironment(stored.AppID, stored.EncryptedEnvironment)
	if err != nil {
		return "", errors.New("configured secrets could not be decrypted")
	}
	history, err := s.decryptRedactionHistory(stored.AppID, stored.EncryptedRedactionHistory)
	if err != nil {
		return "", errors.New("configured secrets could not be decrypted")
	}
	secrets := append(history, values(environment)...)
	bindings, err := s.db.GetBindingsForApp(stored.AppID)
	if err != nil {
		return "", errors.New("configured service credentials could not be loaded")
	}
	for _, binding := range bindings {
		secrets = append(secrets, binding.Password)
		svc, err := s.db.GetService(binding.ServiceID)
		if err != nil {
			// Preserve the stored password redaction for bindings to old or
			// unavailable services without making diagnostics unavailable too.
			continue
		}
		switch svc.Type {
		case "postgres":
			if svc.Port > 0 {
				secrets = append(secrets, fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable",
					binding.Username, binding.Password, svc.Port, binding.DatabaseName))
			}
		case "sqlite":
			if databaseURL, urlErr := managedservice.DatabaseURL(&binding, svc); urlErr == nil {
				secrets = append(secrets, databaseURL)
			}
		}
	}
	// Replace longer values first so a short secret that is a substring of a
	// longer value cannot leave the longer value partially visible.
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) == len(secrets[j]) {
			return secrets[i] < secrets[j]
		}
		return len(secrets[i]) > len(secrets[j])
	})
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value, nil
}

func (s *Service) redactError(name string, err error) error {
	if err == nil {
		return nil
	}
	message, redactErr := s.redactText(name, err.Error())
	if redactErr != nil {
		return redactedError{message: "operation failed; diagnostic text withheld because secrets could not be loaded", cause: err}
	}
	if message == err.Error() {
		return err
	}
	return redactedError{message: message, cause: err}
}

func (s *Service) decryptRedactionHistory(appID string, encrypted []byte) ([]string, error) {
	if len(encrypted) == 0 {
		return []string{}, nil
	}
	plain, err := s.decryptData(appID, "history", encrypted)
	if err != nil {
		return nil, err
	}
	var values []string
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, errors.New("stored environment redaction history cannot be decrypted")
	}
	return values, nil
}

func values(environment map[string]string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		result = append(result, value)
	}
	return result
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// Called under s.mu before builds start. Build credentials share the encrypted
// redaction history so runtime output and older releases remain protected.
func (s *Service) registerBuildRedactions(appID string, environment map[string]string) error {
	if len(environment) == 0 {
		return nil
	}
	settings, err := s.db.AppSettingsByID(appID)
	if err != nil {
		return err
	}
	history, err := s.decryptRedactionHistory(appID, settings.EncryptedRedactionHistory)
	if err != nil {
		return err
	}
	values := make(map[string]bool, len(history)+len(environment))
	for _, value := range history {
		if value != "" {
			values[value] = true
		}
	}
	changed := false
	for _, value := range environment {
		if value != "" && !values[value] {
			values[value] = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	history = make([]string, 0, len(values))
	for value := range values {
		history = append(history, value)
	}
	sort.Strings(history)
	data, err := json.Marshal(history)
	if err != nil || len(data) > maxRedactionHistoryBytes {
		return errors.New("build redaction history exceeds its supported limit")
	}
	settings.EncryptedRedactionHistory, err = s.encryptData(appID, "history", data)
	if err != nil {
		return err
	}
	err = s.db.UpdateAppSettings(settings.AppName, *settings)
	return err
}
