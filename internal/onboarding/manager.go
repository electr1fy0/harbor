package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/source"
)

const maxOutstandingJobs = 8
const maxConcurrentInspections = 2

var (
	ErrQueueFull         = errors.New("deployment queue is full")
	ErrJobNotFound       = errors.New("deployment job was not found")
	ErrSourceUnavailable = errors.New("saved repository settings are unavailable")
	ErrManagerClosed     = errors.New("deployment manager is shutting down")
	ErrInspectBusy       = errors.New("project inspection is busy; try again shortly")
)

type sourceCheckout func(context.Context, string, string, string, string, string) (string, func(), error)

type ServiceManager interface {
	AddService(context.Context, string, string) (*model.Service, error)
	Link(context.Context, string, string) (*model.Binding, error)
	List(context.Context) ([]model.Service, error)
}

type DeployRequest struct {
	Name                string            `json:"name"`
	RepoURL             string            `json:"repoURL"`
	Ref                 string            `json:"ref,omitempty"`
	RootDir             string            `json:"rootDir,omitempty"`
	Config              ConfigSelection   `json:"config,omitempty"`
	Environment         map[string]string `json:"environment,omitempty"`
	BuildEnvironment    map[string]string `json:"buildEnvironment,omitempty"`
	BuildTimeoutSeconds int               `json:"buildTimeoutSeconds,omitempty"`
	ClearCache          bool              `json:"clearCache,omitempty"`
	IncludePaths        []string          `json:"includePaths,omitempty"`
	IgnorePaths         []string          `json:"ignorePaths,omitempty"`
	SqliteService       string            `json:"sqliteService,omitempty"`
	CreateSqlite        bool              `json:"createSqlite,omitempty"`
	preserveSettings    bool
	requireCommitMatch  bool
	explicitSelection   bool
	dedupKeys           []string
}

type JobDeployment struct {
	ID          string      `json:"ID"`
	AppName     string      `json:"AppName"`
	ReleaseID   string      `json:"ReleaseID"`
	Port        int         `json:"Port"`
	State       model.State `json:"State"`
	MemoryLimit string      `json:"MemoryLimit"`
	CPUQuota    float64     `json:"CPUQuota"`
	TasksLimit  int         `json:"TasksLimit"`
	CreatedAt   time.Time   `json:"CreatedAt"`
	StartedAt   *time.Time  `json:"StartedAt"`
	StoppedAt   *time.Time  `json:"StoppedAt"`
	PromotedAt  *time.Time  `json:"PromotedAt"`
}

type Job struct {
	ID                   string              `json:"id"`
	Name                 string              `json:"name"`
	State                string              `json:"state"`
	Stage                string              `json:"stage"`
	CreatedAt            time.Time           `json:"createdAt"`
	UpdatedAt            time.Time           `json:"updatedAt"`
	Deployment           *JobDeployment      `json:"deployment,omitempty"`
	Error                string              `json:"error,omitempty"`
	Logs                 []string            `json:"logs"`
	Source               *source.GitMetadata `json:"source,omitempty"`
	CheckoutMilliseconds *int64              `json:"checkoutMilliseconds,omitempty"`
}

type jobRecord struct {
	Job
	request  DeployRequest
	cancel   context.CancelFunc
	terminal bool
}

type Manager struct {
	service      *deployment.Service
	services     ServiceManager
	credentials  *source.GitHubCredentialStore
	sources      *sourceStore
	protected    *protectedStore
	startupErr   error
	dedup        map[string]dedupRecord
	forgotten    map[string]bool
	buildUser    string
	ctx          context.Context
	cancel       context.CancelFunc
	queue        chan string
	workers      sync.WaitGroup
	mu           sync.Mutex
	initMu       sync.Mutex
	jobs         map[string]*jobRecord
	order        []string
	outstanding  int
	closed       bool
	inspectSlots chan struct{}
	checkout     sourceCheckout
}

func NewManager(service *deployment.Service, services ServiceManager, buildUser string) *Manager {
	dataDir, parent := "", context.Background()
	if service != nil {
		dataDir, parent = service.DataDir(), service.LifetimeContext()
	}
	if buildUser == "" {
		buildUser = "paas-build"
	}
	ctx, cancel := context.WithCancel(parent)
	protected := newProtectedStore(dataDir)
	m := &Manager{service: service, services: services, credentials: source.NewGitHubCredentialStore(dataDir, nil),
		sources: newSourceStore(dataDir), buildUser: buildUser, ctx: ctx, cancel: cancel,
		queue: make(chan string, maxOutstandingJobs), jobs: make(map[string]*jobRecord),
		inspectSlots: make(chan struct{}, maxConcurrentInspections), checkout: source.CheckoutGitHubAsWithToken,
		protected: protected, dedup: make(map[string]dedupRecord), forgotten: make(map[string]bool)}
	if persisted, err := protected.read(); err != nil {
		m.startupErr = err
	} else {
		m.restoreJobs(persisted)
		for key, record := range persisted.Dedup {
			m.dedup[key] = record
		}
	}
	if service != nil {
		m.workers.Add(1)
		go m.worker()
		m.requeueRestored()
	}
	return m
}

func (m *Manager) restoreJobs(state protectedState) {
	ids := append([]string(nil), state.Order...)
	for id := range state.Jobs {
		found := false
		for _, ordered := range ids {
			if ordered == id {
				found = true
				break
			}
		}
		if !found {
			ids = append(ids, id)
		}
	}
	if len(ids) > 100 {
		ids = ids[len(ids)-100:]
	}
	m.order = ids
	now := time.Now().UTC()
	for _, id := range ids {
		saved, ok := state.Jobs[id]
		if !ok {
			continue
		}
		request := cloneDeployRequest(saved.Request)
		request.preserveSettings = saved.PreserveSettings
		request.requireCommitMatch = saved.RequireCommitMatch
		request.explicitSelection = saved.ExplicitSelection
		record := &jobRecord{Job: saved.Job, request: request, terminal: saved.Terminal}
		if !record.terminal && record.Job.State != "QUEUED" {
			record.terminal = true
			record.Job.State, record.Job.Stage = "FAILED", "FAILED"
			record.Job.Error = "Daemon restarted while this deployment was running."
			record.Job.UpdatedAt = now
		}
		m.jobs[id] = record
		if !record.terminal {
			m.outstanding++
		}
	}
	if m.outstanding > maxOutstandingJobs {
		m.outstanding = maxOutstandingJobs
	}
}

func (m *Manager) requeueRestored() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		record := m.jobs[id]
		if record == nil || record.terminal {
			continue
		}
		select {
		case m.queue <- id:
		default:
			record.terminal = true
			record.Job.State, record.Job.Stage = "FAILED", "FAILED"
			record.Job.Error = "The recovered deployment queue exceeded its capacity."
			record.Job.UpdatedAt = time.Now().UTC()
			if m.outstanding > 0 {
				m.outstanding--
			}
		}
	}
	if err := m.persistLocked(); err != nil {
		log.Printf("persist recovered onboarding jobs: %v", err)
	}
}

func (m *Manager) Connection() (source.GitHubConnection, error) { return m.credentials.Metadata() }
func (m *Manager) ConnectGitHub(ctx context.Context, token string) (source.GitHubConnection, error) {
	return m.credentials.Connect(ctx, token)
}
func (m *Manager) DisconnectGitHub() error      { return m.credentials.Disconnect() }
func (m *Manager) CanRedeploy(name string) bool { return m.sources.canRedeploy(name) }

func (m *Manager) ensureAutomationStores() {
	m.initMu.Lock()
	defer m.initMu.Unlock()
	if m.protected == nil {
		m.protected = newProtectedStore("")
	}
	if m.sources == nil {
		m.sources = newSourceStore("")
	}
	if m.dedup == nil {
		m.dedup = make(map[string]dedupRecord)
	}
	if m.forgotten == nil {
		m.forgotten = make(map[string]bool)
	}
	if m.jobs == nil {
		m.jobs = make(map[string]*jobRecord)
	}
}

// GetSource returns editable source settings and build-environment key names.
// Secret values remain inside encrypted daemon storage.
func (m *Manager) GetSource(name string) (SourceSettings, []string, error) {
	m.ensureAutomationStores()
	if !config.ValidName(name) {
		return SourceSettings{}, nil, errors.New("invalid application name")
	}
	settings, values, err := m.sourceAndBuildEnvironment(name)
	if err != nil {
		return SourceSettings{}, nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return settings, keys, nil
}

// sourceAndBuildEnvironment takes one manager-serialized snapshot of settings
// persisted across the regular and encrypted stores. UpdateSource writes these
// stores transactionally under the same lock, so redeploys and webhook jobs
// cannot combine a source version with a different build-environment version.
func (m *Manager) sourceAndBuildEnvironment(name string) (SourceSettings, map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings, err := m.sources.get(name)
	if err != nil {
		return SourceSettings{}, nil, savedSourceLookupError(err)
	}
	state, err := m.protected.read()
	if err != nil {
		return SourceSettings{}, nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	return settings, cloneStringMap(state.BuildEnvironment[name]), nil
}

func savedSourceLookupError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrJobNotFound
	}
	return fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
}

// UpdateSource persists a validated repository and build selection. A nil
// buildEnvironment preserves existing values; a non-nil empty map clears them.
func (m *Manager) UpdateSource(ctx context.Context, settings SourceSettings, buildEnvironment *map[string]string) error {
	m.ensureAutomationStores()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !config.ValidName(settings.Name) {
		return errors.New("invalid application name")
	}
	if err := source.ValidateGitHubCheckoutRequest(settings.RepoURL, settings.Ref, settings.RootDir); err != nil {
		return err
	}
	if !selectionEmpty(settings.Selection) {
		if err := validateSourceSelection(settings.Name, settings.Selection); err != nil {
			return err
		}
	}
	if settings.BuildTimeoutSeconds != 0 && (settings.BuildTimeoutSeconds < 10 || settings.BuildTimeoutSeconds > 1800) {
		return errors.New("build timeout must be between 10 and 1800 seconds")
	}
	if err := validatePathFilters(settings.IncludePaths, settings.IgnorePaths); err != nil {
		return err
	}
	if buildEnvironment != nil {
		if err := validateBuildEnvironment(*buildEnvironment); err != nil {
			return err
		}
	}
	settings.ExplicitSelection = !selectionEmpty(settings.Selection)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveSourceAndBuildEnvironmentLocked(settings, buildEnvironment); err != nil {
		return err
	}
	delete(m.forgotten, settings.Name)
	return nil
}

// saveSourceAndBuildEnvironmentLocked updates the two persisted halves of a
// source configuration as one logical transaction. Callers hold m.mu so
// GetSource, Forget, and other source writes cannot observe an intermediate
// state. If the source file fails after the encrypted environment update, the
// previous encrypted values are restored before returning the failure.
func (m *Manager) saveSourceAndBuildEnvironmentLocked(settings SourceSettings, buildEnvironment *map[string]string) error {
	if buildEnvironment == nil {
		return m.sources.save(settings)
	}
	previous, err := m.protected.read()
	if err != nil {
		return err
	}
	previousValues, hadPrevious := previous.BuildEnvironment[settings.Name]
	if err := m.protected.update(func(state *protectedState) error {
		state.BuildEnvironment[settings.Name] = cloneStringMap(*buildEnvironment)
		return nil
	}); err != nil {
		return err
	}
	if err := m.sources.save(settings); err != nil {
		rollbackErr := m.protected.update(func(state *protectedState) error {
			if hadPrevious {
				state.BuildEnvironment[settings.Name] = cloneStringMap(previousValues)
			} else {
				delete(state.BuildEnvironment, settings.Name)
			}
			return nil
		})
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("restore previous build environment: %w", rollbackErr))
		}
		return err
	}
	return nil
}

// Forget cancels all in-memory work for an application and removes its saved
// source, encrypted build environment, webhook secrets, queued jobs, and
// duplicate-delivery records. It blocks new jobs until a source is explicitly
// configured again in this daemon process.
func (m *Manager) Forget(name string) error {
	m.ensureAutomationStores()
	if !config.ValidName(name) {
		return errors.New("invalid application name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previousSettings, sourceErr := m.sources.get(name)
	hasPreviousSettings := sourceErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return sourceErr
	}
	removed := make(map[string]struct{})
	nextJobs := make(map[string]*jobRecord, len(m.jobs))
	for id, record := range m.jobs {
		if record.Job.Name != name {
			nextJobs[id] = record
			continue
		}
		removed[id] = struct{}{}
	}
	nextOrder := make([]string, 0, len(m.order))
	for _, id := range m.order {
		if nextJobs[id] != nil {
			nextOrder = append(nextOrder, id)
		}
	}
	nextDedup := make(map[string]dedupRecord, len(m.dedup))
	for key, record := range m.dedup {
		if _, deleted := removed[record.JobID]; !deleted {
			nextDedup[key] = record
		}
	}
	if err := m.sources.delete(name); err != nil {
		return err
	}
	storeErr := m.protected.update(func(state *protectedState) error {
		writePersistedJobState(state, nextJobs, nextOrder, nextDedup)
		delete(state.BuildEnvironment, name)
		for id, hook := range state.Hooks {
			if hook.Metadata.Name == name {
				delete(state.Hooks, id)
			}
		}
		return nil
	})
	if storeErr != nil {
		if hasPreviousSettings {
			if rollbackErr := m.sources.restore(previousSettings); rollbackErr != nil {
				// If the two stores cannot be brought back into sync, prevent this
				// process from starting forgotten jobs from their in-memory copies.
				m.applyForgottenJobsLocked(name, removed, nextJobs, nextOrder, nextDedup)
				cleanupErr := m.protected.update(func(state *protectedState) error {
					writePersistedJobState(state, nextJobs, nextOrder, nextDedup)
					delete(state.BuildEnvironment, name)
					for id, hook := range state.Hooks {
						if hook.Metadata.Name == name {
							delete(state.Hooks, id)
						}
					}
					return nil
				})
				return errors.Join(storeErr, fmt.Errorf("restore source settings after failed forget: %w", rollbackErr), cleanupErr)
			}
		}
		return storeErr
	}
	m.applyForgottenJobsLocked(name, removed, nextJobs, nextOrder, nextDedup)
	return nil
}

// applyForgottenJobsLocked updates the in-memory job index only after its
// protected-store representation has committed. Workers may already have
// received a queued ID, but they block on m.mu and will observe the deletion.
func (m *Manager) applyForgottenJobsLocked(name string, removed map[string]struct{}, jobs map[string]*jobRecord, order []string, dedup map[string]dedupRecord) {
	for id := range removed {
		if record := m.jobs[id]; record != nil {
			m.removeQueuedLocked(id)
			if record.cancel != nil {
				record.cancel()
			}
			if !record.terminal && m.outstanding > 0 {
				m.outstanding--
			}
		}
	}
	m.jobs = jobs
	m.order = order
	m.dedup = dedup
	m.forgotten[name] = true
}

func (m *Manager) Repositories(ctx context.Context) ([]source.GitHubRepository, error) {
	token, err := m.githubToken()
	if err != nil {
		return nil, err
	}
	return source.NewGitHubAPI().Repositories(ctx, token)
}

// BrowseRepositories applies the connected-account owner filter or lists public
// repositories when no credential is configured.
func (m *Manager) BrowseRepositories(ctx context.Context, owner string) ([]source.GitHubRepository, error) {
	token, err := m.githubToken()
	if err != nil {
		return nil, err
	}
	api := source.NewGitHubAPI()
	if token == "" {
		return api.PublicRepositories(ctx, owner, "")
	}
	repos, err := api.Repositories(ctx, token)
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return repos, nil
	}
	filtered := make([]source.GitHubRepository, 0)
	for _, repo := range repos {
		if strings.EqualFold(strings.SplitN(repo.FullName, "/", 2)[0], owner) {
			filtered = append(filtered, repo)
		}
	}
	return filtered, nil
}

func (m *Manager) Repository(ctx context.Context, owner, repo string) (source.GitHubRepository, error) {
	token, err := m.githubToken()
	if err != nil {
		return source.GitHubRepository{}, err
	}
	return source.NewGitHubAPI().Repository(ctx, owner, repo, token)
}

func (m *Manager) githubToken() (string, error) {
	token, err := m.credentials.Token()
	if err != nil {
		return "", err
	}
	if token != "" {
		return token, nil
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN")), nil
}

func (m *Manager) InspectProject(ctx context.Context, repoURL, ref, rootDir, name string, selection ConfigSelection) (Inspection, error) {
	if name == "" {
		name = "app"
	}
	return m.inspectProject(ctx, repoURL, ref, rootDir, name, selection)
}

func (m *Manager) inspectProject(ctx context.Context, repoURL, ref, rootDir, name string, selection ConfigSelection) (Inspection, error) {
	if m.service == nil {
		return Inspection{}, errors.New("onboarding is unavailable")
	}
	select {
	case m.inspectSlots <- struct{}{}:
		defer func() { <-m.inspectSlots }()
	case <-ctx.Done():
		return Inspection{}, ctx.Err()
	case <-m.ctx.Done():
		return Inspection{}, ErrManagerClosed
	case <-time.After(2 * time.Second):
		return Inspection{}, ErrInspectBusy
	}
	inspectCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	token, err := m.githubToken()
	if err != nil {
		return Inspection{}, err
	}
	selected, cleanup, err := m.checkout(inspectCtx, repoURL, ref, rootDir, m.buildUser, token)
	if err != nil {
		return Inspection{}, err
	}
	defer cleanup()
	return Inspect(selected, name, selection)
}

func (m *Manager) Submit(ctx context.Context, request DeployRequest) (Job, error) {
	m.ensureAutomationStores()
	if err := m.reserve(); err != nil {
		return Job{}, err
	}
	reserved := true
	defer func() {
		if reserved {
			m.releaseReservation()
		}
	}()
	if !config.ValidName(request.Name) {
		return Job{}, errors.New("invalid application name")
	}
	if err := source.ValidateGitHubCheckoutRequest(request.RepoURL, request.Ref, request.RootDir); err != nil {
		return Job{}, err
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return Job{}, err
	}
	if err := validateBuildEnvironment(request.BuildEnvironment); err != nil {
		return Job{}, err
	}
	if err := validateDeployBuildOptions(request.BuildTimeoutSeconds, request.IncludePaths, request.IgnorePaths); err != nil {
		return Job{}, err
	}
	if request.BuildEnvironment == nil {
		state, err := m.protected.read()
		if err != nil {
			return Job{}, err
		}
		if values, ok := state.BuildEnvironment[request.Name]; ok {
			request.BuildEnvironment = cloneStringMap(values)
		}
	}
	if _, err := configFromSelection(request.Name, request.Config); err != nil {
		return Job{}, err
	}
	if err := m.validateSqlite(ctx, request); err != nil {
		return Job{}, err
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	job, err := m.enqueue(request)
	if err != nil {
		return Job{}, err
	}
	reserved = false
	return job, nil
}

func (m *Manager) Redeploy(ctx context.Context, name string) (Job, error) {
	return m.RedeployWithOptions(ctx, name, false)
}

func (m *Manager) RedeployWithOptions(ctx context.Context, name string, clearCache bool) (Job, error) {
	m.ensureAutomationStores()
	if err := m.reserve(); err != nil {
		return Job{}, err
	}
	reserved := true
	defer func() {
		if reserved {
			m.releaseReservation()
		}
	}()
	if !config.ValidName(name) {
		return Job{}, errors.New("invalid application name")
	}
	settings, buildEnvironment, err := m.sourceAndBuildEnvironment(name)
	if err != nil {
		return Job{}, err
	}
	if err := source.ValidateGitHubCheckoutRequest(settings.RepoURL, settings.Ref, settings.RootDir); err != nil {
		return Job{}, errors.New("saved repository settings are invalid")
	}
	if !selectionEmpty(settings.Selection) {
		if err := validateSourceSelection(name, settings.Selection); err != nil {
			return Job{}, errors.New("saved build settings are invalid")
		}
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	job, err := m.enqueue(DeployRequest{Name: name, RepoURL: settings.RepoURL, Ref: settings.Ref, RootDir: settings.RootDir, Config: settings.Selection,
		BuildEnvironment: buildEnvironment, BuildTimeoutSeconds: settings.BuildTimeoutSeconds,
		ClearCache: clearCache, IncludePaths: append([]string(nil), settings.IncludePaths...), IgnorePaths: append([]string(nil), settings.IgnorePaths...),
		preserveSettings: true, explicitSelection: settings.ExplicitSelection})
	if err != nil {
		return Job{}, err
	}
	reserved = false
	return job, nil
}

func (m *Manager) validateSqlite(ctx context.Context, request DeployRequest) error {
	if !request.CreateSqlite && request.SqliteService == "" {
		return nil
	}
	if m.services == nil {
		return errors.New("managed SQLite is unavailable")
	}
	if request.CreateSqlite && !config.ValidName(request.SqliteService) {
		return errors.New("enter a valid name for the SQLite service")
	}
	list, err := m.services.List(ctx)
	if err != nil {
		return errors.New("managed services could not be checked")
	}
	for _, service := range list {
		if service.Name != request.SqliteService {
			continue
		}
		if request.CreateSqlite {
			return errors.New("a managed service already uses that name")
		}
		if service.Type == "sqlite" && service.State == model.ServiceReady {
			return nil
		}
	}
	if request.CreateSqlite {
		return nil
	}
	return errors.New("choose a READY managed SQLite service")
}

func (m *Manager) reserve() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx != nil && m.ctx.Err() != nil {
		return ErrManagerClosed
	}
	if m.service == nil {
		return errors.New("onboarding is unavailable")
	}
	if m.startupErr != nil {
		return errors.New("automation persistence is unavailable")
	}
	if m.outstanding >= maxOutstandingJobs {
		return ErrQueueFull
	}
	m.outstanding++
	return nil
}

func (m *Manager) releaseReservation() {
	m.mu.Lock()
	if m.outstanding > 0 {
		m.outstanding--
	}
	m.mu.Unlock()
}

func (m *Manager) enqueue(request DeployRequest) (Job, error) {
	job, _, err := m.enqueueWithDedup(request, nil)
	return job, err
}

func (m *Manager) enqueueWithDedup(request DeployRequest, keys []string) (Job, bool, error) {
	m.ensureAutomationStores()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Job{}, false, errors.New("could not create deployment job")
	}
	id, now := "job_"+hex.EncodeToString(raw[:]), time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Job{}, false, ErrManagerClosed
	}
	if m.startupErr != nil {
		return Job{}, false, errors.New("automation persistence is unavailable")
	}
	if m.forgotten[request.Name] {
		return Job{}, false, errors.New("application source was removed")
	}
	for _, key := range keys {
		if existing, ok := m.dedup[key]; ok {
			if previous := m.jobs[existing.JobID]; previous != nil {
				return cloneJob(previous.Job), true, nil
			}
		}
	}
	request = cloneDeployRequest(request)
	record := &jobRecord{Job: Job{ID: id, Name: request.Name, State: "QUEUED", Stage: "QUEUED", CreatedAt: now, UpdatedAt: now, Logs: []string{"Deployment queued."}}, request: request}
	m.jobs[id] = record
	m.order = append(m.order, id)
	for _, key := range keys {
		m.dedup[key] = dedupRecord{JobID: id, CreatedAt: now}
	}
	select {
	case m.queue <- id:
		m.pruneLocked()
		if err := m.persistLocked(); err != nil {
			delete(m.jobs, id)
			m.order = removeID(m.order, id)
			for _, key := range keys {
				if m.dedup[key].JobID == id {
					delete(m.dedup, key)
				}
			}
			return Job{}, false, err
		}
		return cloneJob(record.Job), false, nil
	default:
		delete(m.jobs, id)
		m.order = m.order[:len(m.order)-1]
		for _, key := range keys {
			if m.dedup[key].JobID == id {
				delete(m.dedup, key)
			}
		}
		return Job{}, false, ErrQueueFull
	}
}

func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	record := m.jobs[id]
	if record == nil {
		m.mu.Unlock()
		return Job{}, ErrJobNotFound
	}
	job := cloneJob(record.Job)
	secrets := buildEnvironmentValues(record.request.BuildEnvironment)
	m.mu.Unlock()
	if job.Deployment != nil && m.service != nil {
		if logs, err := m.service.LogsForDeployment(job.Name, job.Deployment.ID); err == nil && logs != "" {
			job.Logs = []string{safeLogTail(logs, 64<<10, secrets)}
		}
	}
	return job, nil
}

func (m *Manager) ListJobs(name string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := make([]Job, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if record := m.jobs[m.order[i]]; record != nil && (name == "" || record.Job.Name == name) {
			jobs = append(jobs, cloneJob(record.Job))
		}
	}
	return jobs
}

func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.jobs[id]
	if record == nil {
		return Job{}, ErrJobNotFound
	}
	if record.terminal {
		return cloneJob(record.Job), nil
	}
	if record.cancel == nil {
		record.terminal = true
		record.Job.State = "CANCELED"
		record.Job.Stage = "CANCELED"
		record.Job.UpdatedAt = time.Now().UTC()
		m.removeQueuedLocked(id)
		if m.outstanding > 0 {
			m.outstanding--
		}
	} else {
		record.Job.State = "CANCELLING"
		record.Job.UpdatedAt = time.Now().UTC()
		record.cancel()
	}
	if err := m.persistLocked(); err != nil {
		log.Printf("persist canceled onboarding job: %v", err)
	}
	return cloneJob(record.Job), nil
}

// removeQueuedLocked compacts canceled jobs out of the bounded channel. A
// worker may receive an item concurrently, so drain only the entries currently
// available and put the remaining ones back in their original order.
func (m *Manager) removeQueuedLocked(id string) {
	if m.queue == nil {
		return
	}
	queued := make([]string, 0, len(m.queue))
	for count := len(m.queue); count > 0; count-- {
		select {
		case queuedID := <-m.queue:
			if queuedID != id {
				queued = append(queued, queuedID)
			}
		default:
			count = 0
		}
	}
	for _, queuedID := range queued {
		m.queue <- queuedID
	}
}

func (m *Manager) worker() {
	defer m.workers.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case id := <-m.queue:
			m.run(id)
		}
	}
}

func (m *Manager) run(id string) {
	m.mu.Lock()
	record := m.jobs[id]
	if record == nil || record.terminal {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	record.cancel = cancel
	record.Job.State = "RUNNING"
	record.Job.Stage = "CLONING"
	record.Job.UpdatedAt = time.Now().UTC()
	request := record.request
	if err := m.persistLocked(); err != nil {
		log.Printf("persist onboarding job start: %v", err)
	}
	m.mu.Unlock()
	defer cancel()
	if ctx.Err() != nil {
		m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", nil)
		return
	}
	token, err := m.githubToken()
	if err != nil {
		m.finish(id, "FAILED", "FAILED", "GitHub credentials are unavailable.", nil)
		return
	}
	checkoutStarted := time.Now()
	selected, cleanup, err := m.checkout(ctx, request.RepoURL, request.Ref, request.RootDir, m.buildUser, token)
	m.recordCheckoutTiming(id, time.Since(checkoutStarted))
	if err != nil {
		if ctx.Err() != nil {
			m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", nil)
			return
		}
		m.finish(id, "FAILED", "FAILED", "Repository checkout failed. Check the URL, branch, and GitHub access.", nil)
		return
	}
	defer cleanup()
	metadata, metadataErr := source.ReadGitMetadata(ctx, selected, request.RepoURL, m.buildUser)
	if metadataErr == nil {
		if request.requireCommitMatch && !strings.EqualFold(metadata.CommitSHA, request.Ref) {
			m.finish(id, "FAILED", "FAILED", "The checked out commit did not match the webhook push.", nil)
			return
		}
		m.setSourceMetadata(id, metadata)
	} else if request.requireCommitMatch {
		m.finish(id, "FAILED", "FAILED", "The webhook commit metadata could not be verified.", nil)
		return
	}
	if ctx.Err() != nil {
		m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", nil)
		return
	}
	inspection, err := Inspect(selected, request.Name, ConfigSelection{})
	if err != nil {
		m.finish(id, "FAILED", "FAILED", "Repository settings could not be prepared.", nil)
		return
	}
	selection := request.Config
	storedSelection := request.Config
	if request.preserveSettings && inspection.ExistingConfig && !request.explicitSelection {
		// Older saved records may contain inspection defaults in Selection even
		// though the repository config is authoritative. Only UpdateSource marks
		// those values as intentional overrides.
		selection = ConfigSelection{}
	}
	if !inspection.ExistingConfig {
		if selectionEmpty(selection) {
			selection = selectionFromView(inspection.Config)
		}
		storedSelection = selection
	} else {
		// The checked-out paas.yaml remains authoritative for later redeploys.
		// Explicit overrides saved through UpdateSource are stored separately;
		// do not turn inspection-time values from this initial submit into stale
		// overrides that hide future repository config changes.
		storedSelection = ConfigSelection{}
	}
	cfg, err := PrepareConfigured(selected, request.Name, selection)
	if err != nil {
		m.finish(id, "FAILED", "FAILED", "Repository settings could not be prepared.", nil)
		return
	}
	memory, cpu, tasks := cfg.Resources.Memory, cfg.Resources.CPU, cfg.Resources.Tasks
	if !request.preserveSettings {
		if _, err := m.service.Configure(ctx, request.Name, deployment.ConfigureOptions{Environment: cloneStringMap(request.Environment), MemoryLimit: &memory, CPUQuota: &cpu, TasksLimit: &tasks}); err != nil {
			m.finish(id, "FAILED", "FAILED", "Application settings could not be saved. Check environment names and resource values.", nil)
			return
		}
	}
	if !request.preserveSettings {
		settings := SourceSettings{Name: request.Name, RepoURL: request.RepoURL, Ref: request.Ref, RootDir: request.RootDir, Selection: storedSelection,
			IncludePaths: request.IncludePaths, IgnorePaths: request.IgnorePaths, BuildTimeoutSeconds: request.BuildTimeoutSeconds}
		if err := m.saveSourceForCurrentJob(id, settings, request.BuildEnvironment); err != nil {
			m.finish(id, "FAILED", "FAILED", "Repository settings could not be saved for retries.", nil)
			return
		}
	}
	if request.CreateSqlite {
		m.setStage(id, "PROVISIONING_DATABASE")
		created, err := m.services.AddService(ctx, "sqlite", request.SqliteService)
		if err != nil || created == nil || created.Type != "sqlite" || created.State != model.ServiceReady {
			m.finish(id, "FAILED", "FAILED", "SQLite service could not be created.", nil)
			return
		}
	}
	if request.SqliteService != "" {
		if err := m.bindDatabase(ctx, request.Name, request.SqliteService); err != nil {
			m.finish(id, "FAILED", "FAILED", "The SQLite service could not be linked.", nil)
			return
		}
	}
	if ctx.Err() != nil {
		m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", nil)
		return
	}
	type deployResult struct {
		deployment *model.Deployment
		err        error
	}
	result := make(chan deployResult, 1)
	attempts := make(chan deployment.DeploymentIdentity, 1)
	go func() {
		options := contracts.DeployOptions{BuildEnvironment: cloneStringMap(request.BuildEnvironment), BuildTimeout: time.Duration(request.BuildTimeoutSeconds) * time.Second, ClearCache: request.ClearCache}
		d, err := m.service.DeployWithOptions(ctx, selected, request.Name, options, func(identity deployment.DeploymentIdentity) {
			attempts <- identity
		})
		result <- deployResult{deployment: d, err: err}
	}()
	tick := time.NewTicker(350 * time.Millisecond)
	defer tick.Stop()
	logsTick := time.NewTicker(time.Second)
	defer logsTick.Stop()
	observedID := ""
	for {
		select {
		case identity := <-attempts:
			if identity.AppName == request.Name {
				observedID = identity.ID
			}
		case outcome := <-result:
			deployed, deployErr := outcome.deployment, outcome.err
			if deployed != nil {
				m.updateDeployment(id, deployed)
			}
			if ctx.Err() != nil || errors.Is(deployErr, context.Canceled) {
				m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", deployed)
			} else if deployed != nil && deployed.State == model.Running {
				m.finish(id, "SUCCEEDED", "COMPLETED", "", deployed)
			} else {
				m.finish(id, "FAILED", "FAILED", safeDeploymentFailure(deployed), deployed)
			}
			return
		case <-tick.C:
			if observedID != "" {
				if d, e := m.service.DeploymentForApp(request.Name, observedID); e == nil && d != nil {
					m.updateDeployment(id, d)
					m.stageForDeployment(id, d.State)
				}
			}
		case <-logsTick.C:
			if observedID != "" {
				m.refreshLogs(id, request.Name, observedID)
			}
		case <-ctx.Done():
			outcome := <-result
			m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", outcome.deployment)
			return
		}
	}
}

func (m *Manager) bindDatabase(ctx context.Context, appName, serviceName string) error {
	list, err := m.services.List(ctx)
	if err != nil {
		return err
	}
	ready := false
	for _, svc := range list {
		if svc.Name == serviceName && svc.Type == "sqlite" && svc.State == model.ServiceReady {
			ready = true
			break
		}
	}
	if !ready {
		return errors.New("managed SQLite service is not ready")
	}
	_, err = m.services.Link(ctx, appName, serviceName)
	return err
}

func (m *Manager) setStage(id, stage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil && !j.terminal {
		j.Job.Stage = stage
		j.Job.UpdatedAt = time.Now().UTC()
		if err := m.persistLocked(); err != nil {
			log.Printf("persist onboarding job stage: %v", err)
		}
	}
}
func (m *Manager) stageForDeployment(id string, state model.State) {
	switch state {
	case model.Building:
		m.setStage(id, "BUILDING")
	case model.Built, model.Starting:
		m.setStage(id, "STARTING")
	case model.HealthChecking:
		m.setStage(id, "HEALTH_CHECK")
	}
}
func (m *Manager) updateDeployment(id string, d *model.Deployment) {
	if d == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil && !j.terminal {
		j.Job.Deployment = &JobDeployment{ID: d.ID, AppName: d.AppName, ReleaseID: d.ReleaseID, Port: d.Port, State: d.State, MemoryLimit: d.MemoryLimit, CPUQuota: d.CPUQuota, TasksLimit: d.TasksLimit, CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, StoppedAt: d.StoppedAt, PromotedAt: d.PromotedAt}
		j.Job.UpdatedAt = time.Now().UTC()
		if err := m.persistLocked(); err != nil {
			log.Printf("persist onboarding deployment: %v", err)
		}
	}
}
func (m *Manager) refreshLogs(id, name, deploymentID string) {
	if deploymentID == "" {
		return
	}
	logs, err := m.service.LogsForDeployment(name, deploymentID)
	if err != nil || logs == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil && !j.terminal {
		j.Job.Logs = []string{safeLogTail(logs, 64<<10, buildEnvironmentValues(j.request.BuildEnvironment))}
		j.Job.UpdatedAt = time.Now().UTC()
		if err := m.persistLocked(); err != nil {
			log.Printf("persist onboarding job logs: %v", err)
		}
	}
}
func (m *Manager) finish(id, state, stage, message string, d *model.Deployment) {
	if d != nil {
		m.updateDeployment(id, d)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil || j.terminal {
		return
	}
	j.Job.State = state
	j.Job.Stage = stage
	j.Job.Error = message
	j.Job.UpdatedAt = time.Now().UTC()
	j.terminal = true
	if j.cancel != nil {
		j.cancel()
	}
	if m.outstanding > 0 {
		m.outstanding--
	}
	m.pruneLocked()
	if err := m.persistLocked(); err != nil {
		log.Printf("persist onboarding job result: %v", err)
	}
}
func (m *Manager) pruneLocked() {
	for len(m.order) > 100 {
		// Keep active jobs regardless of age, but do not let one long-running
		// oldest job prevent pruning newer terminal history indefinitely.
		removeAt := -1
		for index, id := range m.order {
			record := m.jobs[id]
			if record == nil || record.terminal {
				removeAt = index
				break
			}
		}
		if removeAt < 0 {
			break
		}
		old := m.order[removeAt]
		delete(m.jobs, old)
		for key, record := range m.dedup {
			if record.JobID == old {
				delete(m.dedup, key)
			}
		}
		m.order = append(m.order[:removeAt], m.order[removeAt+1:]...)
	}
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	for key, record := range m.dedup {
		if record.CreatedAt.Before(cutoff) {
			delete(m.dedup, key)
		}
	}
}

func (m *Manager) persistLocked() error {
	if m.protected == nil {
		return errors.New("protected automation storage is unavailable")
	}
	return m.protected.update(func(state *protectedState) error {
		writePersistedJobState(state, m.jobs, m.order, m.dedup)
		return nil
	})
}

func writePersistedJobState(state *protectedState, jobs map[string]*jobRecord, order []string, dedup map[string]dedupRecord) {
	state.Jobs = make(map[string]storedJob, len(jobs))
	for id, record := range jobs {
		state.Jobs[id] = storedJob{Job: cloneJob(record.Job), Request: cloneDeployRequest(record.request), PreserveSettings: record.request.preserveSettings, RequireCommitMatch: record.request.requireCommitMatch, ExplicitSelection: record.request.explicitSelection, Terminal: record.terminal}
	}
	state.Order = append([]string(nil), order...)
	state.Dedup = make(map[string]dedupRecord, len(dedup))
	for key, record := range dedup {
		state.Dedup[key] = record
	}
}

func (m *Manager) setSourceMetadata(id string, metadata source.GitMetadata) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record := m.jobs[id]; record != nil && !record.terminal {
		copy := metadata
		record.Job.Source = &copy
		record.Job.UpdatedAt = time.Now().UTC()
		if err := m.persistLocked(); err != nil {
			log.Printf("persist onboarding source metadata: %v", err)
		}
	}
}

func (m *Manager) saveSourceForCurrentJob(id string, settings SourceSettings, buildEnvironment map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.jobs[id]
	if record == nil || record.terminal || m.forgotten[settings.Name] {
		return errors.New("deployment was canceled")
	}
	values := cloneStringMap(buildEnvironment)
	if err := m.saveSourceAndBuildEnvironmentLocked(settings, &values); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	for _, j := range m.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
	if err := m.persistLocked(); err != nil {
		log.Printf("persist onboarding shutdown: %v", err)
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateEnvironment(values map[string]string) error {
	if len(values) > 100 {
		return errors.New("too many application environment values")
	}
	for key, value := range values {
		if !envKeyPattern.MatchString(key) || reservedEnvironmentKey(key) {
			return fmt.Errorf("invalid or reserved environment key %q", key)
		}
		if len(value) > 65_536 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment value for %q is invalid or too large", key)
		}
	}
	return nil
}

func validateBuildEnvironment(values map[string]string) error {
	if len(values) > 100 {
		return errors.New("too many build environment values")
	}
	total := 0
	for key, value := range values {
		if !envKeyPattern.MatchString(key) || reservedBuildKey(key) {
			return fmt.Errorf("invalid or reserved build environment key %q", key)
		}
		if len(value) > 65_536 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("build environment value for %q is invalid or too large", key)
		}
		total += len(key) + len(value)
	}
	if total > 48<<10 {
		return errors.New("build environment values exceed the size limit")
	}
	return nil
}

func reservedBuildKey(key string) bool {
	switch key {
	case "PATH", "HOME", "TMPDIR", "PAAS_OUT", "CGO_ENABLED", "GOFLAGS", "GOMAXPROCS", "GOCACHE", "GOMODCACHE",
		"npm_config_cache", "npm_config_store_dir", "pnpm_config_store_dir", "YARN_CACHE_FOLDER", "YARN_GLOBAL_FOLDER":
		return true
	default:
		return false
	}
}

func buildEnvironmentValues(values map[string]string) []string {
	secrets := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets
}

func redactSecrets(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func safeLogTail(value string, limit int, secrets []string) string {
	return tailString(redactSecrets(value, secrets), limit)
}

func validateDeployBuildOptions(timeout int, includes, ignores []string) error {
	if timeout != 0 && (timeout < 10 || timeout > 1800) {
		return errors.New("build timeout must be between 10 and 1800 seconds")
	}
	return validatePathFilters(includes, ignores)
}
func selectionFromView(c ProjectConfig) ConfigSelection {
	cpu, tasks := c.CPUQuota, c.TasksLimit
	return ConfigSelection{Runtime: c.Runtime, BuildCommand: c.BuildCommand, StartCommand: c.StartCommand, HealthPath: c.HealthPath, MemoryLimit: c.MemoryLimit, CPUQuota: &cpu, TasksLimit: &tasks}
}
func selectionEmpty(s ConfigSelection) bool {
	return s.Runtime == "" && s.BuildCommand == "" && s.StartCommand == "" && s.HealthPath == "" && s.MemoryLimit == "" && s.CPUQuota == nil && s.TasksLimit == nil
}
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func safeDeploymentFailure(d *model.Deployment) string {
	if d != nil {
		switch d.State {
		case model.BuildFailed:
			return "Build failed. Review the deployment logs."
		case model.StartFailed:
			return "App failed to start. Review the deployment logs."
		case model.Unhealthy:
			return "App did not pass its health check. Review the deployment logs."
		case model.OOMKilled:
			if d.SandboxType == "systemd" && d.MemoryLimit != "" {
				return "App exceeded its enforced memory limit."
			}
			return "Runtime was OOM-killed. Check host and application memory."
		case model.Crashed:
			return "App stopped unexpectedly. Review the deployment logs."
		case model.Stopped:
			return "Deployment stopped before it became healthy."
		}
	}
	return "Deployment failed. Review the deployment logs."
}
func cloneJob(j Job) Job {
	j.Logs = append([]string(nil), j.Logs...)
	if j.Deployment != nil {
		d := *j.Deployment
		j.Deployment = &d
	}
	if j.Source != nil {
		metadata := *j.Source
		j.Source = &metadata
	}
	return j
}

func cloneDeployRequest(request DeployRequest) DeployRequest {
	request.Environment = cloneStringMap(request.Environment)
	request.BuildEnvironment = cloneStringMap(request.BuildEnvironment)
	request.IncludePaths = append([]string(nil), request.IncludePaths...)
	request.IgnorePaths = append([]string(nil), request.IgnorePaths...)
	request.dedupKeys = append([]string(nil), request.dedupKeys...)
	return request
}

func removeID(values []string, id string) []string {
	out := values[:0]
	for _, value := range values {
		if value != id {
			out = append(out, value)
		}
	}
	return out
}
func tailString(value string, limit int) string {
	if len(value) > limit {
		start := len(value) - limit
		for start < len(value) && !utf8.RuneStart(value[start]) {
			start++
		}
		value = value[start:]
	}
	return strings.TrimSpace(value)
}

func (m *Manager) recordCheckoutTiming(id string, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record := m.jobs[id]; record != nil {
		elapsed := duration.Milliseconds()
		record.Job.CheckoutMilliseconds = &elapsed
		if err := m.persistLocked(); err != nil {
			log.Printf("persist checkout timing: %v", err)
		}
	}
}
