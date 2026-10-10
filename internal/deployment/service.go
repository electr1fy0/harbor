// Package deployment coordinates builds, processes, readiness, and routes.
package deployment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/health"
	"paas/internal/model"
	"paas/internal/router"
	"paas/internal/store"
)

type Options struct {
	DataDir     string
	RuntimeUser string
}

type Service struct {
	mu         sync.Mutex
	appLocksMu sync.Mutex
	appLocks   map[string]*sync.Mutex
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

func New(db *store.Store, b contracts.Builder, sandbox contracts.Sandbox, routes *router.HostRouter, opts Options) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(opts.DataDir, "logs"), 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		db:       db,
		builder:  b,
		sandbox:  sandbox,
		router:   routes,
		opts:     opts,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		pending:  make(map[string]pendingDeploy),
		appLocks: make(map[string]*sync.Mutex),
	}
	// Host processes are owned by this daemon session. Preserve history, but
	// never present a previous session's unverifiable PID as a live process.
	all, err := db.Deployments()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, d := range all {
		if d.State.Active() {
			d.State = model.Stopped
			d.Error = "daemon restarted; previous runtime session is not managed"
			now := time.Now().UTC()
			d.StoppedAt = &now
			if err := db.Finish(d, d.AppName+".localhost"); err != nil {
				cancel()
				return nil, err
			}
		}
	}
	go s.monitor()
	return s, nil
}

func (s *Service) lockApp(name string) func() {
	s.appLocksMu.Lock()
	mu, ok := s.appLocks[name]
	if !ok {
		mu = &sync.Mutex{}
		s.appLocks[name] = mu
	}
	s.appLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Service) recordEvent(deploymentID, appID string, eventType model.EventType, payload string) {
	_ = s.db.RecordEvent(deploymentID, appID, eventType, payload)
}

func (s *Service) retireDeployment(oldID, appName string, gracePeriod time.Duration) {
	oldDep, err := s.db.Deployment(oldID)
	if err != nil || !oldDep.State.Active() {
		return
	}
	s.recordEvent(oldDep.ID, oldDep.AppID, model.EventGracePeriodStarted, fmt.Sprintf("duration=%s", gracePeriod))
	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-timer.C:
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.sandbox.Stop(stopCtx, oldDep.ID)
	now := time.Now().UTC()
	oldDep.State = model.Stopped
	oldDep.StoppedAt = &now
	_ = s.db.UpdateDeployment(*oldDep)
	s.recordEvent(oldDep.ID, oldDep.AppID, model.EventOldDeploymentStopped, "")
}

func (s *Service) Deploy(ctx context.Context, sourceDir, name string) (*model.Deployment, error) {
	cfg, err := config.Load(sourceDir)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = cfg.Name
	}
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid application name %q", name)
	}

	unlockApp := s.lockApp(name)
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

	app, err := s.db.CreateApp(name)
	if err != nil {
		return nil, err
	}
	d := model.Deployment{ID: newID("dep"), AppID: app.ID, AppName: name, State: model.Queued, CreatedAt: time.Now().UTC()}
	d.LogPath = filepath.Join(s.opts.DataDir, "logs", d.ID+".log")
	if err = s.db.CreateDeployment(d); err != nil {
		return nil, err
	}
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
	if err = s.state(&d, model.Building); err != nil {
		return &d, err
	}
	s.recordEvent(d.ID, app.ID, model.EventBuildStarted, "")
	// Builds can take minutes. Do not block crash supervision or stop requests
	// for other applications while the builder runs.
	s.mu.Unlock()
	locked = false
	release, err := s.builder.Build(ctx, contracts.BuildRequest{
		SourceDir: sourceDir, AppName: name, Runtime: cfg.Runtime,
		BuildCommand: cfg.Build.Command, StartCommand: cfg.Start.Command, LogPath: d.LogPath,
	})
	s.mu.Lock()
	locked = true
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		s.recordEvent(d.ID, app.ID, model.EventBuildFailed, err.Error())
		return s.fail(&d, model.BuildFailed, err)
	}
	if err = s.db.SaveRelease(app.ID, release); err != nil {
		s.recordEvent(d.ID, app.ID, model.EventBuildFailed, err.Error())
		return s.fail(&d, model.BuildFailed, err)
	}
	s.recordEvent(d.ID, app.ID, model.EventBuildCompleted, fmt.Sprintf("release=%s hash=%s", release.ID, release.SourceHash))
	d.ReleaseID = release.ID
	if err = s.state(&d, model.Built); err != nil {
		return &d, err
	}
	if err = s.state(&d, model.Starting); err != nil {
		return &d, err
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
		d.SystemdUnit = fmt.Sprintf("paas-deploy-%s.service", d.ID)
		d.SandboxID = d.ID
		d.MemoryLimit = cfg.Resources.Memory
		d.CPUQuota = cfg.Resources.CPU
		d.TasksLimit = cfg.Resources.Tasks
		handle, err = s.sandbox.Start(ctx, contracts.SandboxSpec{
			ID: d.ID, Command: release.Command, Dir: release.ArtifactDir,
			Env:       []string{"PORT=" + strconv.Itoa(port), "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp"},
			User:      s.opts.RuntimeUser, LogPath: d.LogPath,
			MemoryMax: cfg.Resources.Memory,
			CPUQuota:  cfg.Resources.CPU,
			TasksMax:  cfg.Resources.Tasks,
		})
		if err != nil {
			info, _ := s.sandbox.Inspect(context.Background(), d.ID)
			if info != nil && (info.OOMKilled || info.Result == "oom-kill") {
				return s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
			}
			return s.fail(&d, model.StartFailed, err)
		}
		d.PID = handle.PID
		now := time.Now().UTC()
		d.StartedAt = &now
		s.recordEvent(d.ID, app.ID, model.EventSandboxStarted, fmt.Sprintf("pid=%d port=%d", d.PID, d.Port))
		if err = s.state(&d, model.HealthChecking); err != nil {
			_ = s.sandbox.Stop(context.Background(), d.ID)
			return &d, err
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
		info, inspectErr := s.sandbox.Inspect(context.Background(), d.ID)
		_ = s.sandbox.Stop(context.Background(), d.ID)
		if inspectErr == nil && (info.OOMKilled || info.Result == "oom-kill") {
			return s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
		}
		if inspectErr == nil && !info.Running && attempt < 2 && ctx.Err() == nil {
			d.State = model.Starting
			if updateErr := s.db.UpdateDeployment(d); updateErr != nil {
				return &d, updateErr
			}
			continue
		}
		if inspectErr == nil && !info.Running {
			return s.fail(&d, model.StartFailed, err)
		}
		return s.fail(&d, model.Unhealthy, err)
	}
	if err != nil {
		info, _ := s.sandbox.Inspect(context.Background(), d.ID)
		if info != nil && (info.OOMKilled || info.Result == "oom-kill") {
			return s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
		}
		return s.fail(&d, model.StartFailed, err)
	}
	info, err := s.sandbox.Inspect(ctx, d.ID)
	if err != nil || !info.Running {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		if info != nil && (info.OOMKilled || info.Result == "oom-kill") {
			return s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
		}
		return s.fail(&d, model.StartFailed, fmt.Errorf("process exited before route publication"))
	}
	s.recordEvent(d.ID, app.ID, model.EventHealthCheckPassed, "")
	d.State = model.Running
	d.Error = ""
	oldDeploymentID, err := s.db.Promote(d, name+".localhost")
	if err != nil {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, err)
	}
	if err = s.router.SetRoute(name+".localhost", "http://127.0.0.1:"+strconv.Itoa(d.Port)); err != nil {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, err)
	}
	s.recordEvent(d.ID, app.ID, model.EventRouteSwitched, fmt.Sprintf("upstream=http://127.0.0.1:%d", d.Port))
	s.recordEvent(d.ID, app.ID, model.EventDeploymentSucceeded, "")
	if oldDeploymentID != "" {
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			s.retireDeployment(oldDeploymentID, name, 1500*time.Millisecond)
		}()
	}
	return &d, nil
}

func (s *Service) Rollback(ctx context.Context, name, targetReleaseID string) (*model.Deployment, error) {
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid application name %q", name)
	}
	unlockApp := s.lockApp(name)
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

	var targetRelease *contracts.Release
	if targetReleaseID != "" {
		rel, err := s.db.Release(targetReleaseID)
		if err != nil {
			return nil, fmt.Errorf("release %s not found: %w", targetReleaseID, err)
		}
		targetRelease = rel
	} else {
		rel, err := s.db.PreviousRelease(name)
		if err != nil {
			return nil, err
		}
		targetRelease = rel
	}

	if _, err := os.Stat(targetRelease.ArtifactDir); err != nil {
		return nil, fmt.Errorf("release artifact missing at %s: %w", targetRelease.ArtifactDir, err)
	}

	app, err := s.db.CreateApp(name)
	if err != nil {
		return nil, err
	}

	memLimit := "128Mi"
	cpuQuota := 0.5
	tasksLimit := 64
	if prevDep, err := s.db.DeploymentForRelease(targetRelease.ID); err == nil && prevDep != nil {
		if prevDep.MemoryLimit != "" {
			memLimit = prevDep.MemoryLimit
		}
		if prevDep.CPUQuota > 0 {
			cpuQuota = prevDep.CPUQuota
		}
		if prevDep.TasksLimit > 0 {
			tasksLimit = prevDep.TasksLimit
		}
	}

	d := model.Deployment{
		ID:          newID("dep"),
		AppID:       app.ID,
		AppName:     name,
		ReleaseID:   targetRelease.ID,
		State:       model.Queued,
		CreatedAt:   time.Now().UTC(),
		MemoryLimit: memLimit,
		CPUQuota:    cpuQuota,
		TasksLimit:  tasksLimit,
	}
	d.LogPath = filepath.Join(s.opts.DataDir, "logs", d.ID+".log")
	if err = s.db.CreateDeployment(d); err != nil {
		return nil, err
	}

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

	s.recordEvent(d.ID, app.ID, model.EventRollbackTriggered, fmt.Sprintf("target_release=%s", targetRelease.ID))

	if err = s.state(&d, model.Built); err != nil {
		return &d, err
	}
	if err = s.state(&d, model.Starting); err != nil {
		return &d, err
	}
	s.recordEvent(d.ID, app.ID, model.EventSandboxStarting, "")

	var handle *contracts.SandboxHandle
	for attempt := 0; attempt < 3; attempt++ {
		port, portErr := allocatePort()
		if portErr != nil {
			return s.fail(&d, model.StartFailed, portErr)
		}
		d.Port = port
		d.SystemdUnit = fmt.Sprintf("paas-deploy-%s.service", d.ID)
		d.SandboxID = d.ID
		handle, err = s.sandbox.Start(ctx, contracts.SandboxSpec{
			ID:        d.ID,
			Command:   targetRelease.Command,
			Dir:       targetRelease.ArtifactDir,
			Env:       []string{"PORT=" + strconv.Itoa(port), "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp"},
			User:      s.opts.RuntimeUser,
			LogPath:   d.LogPath,
			MemoryMax: d.MemoryLimit,
			CPUQuota:  d.CPUQuota,
			TasksMax:  d.TasksLimit,
		})
		if err != nil {
			info, _ := s.sandbox.Inspect(context.Background(), d.ID)
			if info != nil && (info.OOMKilled || info.Result == "oom-kill") {
				return s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
			}
			return s.fail(&d, model.StartFailed, err)
		}
		d.PID = handle.PID
		now := time.Now().UTC()
		d.StartedAt = &now
		s.recordEvent(d.ID, app.ID, model.EventSandboxStarted, fmt.Sprintf("pid=%d port=%d", d.PID, d.Port))
		if err = s.state(&d, model.HealthChecking); err != nil {
			_ = s.sandbox.Stop(context.Background(), d.ID)
			return &d, err
		}
		s.mu.Unlock()
		locked = false
		checkErr := health.Check(ctx, port, "/health")
		if checkErr != nil && !strings.Contains(checkErr.Error(), "HTTP 5") {
			if tcpErr := health.Check(ctx, port, ""); tcpErr == nil {
				checkErr = nil
			}
		}
		err = checkErr
		s.mu.Lock()
		locked = true
		if err == nil {
			break
		}
		s.recordEvent(d.ID, app.ID, model.EventHealthCheckFailed, err.Error())
		info, inspectErr := s.sandbox.Inspect(context.Background(), d.ID)
		_ = s.sandbox.Stop(context.Background(), d.ID)
		if inspectErr == nil && (info.OOMKilled || info.Result == "oom-kill") {
			return s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
		}
		if inspectErr == nil && !info.Running && attempt < 2 && ctx.Err() == nil {
			d.State = model.Starting
			if updateErr := s.db.UpdateDeployment(d); updateErr != nil {
				return &d, updateErr
			}
			continue
		}
		if inspectErr == nil && !info.Running {
			return s.fail(&d, model.StartFailed, err)
		}
		return s.fail(&d, model.Unhealthy, err)
	}
	if err != nil {
		return s.fail(&d, model.StartFailed, err)
	}

	info, err := s.sandbox.Inspect(ctx, d.ID)
	if err != nil || !info.Running {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, fmt.Errorf("process exited before route publication"))
	}

	s.recordEvent(d.ID, app.ID, model.EventHealthCheckPassed, "")
	d.State = model.Running
	d.Error = ""

	oldDeploymentID, err := s.db.Promote(d, name+".localhost")
	if err != nil {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, err)
	}
	if err = s.router.SetRoute(name+".localhost", "http://127.0.0.1:"+strconv.Itoa(d.Port)); err != nil {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, err)
	}
	s.recordEvent(d.ID, app.ID, model.EventRouteSwitched, fmt.Sprintf("upstream=http://127.0.0.1:%d", d.Port))
	s.recordEvent(d.ID, app.ID, model.EventDeploymentSucceeded, "")

	if oldDeploymentID != "" {
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			s.retireDeployment(oldDeploymentID, name, 1500*time.Millisecond)
		}()
	}
	return &d, nil
}

func (s *Service) state(d *model.Deployment, state model.State) error {
	d.State = state
	return s.db.UpdateDeployment(*d)
}

func (s *Service) fail(d *model.Deployment, state model.State, cause error) (*model.Deployment, error) {
	d.State = state
	d.Error = cause.Error()
	now := time.Now().UTC()
	d.StoppedAt = &now
	s.recordEvent(d.ID, d.AppID, model.EventDeploymentFailed, cause.Error())
	active, _ := s.db.ActiveDeployment(d.AppName)
	if active != nil && active.ID == d.ID {
		s.router.Delete(d.AppName + ".localhost")
		if err := s.db.Finish(*d, d.AppName+".localhost"); err != nil {
			return d, errors.Join(cause, err)
		}
	} else {
		if err := s.db.UpdateDeployment(*d); err != nil {
			return d, errors.Join(cause, err)
		}
	}
	return d, cause
}

func (s *Service) Inspect(name string) (*model.Deployment, error) {
	if active, err := s.db.ActiveDeployment(name); err == nil {
		return active, nil
	}
	return s.db.LatestDeployment(name)
}

func (s *Service) Releases(name string) ([]model.ReleaseView, error) {
	return s.db.ReleasesForApp(name)
}

func (s *Service) Apps() ([]model.Application, error) { return s.db.Apps() }
func (s *Service) Logs(name string) (string, error) {
	d, err := s.Inspect(name)
	if err != nil {
		return "", err
	}
	if d.SystemdUnit != "" {
		args := []string{}
		if os.Geteuid() != 0 {
			args = append(args, "--user")
		}
		args = append(args, "-u", d.SystemdUnit, "--no-pager", "-n", "500", "-o", "cat")
		cmd := exec.Command("journalctl", args...)
		if out, err := cmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			return string(out), nil
		}
	}
	file, err := os.Open(d.LogPath)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.Inspect(name)
	if err != nil {
		return nil, err
	}
	for id, p := range s.pending {
		if p.appName == name && id != d.ID {
			p.cancel()
		}
	}
	if pending, ok := s.pending[d.ID]; ok {
		pending.cancel()
		s.mu.Unlock()
		select {
		case <-pending.done:
		case <-ctx.Done():
			s.mu.Lock()
			return d, ctx.Err()
		}
		s.mu.Lock()
		// Finish the deployment this stop actually canceled, not a newer
		// deployment admitted while we were waiting.
		d, err = s.db.Deployment(d.ID)
		if err != nil {
			return nil, err
		}
		d.State = model.Stopped
		d.Error = ""
		now := time.Now().UTC()
		d.StoppedAt = &now
		return d, s.db.Finish(*d, name+".localhost")
	}
	if !d.State.Active() {
		return d, nil
	}
	s.router.Delete(name + ".localhost")
	if err = s.sandbox.Stop(ctx, d.ID); err != nil {
		return d, err
	}
	d.State = model.Stopped
	d.Error = ""
	now := time.Now().UTC()
	d.StoppedAt = &now
	if all, listErr := s.db.Deployments(); listErr == nil {
		for _, other := range all {
			if other.AppName == name && other.ID != d.ID && other.State.Active() {
				_ = s.sandbox.Stop(ctx, other.ID)
				other.State = model.Stopped
				other.StoppedAt = &now
				_ = s.db.UpdateDeployment(other)
			}
		}
	}
	return d, s.db.Finish(*d, name+".localhost")
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
			// A build holds mu; it checks runtime exit itself before publication.
			if !s.mu.TryLock() {
				continue
			}
			all, err := s.db.Deployments()
			if err == nil {
				activeRoutes, _ := s.db.Routes()
				for _, d := range all {
					if d.State != model.Running {
						continue
					}
					// Only monitor crashes for deployments that are currently routed.
					if activeRoutes[d.AppName+".localhost"] != d.ID {
						continue
					}
					info, err := s.sandbox.Inspect(s.ctx, d.ID)
					if s.ctx.Err() != nil {
						break
					}
					if err != nil || !info.Running {
						_ = s.sandbox.Stop(context.Background(), d.ID)
						if info != nil && (info.OOMKilled || info.Result == "oom-kill") {
							_, _ = s.fail(&d, model.OomKilled, errors.New("memory limit exceeded (OOM killed)"))
						} else {
							message := "runtime process exited"
							if info != nil && info.ExitCode != nil {
								message = fmt.Sprintf("runtime process exited with code %d", *info.ExitCode)
							}
							_, _ = s.fail(&d, model.Crashed, errors.New(message))
						}
					}
				}
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
	all, err := s.db.Deployments()
	if err != nil {
		return err
	}
	var failures []error
	var resultMu sync.Mutex
	var stopped sync.WaitGroup
	for _, d := range all {
		if d.State.Active() {
			s.router.Delete(d.AppName + ".localhost")
			stopped.Add(1)
			go func(d model.Deployment) {
				defer stopped.Done()
				err := s.sandbox.Stop(context.Background(), d.ID)
				if err == nil {
					d.State = model.Stopped
					now := time.Now().UTC()
					d.StoppedAt = &now
					err = s.db.Finish(d, d.AppName+".localhost")
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

func allocatePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

func newID(prefix string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(value[:])
}
