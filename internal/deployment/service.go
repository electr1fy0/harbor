// Package deployment coordinates builds, processes, readiness, and routes.
package deployment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/health"
	"paas/internal/model"
	"paas/internal/router"
	"paas/internal/sandbox"
	"paas/internal/store"
)

type Options struct {
	DataDir     string
	RuntimeUser string
}

type Service struct {
	mu         sync.Mutex
	appLocksMu sync.Mutex
	appLocks   map[string]*appLock
	stopping   map[string]int
	retiring   map[string]bool
	inflight   sync.WaitGroup
	pending    map[string]pendingDeploy
	db         *store.Store
	builder    contracts.Builder
	sandbox    contracts.Sandbox
	router     *router.HostRouter
	opts       Options
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
}

type pendingDeploy struct {
	appName string
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(db *store.Store, b contracts.Builder, runtimeSandbox contracts.Sandbox, routes *router.HostRouter, opts Options) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(opts.DataDir, "logs"), 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		db: db, builder: b, sandbox: runtimeSandbox, router: routes, opts: opts,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		pending: make(map[string]pendingDeploy), appLocks: make(map[string]*appLock),
		stopping: make(map[string]int), retiring: make(map[string]bool),
	}
	// Host processes are owned by this daemon session. Preserve history, but
	// never present a previous session's unverifiable PID as a live process.
	all, err := db.UnfinishedDeployments("")
	if err != nil {
		cancel()
		return nil, err
	}
	for _, d := range all {
		if d.State.Active() || needsCleanup(d) {
			// Transient systemd services outlive the daemon. Reclaim them before
			// clearing the persisted active state, including legacy v2 records.
			if d.SystemdUnit != "" && (d.SandboxType == "systemd" || d.SandboxType == "" && runtime.GOOS == "linux") {
				if expected, unitErr := sandbox.SystemdUnitName(d.ID); unitErr != nil || expected != d.SystemdUnit {
					cancel()
					return nil, fmt.Errorf("invalid persisted systemd unit for deployment %s", d.ID)
				}
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 6*time.Second)
				stopErr := sandbox.StopSystemdDeployment(cleanupCtx, d.ID)
				cleanupCancel()
				if stopErr != nil {
					cancel()
					return nil, fmt.Errorf("reclaim previous runtime %s: %w", d.ID, stopErr)
				}
			}
			d.State = model.Stopped
			d.Error = "daemon restarted; previous runtime session is not managed"
			now := time.Now().UTC()
			d.StoppedAt = &now
			routes.Delete(d.AppName + ".localhost")
			if err := db.Finish(d, d.AppName+".localhost"); err != nil {
				cancel()
				return nil, err
			}
			_ = db.RecordEvent(d.ID, d.AppID, "ReconciledStopped", d.Error)
		}
	}
	go s.monitor()
	return s, nil
}

type appLock struct {
	token chan struct{}
	refs  int
}

// App operations serialize, but canceled callers and shutdown never wait for a
// long build to release a mutex. Reference counting bounds the lock registry.
func (s *Service) lockApp(ctx context.Context, name string) (func(), error) {
	s.appLocksMu.Lock()
	gate := s.appLocks[name]
	if gate == nil {
		gate = &appLock{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		s.appLocks[name] = gate
	}
	gate.refs++
	s.appLocksMu.Unlock()
	releaseRef := func() {
		s.appLocksMu.Lock()
		defer s.appLocksMu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.appLocks, name)
		}
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case <-s.ctx.Done():
		releaseRef()
		return nil, errors.New("daemon shutting down")
	case <-gate.token:
		return func() { gate.token <- struct{}{}; releaseRef() }, nil
	}
}

func (s *Service) recordEvent(deploymentID, appID string, eventType model.EventType, payload string) {
	if err := s.db.RecordEvent(deploymentID, appID, eventType, payload); err != nil {
		log.Printf("record lifecycle event %s for deployment %s: %v", eventType, deploymentID, err)
	}
}

const drainTimeout = 30 * time.Second

func (s *Service) retireDeployment(oldID string, drained <-chan struct{}) {
	defer func() { s.mu.Lock(); delete(s.retiring, oldID); s.mu.Unlock() }()
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-drained:
	case <-timer.C:
		log.Printf("drain deadline reached for deployment %s", oldID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Stop/monitor may have already finished this runtime while it drained.
	d, err := s.db.Deployment(oldID)
	if err != nil {
		log.Printf("read retiring deployment %s: %v", oldID, err)
		return
	}
	if !d.State.Active() {
		return
	}
	if err := s.finishRetirement(d); err != nil {
		log.Printf("retire deployment %s: %v", oldID, err)
	}
}

// finishRetirement runs under mu and keeps cleanup ownership on every failure.
func (s *Service) finishRetirement(d *model.Deployment) error {
	if err := s.stopRuntime(d.ID); err != nil {
		_, finishErr := s.fail(d, model.Stopped, fmt.Errorf("retire runtime: %w", err))
		return finishErr
	}
	d.State = model.Stopped
	now := time.Now().UTC()
	d.StoppedAt = &now
	if err := s.db.Finish(*d, d.AppName+".localhost"); err != nil {
		return err
	}
	s.recordEvent(d.ID, d.AppID, model.EventOldDeploymentStopped, "")
	return nil
}

func (s *Service) Deploy(ctx context.Context, sourceDir, name string) (*model.Deployment, error) {
	cfg, err := config.Load(sourceDir)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = cfg.Name
	}
	return s.deploy(ctx, sourceDir, name, cfg, nil)
}

func (s *Service) Rollback(ctx context.Context, name, targetReleaseID string) (*model.Deployment, error) {
	return s.deploy(ctx, "", name, nil, &targetReleaseID)
}

// deploy is the single candidate lifecycle for both builds and rollback.
func (s *Service) deploy(ctx context.Context, sourceDir, name string, cfg *config.Config, rollbackID *string) (*model.Deployment, error) {
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid application name %q", name)
	}
	unlockApp, err := s.lockApp(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlockApp()

	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if s.ctx.Err() != nil {
		return nil, fmt.Errorf("daemon shutting down")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.stopping[name] > 0 {
		return nil, fmt.Errorf("application %s is stopping", name)
	}
	all, err := s.db.UnfinishedDeployments(name)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		_, pending := s.pending[d.ID]
		if needsCleanup(d) || d.State.Active() && d.State != model.Running && !pending {
			return nil, fmt.Errorf("application %s still has runtime cleanup pending", name)
		}
	}
	var release *contracts.Release
	var app *model.Application
	if rollbackID == nil {
		app, err = s.db.CreateApp(name)
	} else {
		if *rollbackID == "" {
			release, err = s.db.PreviousRelease(name)
		} else {
			release, err = s.db.ReleaseForApp(name, *rollbackID)
		}
		if err != nil {
			return nil, err
		}
		prior, priorErr := s.db.DeploymentForRelease(release.ID)
		if priorErr != nil {
			return nil, priorErr
		}
		if !prior.ReadinessKnown {
			return nil, fmt.Errorf("release %s has no saved readiness configuration; deploy its source before rolling back", release.ID)
		}
		info, statErr := os.Stat(release.ArtifactDir)
		if statErr != nil {
			return nil, fmt.Errorf("inspect rollback artifact: %w", statErr)
		}
		if !info.IsDir() {
			return nil, errors.New("rollback artifact is not a directory")
		}
		app = &model.Application{ID: prior.AppID, Name: name}
		cfg = &config.Config{Resources: config.Resources{Memory: prior.MemoryLimit, CPU: prior.CPUQuota, Tasks: prior.TasksLimit}, Health: config.Health{Path: prior.HealthPath}}
		// Pre-resource-schema releases did not persist requests.
		if cfg.Resources.Memory == "" {
			cfg.Resources.Memory = config.DefaultMemory
		}
		if cfg.Resources.CPU <= 0 {
			cfg.Resources.CPU = config.DefaultCPU
		}
		if cfg.Resources.Tasks <= 0 {
			cfg.Resources.Tasks = config.DefaultTasks
		}
	}
	if err != nil {
		return nil, err
	}
	id, err := newID("dep")
	if err != nil {
		return nil, fmt.Errorf("generate deployment id: %w", err)
	}
	d := model.Deployment{ID: id, AppID: app.ID, AppName: name, State: model.Queued, CreatedAt: time.Now().UTC()}
	d.LogPath = filepath.Join(s.opts.DataDir, "logs", d.ID+".log")
	d.ReadinessKnown = true
	d.MemoryLimit, d.CPUQuota, d.TasksLimit, d.HealthPath = cfg.Resources.Memory, cfg.Resources.CPU, cfg.Resources.Tasks, cfg.Health.Path
	if err = s.db.CreateDeployment(d); err != nil {
		return nil, err
	}
	s.recordEvent(d.ID, app.ID, model.EventType(model.Queued), fmt.Sprintf("deployment queued for %s", name))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	unregister := context.AfterFunc(s.ctx, cancel)
	defer unregister()
	s.inflight.Add(1)
	pending := pendingDeploy{appName: name, cancel: cancel, done: make(chan struct{})}
	s.pending[d.ID] = pending
	defer func() {
		delete(s.pending, d.ID)
		close(pending.done)
		s.inflight.Done()
	}()
	if rollbackID == nil {
		if err = s.state(&d, model.Building); err != nil {
			return s.fail(&d, model.BuildFailed, err)
		}
		s.recordEvent(d.ID, app.ID, model.EventBuildStarted, "")
		// Long builds must not block supervision or other app operations.
		s.mu.Unlock()
		locked = false
		release, err = s.builder.Build(ctx, contracts.BuildRequest{
			SourceDir: sourceDir, AppName: name, Runtime: cfg.Runtime,
			BuildCommand: cfg.Build.Command, StartCommand: cfg.Start.Command, LogPath: d.LogPath,
		})
		s.mu.Lock()
		locked = true
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && release == nil {
			err = errors.New("builder returned no release")
		}
		if err == nil {
			err = s.db.SaveRelease(app.ID, release)
		}
		if err != nil {
			s.recordEvent(d.ID, app.ID, model.EventBuildFailed, err.Error())
			return s.fail(&d, model.BuildFailed, err)
		}
		s.recordEvent(d.ID, app.ID, model.EventBuildCompleted, fmt.Sprintf("release=%s hash=%s", release.ID, release.SourceHash))
	} else {
		s.recordEvent(d.ID, app.ID, model.EventRollbackTriggered, fmt.Sprintf("target_release=%s", release.ID))
	}
	d.ReleaseID = release.ID
	if err = s.state(&d, model.Built); err != nil {
		return s.fail(&d, model.StartFailed, err)
	}
	d.SandboxType = s.sandbox.Kind()
	d.SandboxID = d.ID
	if d.SandboxType == "systemd" {
		d.SystemdUnit, err = sandbox.SystemdUnitName(d.ID)
		if err != nil {
			return s.fail(&d, model.StartFailed, err)
		}
	}
	if err = s.state(&d, model.Starting); err != nil {
		return s.fail(&d, model.StartFailed, err)
	}
	s.recordEvent(d.ID, app.ID, model.EventSandboxStarting, "")
	// PORT-only apps cannot inherit a reserved listener. Retry a bounded number
	// of starts if a process fails during the loopback port handoff.
	var handle *contracts.SandboxHandle
	for attempt := 0; attempt < 3; attempt++ {
		port, portErr := allocatePort()
		if portErr != nil {
			return s.fail(&d, model.StartFailed, portErr)
		}
		d.Port = port
		defaultPath := "/usr/local/bin:/usr/bin:/bin"
		if home, err := os.UserHomeDir(); err == nil {
			defaultPath = filepath.Join(home, ".local", "bin") + ":" + defaultPath
		}
		deployEnv := []string{"PORT=" + strconv.Itoa(port), "PATH=" + defaultPath, "HOME=/tmp"}
		if bindings, err := s.db.GetBindingsForApp(app.ID); err == nil {
			for _, b := range bindings {
				if svc, err := s.db.GetService(b.ServiceID); err == nil && svc.Port > 0 {
					dbURL := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable",
						b.Username, b.Password, svc.Port, b.DatabaseName)
					deployEnv = append(deployEnv, b.EnvKey+"="+dbURL)
				}
			}
		}

		// Startup can wait on a runtime manager. Stop must be able to cancel
		// this candidate, and other applications must remain manageable.
		s.mu.Unlock()
		locked = false
		handle, err = s.sandbox.Start(ctx, contracts.SandboxSpec{
			ID: d.ID, Command: release.Command, Dir: release.ArtifactDir,
			Env:  deployEnv,
			User: s.opts.RuntimeUser, LogPath: d.LogPath,
			MemoryMax: cfg.Resources.Memory,
			CPUQuota:  cfg.Resources.CPU,
			TasksMax:  cfg.Resources.Tasks,
		})
		s.mu.Lock()
		locked = true
		if err != nil {
			info, inspectErr := s.inspectRuntime(d.ID)
			if isOOM(info) {
				return s.fail(&d, model.OOMKilled, errors.Join(errors.New("memory limit exceeded (OOM killed)"), err))
			}
			if errors.Is(inspectErr, sandbox.ErrSandboxNotFound) {
				inspectErr = nil
			}
			return s.fail(&d, model.StartFailed, errors.Join(err, inspectErr))
		}
		if handle == nil {
			return s.fail(&d, model.StartFailed, errors.New("sandbox returned no process handle"))
		}
		d.PID = handle.PID
		now := time.Now().UTC()
		d.StartedAt = &now
		s.recordEvent(d.ID, app.ID, model.EventSandboxStarted, fmt.Sprintf("pid=%d port=%d", d.PID, d.Port))
		if err = s.state(&d, model.HealthChecking); err != nil {
			return s.fail(&d, model.StartFailed, err)
		}
		s.mu.Unlock()
		locked = false
		err = health.Check(ctx, port, cfg.Health.Path)
		s.mu.Lock()
		locked = true
		if err == nil {
			break
		}
		s.recordEvent(d.ID, app.ID, model.EventHealthCheckFailed, err.Error())
		info, inspectErr := s.inspectRuntime(d.ID)
		stopErr := s.stopRuntime(d.ID)
		if stopErr != nil {
			return s.fail(&d, model.StartFailed, errors.Join(err, inspectErr, stopErr))
		}
		if inspectErr == nil && isOOM(info) {
			return s.fail(&d, model.OOMKilled, errors.New("memory limit exceeded (OOM killed)"))
		}
		if inspectErr == nil && info != nil && !info.Running && attempt < 2 && ctx.Err() == nil {
			d.State = model.Starting
			if updateErr := s.db.UpdateDeployment(d); updateErr != nil {
				return s.fail(&d, model.StartFailed, updateErr)
			}
			continue
		}
		if inspectErr == nil && info != nil && !info.Running {
			return s.fail(&d, model.StartFailed, err)
		}
		return s.fail(&d, model.Unhealthy, errors.Join(err, inspectErr))
	}
	if err := ctx.Err(); err != nil {
		return s.fail(&d, model.StartFailed, err)
	}
	info, err := s.inspectRuntime(d.ID)
	if err != nil || info == nil || !info.Running {
		if isOOM(info) {
			return s.fail(&d, model.OOMKilled, errors.New("memory limit exceeded (OOM killed)"))
		}
		return s.fail(&d, model.StartFailed, errors.Join(errors.New("process exited before route publication"), err))
	}
	s.recordEvent(d.ID, app.ID, model.EventHealthCheckPassed, "")
	d.State = model.Running
	d.Error = ""
	now := time.Now().UTC()
	d.PromotedAt = &now
	var oldDeploymentID string
	drained, err := s.router.SwitchRoute(name+".localhost", "http://127.0.0.1:"+strconv.Itoa(d.Port), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.ctx.Err() != nil {
			return errors.New("daemon shutting down")
		}
		var err error
		oldDeploymentID, err = s.db.Promote(d, name+".localhost")
		return err
	})
	if err != nil {
		d.PromotedAt = nil
		return s.fail(&d, model.StartFailed, err)
	}
	s.recordEvent(d.ID, app.ID, model.EventRouteSwitched, fmt.Sprintf("upstream=http://127.0.0.1:%d", d.Port))
	s.recordEvent(d.ID, app.ID, model.EventDeploymentSucceeded, "")
	if oldDeploymentID != "" {
		old, err := s.db.Deployment(oldDeploymentID)
		if err != nil {
			log.Printf("read replaced deployment %s: %v", oldDeploymentID, err)
		} else {
			s.recordEvent(old.ID, old.AppID, model.EventGracePeriodStarted, fmt.Sprintf("drain_timeout=%s", drainTimeout))
		}
		s.retiring[oldDeploymentID] = true
		s.inflight.Add(1)
		go func() { defer s.inflight.Done(); s.retireDeployment(oldDeploymentID, drained) }()
	}
	s.recordEvent(d.ID, app.ID, model.EventType(model.Running), fmt.Sprintf("route published at %s.localhost -> :%d", name, d.Port))
	return &d, nil
}

func (s *Service) state(d *model.Deployment, state model.State) error {
	d.State = state
	if err := s.db.UpdateDeployment(*d); err != nil {
		return err
	}
	s.recordEvent(d.ID, d.AppID, model.EventType(state), fmt.Sprintf("state changed to %s", state))
	return nil
}

func (s *Service) fail(d *model.Deployment, state model.State, cause error) (*model.Deployment, error) {
	// A failed deployment must not leave an unsupervised live runtime behind.
	var stopErr error
	if d.SandboxID != "" {
		stopErr = s.stopRuntime(d.SandboxID)
		cause = errors.Join(cause, stopErr)
	}
	d.State = state
	d.Error = cause.Error()
	d.StoppedAt = nil
	if stopErr == nil {
		now := time.Now().UTC()
		d.StoppedAt = &now
	}
	s.recordEvent(d.ID, d.AppID, model.EventDeploymentFailed, cause.Error())
	active, readErr := s.db.ActiveDeployment(d.AppName)
	if readErr != nil && !errors.Is(readErr, store.ErrNotFound) {
		return d, errors.Join(cause, readErr, s.db.UpdateDeployment(*d))
	}
	if active != nil && active.ID == d.ID {
		s.router.Delete(d.AppName + ".localhost")
	}
	if err := s.db.Finish(*d, d.AppName+".localhost"); err != nil {
		return d, errors.Join(cause, err)
	}
	s.recordEvent(d.ID, d.AppID, model.EventType(state), cause.Error())
	return d, cause
}

func (s *Service) Inspect(name string) (*model.Deployment, error) {
	if active, err := s.db.ActiveDeployment(name); err == nil {
		return active, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return s.db.LatestDeployment(name)
}

func (s *Service) Releases(name string) ([]model.ReleaseView, error) {
	return s.db.ReleasesForApp(name)
}

func (s *Service) Apps() ([]model.Application, error) { return s.db.Apps() }
func (s *Service) Logs(name string) (string, error) {
	// A failed candidate's diagnostics must remain accessible while inspection
	// and traffic continue to identify the active deployment.
	d, err := s.db.LatestDeployment(name)
	if err != nil {
		return "", err
	}
	buildLogs, err := logTail(d.LogPath)
	if err != nil {
		return "", err
	}
	if d.SystemdUnit == "" || d.SandboxType != "systemd" {
		return buildLogs, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	runtimeLogs, err := sandbox.JournalLogs(ctx, d.SystemdUnit)
	if err != nil {
		return "", err
	}
	logs := buildLogs + runtimeLogs
	if len(logs) > 1<<20 {
		logs = logs[len(logs)-(1<<20):]
	}
	return logs, nil
}

func logTail(path string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	// Bound responses and return the tail without loading unlimited logs.
	stat, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := stat.Size() - (1 << 20)
	if offset < 0 {
		offset = 0
	}
	if _, err = file.Seek(offset, 0); err != nil {
		return "", err
	}
	buf := make([]byte, 1<<20)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return string(buf[:n]), nil
}

func (s *Service) Stop(ctx context.Context, name string) (*model.Deployment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	d, err := s.Inspect(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	all, err := s.db.UnfinishedDeployments(name)
	if err != nil {
		s.mu.Unlock()
		return d, err
	}
	// Freeze admission while canceling and awaiting every candidate. Capture IDs
	// now so a stop never mutates a deployment admitted after this operation.
	s.stopping[name]++
	defer func() {
		s.mu.Lock()
		s.stopping[name]--
		if s.stopping[name] == 0 {
			delete(s.stopping, name)
		}
		s.mu.Unlock()
	}()
	targets := map[string]bool{d.ID: true}
	canceled := make(map[string]bool)
	var pendingDone []<-chan struct{}
	for _, other := range all {
		targets[other.ID] = true
	}
	for id, pending := range s.pending {
		if pending.appName == name {
			targets[id] = true
			canceled[id] = true
			pending.cancel()
			pendingDone = append(pendingDone, pending.done)
		}
	}
	s.mu.Unlock()
	for _, done := range pendingDone {
		select {
		case <-done:
		case <-ctx.Done():
			return d, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures []error
	for id := range targets {
		current, err := s.db.Deployment(id)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// A completed attempt owns no runtime. Repeated stop must preserve
		// its failure state, diagnostics, and original stop timestamp.
		if !current.State.Active() && !needsCleanup(*current) && !canceled[id] {
			continue
		}
		if current.SandboxID != "" && (current.State.Active() || needsCleanup(*current)) {
			if err := ctx.Err(); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := s.stopRuntime(current.SandboxID); err != nil {
				failures = append(failures, fmt.Errorf("stop deployment %s: %w", id, err))
				continue
			}
		}
		active, err := s.db.ActiveDeployment(name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			failures = append(failures, err)
			continue
		}
		if active != nil && active.ID == id {
			s.router.Delete(name + ".localhost")
		}
		current.State = model.Stopped
		now := time.Now().UTC()
		current.StoppedAt = &now
		if err := s.db.Finish(*current, name+".localhost"); err != nil {
			failures = append(failures, err)
		}
		s.recordEvent(current.ID, current.AppID, "Stopped", "deployment stopped")
		if id == d.ID {
			d = current
		}
	}
	return d, errors.Join(failures...)
}

func (s *Service) monitor() {
	defer close(s.done)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			// Skip a busy lifecycle transition; builds/startup/readiness release mu.
			if !s.mu.TryLock() {
				continue
			}
			all, err := s.db.UnfinishedDeployments("")
			if err == nil {
				activeRoutes, routeErr := s.db.Routes()
				if routeErr != nil {
					log.Printf("read routes for supervision: %v", routeErr)
					s.mu.Unlock()
					continue
				}
				for _, d := range all {
					if needsCleanup(d) {
						if err := s.stopRuntime(d.SandboxID); err != nil {
							log.Printf("retry runtime cleanup %s: %v", d.ID, err)
							continue
						}
						now := time.Now().UTC()
						d.StoppedAt = &now
						if activeRoutes[d.AppName+".localhost"] == d.ID {
							s.router.Delete(d.AppName + ".localhost")
						}
						if err := s.db.Finish(d, d.AppName+".localhost"); err != nil {
							log.Printf("record runtime cleanup %s: %v", d.ID, err)
						}
						continue
					}
					if d.State != model.Running {
						if _, pending := s.pending[d.ID]; d.State.Active() && !pending {
							if _, err := s.fail(&d, model.StartFailed, errors.New("deployment interrupted before promotion")); err != nil {
								log.Printf("finish interrupted deployment %s: %v", d.ID, err)
							}
						}
						continue
					}
					if activeRoutes[d.AppName+".localhost"] != d.ID && !s.retiring[d.ID] {
						if err := s.finishRetirement(&d); err != nil {
							log.Printf("retry retirement %s: %v", d.ID, err)
						}
						continue
					}
					info, err := s.inspectRuntime(d.ID)
					if s.ctx.Err() != nil {
						break
					}
					if err != nil && !errors.Is(err, sandbox.ErrSandboxNotFound) {
						log.Printf("inspect runtime %s: %v", d.ID, err)
						continue
					}
					if err != nil || info == nil || !info.Running {
						exitCode, result := "unknown", ""
						if info != nil {
							result = info.Result
							if info.ExitCode != nil {
								exitCode = strconv.Itoa(*info.ExitCode)
							}
						}
						s.recordEvent(d.ID, d.AppID, model.EventSandboxExited, fmt.Sprintf("exit_code=%s result=%s error=%v", exitCode, result, err))
						if isOOM(info) {
							if _, err := s.fail(&d, model.OOMKilled, errors.New("memory limit exceeded (OOM killed)")); err != nil {
								log.Printf("finish runtime %s: %v", d.ID, err)
							}
						} else {
							message := "runtime process exited"
							if info != nil && info.ExitCode != nil {
								message = fmt.Sprintf("runtime process exited with code %d", *info.ExitCode)
							}
							if _, err := s.fail(&d, model.Crashed, errors.New(message)); err != nil {
								log.Printf("finish runtime %s: %v", d.ID, err)
							}
						}
					}
				}
			} else {
				log.Printf("list deployments for supervision: %v", err)
			}
			s.mu.Unlock()
		}
	}
}

func (s *Service) Close() error {
	s.cancel()
	<-s.done
	// Synchronize with admission before Wait: no new deployment may Add after
	// the shutdown flag is visible under mu.
	s.mu.Lock()
	s.mu.Unlock()
	s.inflight.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.db.UnfinishedDeployments("")
	if err != nil {
		return err
	}
	var failures []error
	var resultMu sync.Mutex
	var stopped sync.WaitGroup
	for _, d := range all {
		if d.State.Active() || needsCleanup(d) {
			s.router.Delete(d.AppName + ".localhost")
			stopped.Add(1)
			go func(d model.Deployment) {
				defer stopped.Done()
				err := s.stopRuntime(d.ID)
				if err == nil {
					d.State = model.Stopped
					now := time.Now().UTC()
					d.StoppedAt = &now
					err = s.db.Finish(d, d.AppName+".localhost")
					_ = s.db.RecordEvent(d.ID, d.AppID, model.EventType(model.Stopped), "daemon shutdown; deployment stopped")
				}
				if err != nil {
					resultMu.Lock()
					failures = append(failures, err)
					resultMu.Unlock()
				}
			}(d)
		}
	}
	stopped.Wait()
	return errors.Join(failures...)
}

// A terminal deployment without a stop timestamp still owns a runtime whose
// cleanup failed. Keep supervising it and block new candidates until reclaimed.
func needsCleanup(d model.Deployment) bool {
	return !d.State.Active() && d.SandboxID != "" && d.StoppedAt == nil
}

func isOOM(info *contracts.SandboxInfo) bool {
	return info != nil && (info.OOMKilled || info.Result == "oom-kill")
}

func (s *Service) inspectRuntime(id string) (*contracts.SandboxInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return s.sandbox.Inspect(ctx, id)
}

func (s *Service) stopRuntime(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	err := s.sandbox.Stop(ctx, id)
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return nil
	}
	return err
}

func allocatePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}
