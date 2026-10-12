package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"paas/internal/config"
	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/source"
)

const maxOutstandingJobs = 8
const maxConcurrentInspections = 2

var (
	ErrQueueFull     = errors.New("deployment queue is full")
	ErrJobNotFound   = errors.New("deployment job was not found")
	ErrManagerClosed = errors.New("deployment manager is shutting down")
	ErrInspectBusy   = errors.New("project inspection is busy; try again shortly")
)

type sourceCheckout func(context.Context, string, string, string, string, string) (string, func(), error)

type ServiceManager interface {
	AddService(context.Context, string, string) (*model.Service, error)
	Link(context.Context, string, string) (*model.Binding, error)
	List(context.Context) ([]model.Service, error)
}

type DeployRequest struct {
	Name             string            `json:"name"`
	RepoURL          string            `json:"repoURL"`
	Ref              string            `json:"ref,omitempty"`
	RootDir          string            `json:"rootDir,omitempty"`
	Config           ConfigSelection   `json:"config,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	SqliteService    string            `json:"sqliteService,omitempty"`
	CreateSqlite     bool              `json:"createSqlite,omitempty"`
	preserveSettings bool
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
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	State      string         `json:"state"`
	Stage      string         `json:"stage"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	Deployment *JobDeployment `json:"deployment,omitempty"`
	Error      string         `json:"error,omitempty"`
	Logs       []string       `json:"logs"`
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
	buildUser    string
	ctx          context.Context
	cancel       context.CancelFunc
	queue        chan string
	workers      sync.WaitGroup
	mu           sync.Mutex
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
	m := &Manager{service: service, services: services, credentials: source.NewGitHubCredentialStore(dataDir, nil),
		sources: newSourceStore(dataDir), buildUser: buildUser, ctx: ctx, cancel: cancel,
		queue: make(chan string, maxOutstandingJobs), jobs: make(map[string]*jobRecord),
		inspectSlots: make(chan struct{}, maxConcurrentInspections), checkout: source.CheckoutGitHubAsWithToken}
	if service != nil {
		m.workers.Add(1)
		go m.worker()
	}
	return m
}

func (m *Manager) Connection() (source.GitHubConnection, error) { return m.credentials.Metadata() }
func (m *Manager) ConnectGitHub(ctx context.Context, token string) (source.GitHubConnection, error) {
	return m.credentials.Connect(ctx, token)
}
func (m *Manager) DisconnectGitHub() error      { return m.credentials.Disconnect() }
func (m *Manager) CanRedeploy(name string) bool { return m.sources.canRedeploy(name) }

func (m *Manager) Repositories(ctx context.Context) ([]source.GitHubRepository, error) {
	token, err := m.githubToken()
	if err != nil {
		return nil, err
	}
	return source.NewGitHubAPI().Repositories(ctx, token)
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
	settings, err := m.sources.get(name)
	if err != nil {
		return Job{}, ErrJobNotFound
	}
	if err := source.ValidateGitHubCheckoutRequest(settings.RepoURL, settings.Ref, settings.RootDir); err != nil {
		return Job{}, errors.New("saved repository settings are invalid")
	}
	if !selectionEmpty(settings.Selection) {
		if _, err := configFromSelection(name, settings.Selection); err != nil {
			return Job{}, errors.New("saved build settings are invalid")
		}
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	job, err := m.enqueue(DeployRequest{Name: name, RepoURL: settings.RepoURL, Ref: settings.Ref, RootDir: settings.RootDir, Config: settings.Selection, preserveSettings: true})
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
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Job{}, errors.New("could not create deployment job")
	}
	id, now := "job_"+hex.EncodeToString(raw[:]), time.Now().UTC()
	record := &jobRecord{Job: Job{ID: id, Name: request.Name, State: "QUEUED", Stage: "QUEUED", CreatedAt: now, UpdatedAt: now, Logs: []string{"Deployment queued."}}, request: request}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Job{}, ErrManagerClosed
	}
	m.jobs[id] = record
	m.order = append(m.order, id)
	select {
	case m.queue <- id:
		m.pruneLocked()
		return cloneJob(record.Job), nil
	default:
		delete(m.jobs, id)
		m.order = m.order[:len(m.order)-1]
		return Job{}, ErrQueueFull
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
	m.mu.Unlock()
	if job.Deployment != nil && m.service != nil {
		if logs, err := m.service.LogsForDeployment(job.Name, job.Deployment.ID); err == nil && logs != "" {
			job.Logs = []string{tailString(logs, 64<<10)}
		}
	}
	return job, nil
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
		if m.outstanding > 0 {
			m.outstanding--
		}
	} else {
		record.Job.State = "CANCELLING"
		record.Job.UpdatedAt = time.Now().UTC()
		record.cancel()
	}
	return cloneJob(record.Job), nil
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
	selected, cleanup, err := m.checkout(ctx, request.RepoURL, request.Ref, request.RootDir, m.buildUser, token)
	if err != nil {
		if ctx.Err() != nil {
			m.finish(id, "CANCELED", "CANCELED", "Deployment was canceled.", nil)
			return
		}
		m.finish(id, "FAILED", "FAILED", "Repository checkout failed. Check the URL, branch, and GitHub access.", nil)
		return
	}
	defer cleanup()
	inspection, err := Inspect(selected, request.Name, request.Config)
	if err != nil {
		m.finish(id, "FAILED", "FAILED", "Repository settings could not be prepared.", nil)
		return
	}
	selection := ConfigSelection{}
	storedSelection := ConfigSelection{}
	if !inspection.ExistingConfig {
		selection = selectionFromView(inspection.Config)
		storedSelection = selection
	}
	cfg, err := Prepare(selected, request.Name, selection)
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
		if err := m.sources.save(SourceSettings{Name: request.Name, RepoURL: request.RepoURL, Ref: request.Ref, RootDir: request.RootDir, Selection: storedSelection}); err != nil {
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
		d, err := m.service.DeployWithObserver(ctx, selected, request.Name, func(identity deployment.DeploymentIdentity) {
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
		j.Job.Logs = []string{tailString(logs, 64<<10)}
		j.Job.UpdatedAt = time.Now().UTC()
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
}
func (m *Manager) pruneLocked() {
	for len(m.order) > 100 {
		old := m.order[0]
		record := m.jobs[old]
		if record != nil && !record.terminal {
			break
		}
		delete(m.jobs, old)
		m.order = m.order[1:]
	}
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	for _, j := range m.jobs {
		if !j.terminal && j.cancel == nil {
			j.terminal = true
			j.Job.State = "CANCELED"
			j.Job.Stage = "CANCELED"
			j.Job.Error = "Deployment was canceled during shutdown."
			j.Job.UpdatedAt = time.Now().UTC()
			if m.outstanding > 0 {
				m.outstanding--
			}
		}
		if j.cancel != nil {
			j.cancel()
		}
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
	return j
}
func tailString(value string, limit int) string {
	if len(value) > limit {
		value = value[len(value)-limit:]
	}
	return strings.TrimSpace(value)
}
