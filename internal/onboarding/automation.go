package onboarding

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"paas/internal/config"
	"paas/internal/source"
)

const maxGitHubPushBody = 1 << 20

// CreateHook creates a revocable GitHub push hook and returns its signing
// secret exactly once. The encrypted store retains the secret only for HMAC
// verification; list and metadata methods never return it.
func (m *Manager) CreateHook(ctx context.Context, name, branch string) (HookMetadata, string, error) {
	m.ensureAutomationStores()
	if err := ctx.Err(); err != nil {
		return HookMetadata{}, "", err
	}
	if !config.ValidName(name) {
		return HookMetadata{}, "", errors.New("invalid application name")
	}
	if strings.TrimSpace(branch) == "" {
		return HookMetadata{}, "", errors.New("a branch is required for this webhook")
	}
	if isCommitRef(branch) {
		return HookMetadata{}, "", errors.New("webhook hooks must target a branch name, not a commit")
	}
	settings, err := m.sources.get(name)
	if err != nil {
		return HookMetadata{}, "", ErrJobNotFound
	}
	if err := source.ValidateGitHubCheckoutRequest(settings.RepoURL, branch, settings.RootDir); err != nil {
		return HookMetadata{}, "", err
	}
	var idBytes [16]byte
	var secretBytes [32]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return HookMetadata{}, "", errors.New("could not create webhook")
	}
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return HookMetadata{}, "", errors.New("could not create webhook secret")
	}
	id := "hook_" + hex.EncodeToString(idBytes[:])
	secret := base64.RawURLEncoding.EncodeToString(secretBytes[:])
	now := time.Now().UTC()
	metadata := HookMetadata{ID: id, Name: name, Branch: branch, CreatedAt: now, UpdatedAt: now, Active: true}
	if err := m.protected.update(func(state *protectedState) error {
		state.Hooks[id] = storedHook{Metadata: metadata, Secret: []byte(secret)}
		return nil
	}); err != nil {
		return HookMetadata{}, "", err
	}
	return metadata, secret, nil
}

// ListHooks returns hook metadata without signing secrets.
func (m *Manager) ListHooks(name string) ([]HookMetadata, error) {
	m.ensureAutomationStores()
	state, err := m.protected.read()
	if err != nil {
		return nil, err
	}
	result := make([]HookMetadata, 0, len(state.Hooks))
	for _, hook := range state.Hooks {
		if name == "" || hook.Metadata.Name == name {
			result = append(result, hook.Metadata)
		}
	}
	sortHookMetadata(result)
	return result, nil
}

// RevokeHook permanently removes the secret so its signatures stop working.
func (m *Manager) RevokeHook(id string) error {
	m.ensureAutomationStores()
	if !validHookID(id) {
		return errors.New("invalid webhook id")
	}
	return m.protected.update(func(state *protectedState) error {
		if _, ok := state.Hooks[id]; !ok {
			return ErrJobNotFound
		}
		delete(state.Hooks, id)
		return nil
	})
}

// RegenerateHook rotates an active hook secret and returns the replacement
// exactly once. Previously generated secrets become invalid immediately.
func (m *Manager) RegenerateHook(id string) (HookMetadata, string, error) {
	m.ensureAutomationStores()
	if !validHookID(id) {
		return HookMetadata{}, "", errors.New("invalid webhook id")
	}
	var secretBytes [32]byte
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return HookMetadata{}, "", errors.New("could not create webhook secret")
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes[:])
	var metadata HookMetadata
	err := m.protected.update(func(state *protectedState) error {
		hook, ok := state.Hooks[id]
		if !ok {
			return ErrJobNotFound
		}
		hook.Secret = []byte(secret)
		hook.Metadata.UpdatedAt = time.Now().UTC()
		hook.Metadata.Active = true
		state.Hooks[id] = hook
		metadata = hook.Metadata
		return nil
	})
	if err != nil {
		return HookMetadata{}, "", err
	}
	return metadata, secret, nil
}

// PublicHandler handles signed GitHub push callbacks outside dashboard session
// authentication. The hook id selects an encrypted per-hook secret.
func (m *Manager) PublicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		kind, id := parsePublicHookPath(r.URL.Path)
		if kind == "" || !validHookID(id) {
			http.NotFound(w, r)
			return
		}
		if kind == "deploy" {
			m.handleDeployHook(w, r, id)
			return
		}
		m.handleGitHubPush(w, r, id)
	})
}

func parsePublicHookPath(path string) (kind, id string) {
	for _, prefix := range []struct{ path, kind string }{
		{path: "/api/hooks/github/", kind: "github"},
		{path: "/api/hooks/deploy/", kind: "deploy"},
		{path: "/github/", kind: "github"},
	} {
		if strings.HasPrefix(path, prefix.path) {
			id = strings.TrimPrefix(path, prefix.path)
			if strings.Contains(id, "/") {
				return "", ""
			}
			return prefix.kind, id
		}
	}
	return "", ""
}

func (m *Manager) handleGitHubPush(w http.ResponseWriter, r *http.Request, id string) {
	m.ensureAutomationStores()
	if r.Header.Get("X-GitHub-Event") != "push" {
		http.Error(w, "unsupported webhook event", http.StatusBadRequest)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if !validDeliveryID(delivery) {
		http.Error(w, "invalid webhook delivery", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGitHubPushBody))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "invalid webhook body", status)
		return
	}
	state, err := m.protected.read()
	if err != nil {
		http.Error(w, "webhook storage unavailable", http.StatusServiceUnavailable)
		return
	}
	hook, ok := state.Hooks[id]
	if !ok || !hook.Metadata.Active {
		http.NotFound(w, r)
		return
	}
	if !verifyGitHubSignature(hook.Secret, r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	var payload githubPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid push payload", http.StatusBadRequest)
		return
	}
	job, duplicate, accepted, err := m.handlePush(r.Context(), hook, delivery, payload, state)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrQueueFull) {
			status = http.StatusTooManyRequests
		}
		if errors.Is(err, ErrManagerClosed) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if accepted {
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "duplicate": duplicate, "job": job})
	} else {
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": false, "duplicate": false})
	}
}

// handleDeployHook accepts a bearer-authenticated CI callback. The configured
// hook branch is passed to checkout directly so a changed UI ref cannot make a
// previously issued hook deploy a different branch.
func (m *Manager) handleDeployHook(w http.ResponseWriter, r *http.Request, id string) {
	m.ensureAutomationStores()
	state, err := m.protected.read()
	if err != nil {
		http.Error(w, "webhook storage unavailable", http.StatusServiceUnavailable)
		return
	}
	hook, ok := state.Hooks[id]
	if !ok || !hook.Metadata.Active {
		http.NotFound(w, r)
		return
	}
	candidate, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || !equalHookSecret(candidate, hook.Secret) {
		http.Error(w, "invalid webhook token", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGitHubPushBody))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "invalid webhook body", status)
		return
	}
	_ = body // CI providers may send event metadata; it is not trusted for source selection.

	settings, err := m.sources.get(hook.Metadata.Name)
	if err != nil {
		http.Error(w, "saved webhook source is unavailable", http.StatusConflict)
		return
	}
	if err := source.ValidateGitHubCheckoutRequest(settings.RepoURL, hook.Metadata.Branch, settings.RootDir); err != nil {
		http.Error(w, "saved webhook branch is invalid", http.StatusConflict)
		return
	}
	if err := validateDeployBuildOptions(settings.BuildTimeoutSeconds, settings.IncludePaths, settings.IgnorePaths); err != nil {
		http.Error(w, "saved webhook source settings are invalid", http.StatusConflict)
		return
	}
	if !selectionEmpty(settings.Selection) {
		if err := validateSourceSelection(hook.Metadata.Name, settings.Selection); err != nil {
			http.Error(w, "saved build settings are invalid", http.StatusConflict)
			return
		}
	}
	if err := r.Context().Err(); err != nil {
		http.Error(w, "request canceled", http.StatusRequestTimeout)
		return
	}
	keys := make([]string, 0, 1)
	for _, header := range []string{"X-Delivery-ID", "Idempotency-Key"} {
		value := r.Header.Get(header)
		if value == "" {
			continue
		}
		if !validDeliveryID(value) {
			http.Error(w, "invalid webhook delivery", http.StatusBadRequest)
			return
		}
		keys = append(keys, "delivery:"+hook.Metadata.ID+":"+value)
		break
	}
	if job, found := m.duplicateJob(hook.Metadata.Name, keys); found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "duplicate": true, "job": job})
		return
	}
	if err := m.reserve(); err != nil {
		writePublicHookError(w, err)
		return
	}
	request := DeployRequest{Name: hook.Metadata.Name, RepoURL: settings.RepoURL, Ref: hook.Metadata.Branch, RootDir: settings.RootDir,
		Config: settings.Selection, BuildEnvironment: cloneStringMap(state.BuildEnvironment[hook.Metadata.Name]),
		BuildTimeoutSeconds: settings.BuildTimeoutSeconds, IncludePaths: append([]string(nil), settings.IncludePaths...),
		IgnorePaths: append([]string(nil), settings.IgnorePaths...), preserveSettings: true}
	job, duplicate, err := m.enqueueWithDedup(request, keys)
	if err != nil || duplicate {
		m.releaseReservation()
	}
	if err != nil {
		writePublicHookError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "duplicate": duplicate, "job": job})
}

func (m *Manager) duplicateJob(name string, keys []string) (Job, bool) {
	m.ensureAutomationStores()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forgotten[name] {
		return Job{}, false
	}
	for _, key := range keys {
		if existing, ok := m.dedup[key]; ok {
			if previous := m.jobs[existing.JobID]; previous != nil {
				return cloneJob(previous.Job), true
			}
		}
	}
	return Job{}, false
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(header, prefix)
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func equalHookSecret(candidate string, secret []byte) bool {
	// Hash both inputs first so the comparison always has equal-length operands;
	// hmac.Equal then compares in constant time regardless of candidate length.
	candidateHash := sha256.Sum256([]byte(candidate))
	secretHash := sha256.Sum256(secret)
	return hmac.Equal(candidateHash[:], secretHash[:])
}

func writePublicHookError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrQueueFull):
		status = http.StatusTooManyRequests
	case errors.Is(err, ErrManagerClosed):
		status = http.StatusServiceUnavailable
	}
	http.Error(w, err.Error(), status)
}

type githubPushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		HTMLURL  string `json:"html_url"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Commits    []githubPushCommit `json:"commits"`
	HeadCommit *githubPushCommit  `json:"head_commit"`
}

type githubPushCommit struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

func (m *Manager) handlePush(ctx context.Context, hook storedHook, delivery string, payload githubPushPayload, hookState protectedState) (Job, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, false, false, err
	}
	if payload.Deleted {
		return Job{}, false, false, nil
	}
	const prefix = "refs/heads/"
	if !strings.HasPrefix(payload.Ref, prefix) {
		return Job{}, false, false, nil
	}
	branch := strings.TrimPrefix(payload.Ref, prefix)
	if branch == "" {
		return Job{}, false, false, errors.New("push branch is invalid")
	}
	if hook.Metadata.Branch != branch {
		return Job{}, false, false, nil
	}
	repoURL := payload.Repository.HTMLURL
	if repoURL == "" {
		repoURL = payload.Repository.CloneURL
	}
	if repoURL == "" {
		return Job{}, false, false, errors.New("push repository is missing")
	}
	if err := source.ValidateGitHubCheckoutRequest(repoURL, payload.After, ""); err != nil {
		return Job{}, false, false, errors.New("push commit or repository is invalid")
	}
	settings, err := m.sources.get(hook.Metadata.Name)
	if err != nil {
		return Job{}, false, false, errors.New("saved webhook source is unavailable")
	}
	configured, err := normalizeHookRepository(settings.RepoURL)
	if err != nil {
		return Job{}, false, false, errors.New("saved webhook source is unavailable")
	}
	pushed, err := normalizeHookRepository(repoURL)
	if err != nil || configured != pushed {
		return Job{}, false, false, errors.New("push repository does not match this hook")
	}
	if err := validateDeployBuildOptions(settings.BuildTimeoutSeconds, settings.IncludePaths, settings.IgnorePaths); err != nil {
		return Job{}, false, false, errors.New("saved webhook source settings are invalid")
	}
	if err := validateSourceSelection(hook.Metadata.Name, settings.Selection); err != nil {
		return Job{}, false, false, errors.New("saved webhook build settings are invalid")
	}
	changed, err := changedPaths(payload)
	if err != nil {
		return Job{}, false, false, err
	}
	if !pathFiltersMatch(settings.IncludePaths, settings.IgnorePaths, changed) {
		return Job{}, false, false, nil
	}
	keys := []string{"delivery:" + hook.Metadata.ID + ":" + delivery, "commit:" + hook.Metadata.Name + ":" + branch + ":" + strings.ToLower(payload.After)}
	m.mu.Lock()
	for _, key := range keys {
		if existing, ok := m.dedup[key]; ok {
			if previous := m.jobs[existing.JobID]; previous != nil {
				job := cloneJob(previous.Job)
				m.mu.Unlock()
				return job, true, true, nil
			}
		}
	}
	if m.forgotten[hook.Metadata.Name] {
		m.mu.Unlock()
		return Job{}, false, false, errors.New("application source was removed")
	}
	m.mu.Unlock()
	if err := m.reserve(); err != nil {
		return Job{}, false, false, err
	}
	request := DeployRequest{Name: hook.Metadata.Name, RepoURL: settings.RepoURL, Ref: strings.ToLower(payload.After), RootDir: settings.RootDir,
		Config: settings.Selection, BuildEnvironment: cloneStringMap(hookState.BuildEnvironment[hook.Metadata.Name]),
		BuildTimeoutSeconds: settings.BuildTimeoutSeconds, IncludePaths: append([]string(nil), settings.IncludePaths...), IgnorePaths: append([]string(nil), settings.IgnorePaths...),
		preserveSettings: true, requireCommitMatch: true}
	job, duplicate, err := m.enqueueWithDedup(request, keys)
	if err != nil || duplicate {
		m.releaseReservation()
	}
	return job, duplicate, err == nil, err
}

func normalizeHookRepository(raw string) (string, error) {
	if err := source.ValidateGitHubCheckoutRequest(raw, "", ""); err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), ".git")), nil
}

func changedPaths(payload githubPushPayload) ([]string, error) {
	if len(payload.Commits) > 2048 {
		return nil, errors.New("push contains too many commits")
	}
	set := make(map[string]struct{})
	add := func(commit *githubPushCommit) error {
		if commit == nil {
			return nil
		}
		for _, group := range [][]string{commit.Added, commit.Modified, commit.Removed} {
			for _, value := range group {
				if len(value) > 512 {
					return errors.New("push path is too long")
				}
				set[value] = struct{}{}
				if len(set) > 10000 {
					return errors.New("push contains too many changed paths")
				}
			}
		}
		return nil
	}
	for i := range payload.Commits {
		if err := add(&payload.Commits[i]); err != nil {
			return nil, err
		}
	}
	if err := add(payload.HeadCommit); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(set))
	for value := range set {
		paths = append(paths, value)
	}
	return paths, nil
}

func verifyGitHubSignature(secret []byte, header string, body []byte) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

func validHookID(id string) bool {
	if !strings.HasPrefix(id, "hook_") || len(id) != len("hook_")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "hook_"))
	return err == nil
}

func validDeliveryID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func isCommitRef(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, r := range ref {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func sortHookMetadata(hooks []HookMetadata) {
	sort.Slice(hooks, func(i, j int) bool {
		if hooks[i].Name != hooks[j].Name {
			return hooks[i].Name < hooks[j].Name
		}
		return hooks[i].CreatedAt.Before(hooks[j].CreatedAt)
	})
}
