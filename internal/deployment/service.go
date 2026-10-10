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
	"path/filepath"
	"strconv"
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
	mu       sync.Mutex
	inflight sync.WaitGroup
	pending  map[string]pendingDeploy
	db       *store.Store
	builder  contracts.Builder
	sandbox  contracts.Sandbox
	router   *router.HostRouter
	opts     Options
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

type pendingDeploy struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func New(db *store.Store, b contracts.Builder, sandbox contracts.Sandbox, routes *router.HostRouter, opts Options) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(opts.DataDir, "logs"), 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{db: db, builder: b, sandbox: sandbox, router: routes, opts: opts, ctx: ctx, cancel: cancel, done: make(chan struct{}), pending: make(map[string]pendingDeploy)}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, fmt.Errorf("daemon shutting down")
	}
	// Until replacement is implemented, an active app must be stopped first.
	existing, err := s.db.LatestDeployment(name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && existing.State.Active() {
		return nil, fmt.Errorf("application %s is active; stop it before deploying again", name)
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
	pending := pendingDeploy{cancel: cancel, done: make(chan struct{})}
	s.pending[d.ID] = pending
	defer func() {
		delete(s.pending, d.ID)
		close(pending.done)
		s.inflight.Done()
	}()
	if err = s.state(&d, model.Building); err != nil {
		return &d, err
	}
	// Builds can take minutes. Do not block crash supervision or stop requests
	// for other applications while the builder runs.
	s.mu.Unlock()
	release, err := s.builder.Build(ctx, contracts.BuildRequest{
		SourceDir: sourceDir, AppName: name, Runtime: cfg.Runtime,
		BuildCommand: cfg.Build.Command, StartCommand: cfg.Start.Command, LogPath: d.LogPath,
	})
	s.mu.Lock()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return s.fail(&d, model.BuildFailed, err)
	}
	if err = s.db.SaveRelease(app.ID, release); err != nil {
		return s.fail(&d, model.BuildFailed, err)
	}
	d.ReleaseID = release.ID
	if err = s.state(&d, model.Built); err != nil {
		return &d, err
	}
	if err = s.state(&d, model.Starting); err != nil {
		return &d, err
	}
	// PORT-only apps cannot inherit a reserved listener. Retry a bounded number
	// of starts if a process fails during the loopback port handoff.
	var handle *contracts.SandboxHandle
	for attempt := 0; attempt < 3; attempt++ {
		port, portErr := allocatePort()
		if portErr != nil {
			return s.fail(&d, model.StartFailed, portErr)
		}
		d.Port = port
		handle, err = s.sandbox.Start(ctx, contracts.SandboxSpec{
			ID: d.ID, Command: release.Command, Dir: release.ArtifactDir,
			Env:  []string{"PORT=" + strconv.Itoa(port), "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp"},
			User: s.opts.RuntimeUser, LogPath: d.LogPath,
		})
		if err != nil {
			return s.fail(&d, model.StartFailed, err)
		}
		d.PID = handle.PID
		now := time.Now().UTC()
		d.StartedAt = &now
		if err = s.state(&d, model.HealthChecking); err != nil {
			_ = s.sandbox.Stop(context.Background(), d.ID)
			return &d, err
		}
		s.mu.Unlock()
		err = health.Check(ctx, port, cfg.Health.Path)
		s.mu.Lock()
		if err == nil {
			break
		}
		info, inspectErr := s.sandbox.Inspect(context.Background(), d.ID)
		_ = s.sandbox.Stop(context.Background(), d.ID)
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
	d.State = model.Running
	d.Error = ""
	if err = s.db.Publish(d, name+".localhost"); err != nil {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, err)
	}
	if err = s.router.SetRoute(name+".localhost", "http://127.0.0.1:"+strconv.Itoa(d.Port)); err != nil {
		_ = s.sandbox.Stop(context.Background(), d.ID)
		return s.fail(&d, model.StartFailed, err)
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
	s.router.Delete(d.AppName + ".localhost")
	if err := s.db.Finish(*d, d.AppName+".localhost"); err != nil {
		return d, errors.Join(cause, err)
	}
	return d, cause
}

func (s *Service) Inspect(name string) (*model.Deployment, error) { return s.db.LatestDeployment(name) }
func (s *Service) Apps() ([]model.Application, error)             { return s.db.Apps() }
func (s *Service) Logs(name string) (string, error) {
	d, err := s.Inspect(name)
	if err != nil {
		return "", err
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
				for _, d := range all {
					if d.State != model.Running {
						continue
					}
					info, err := s.sandbox.Inspect(s.ctx, d.ID)
					if s.ctx.Err() != nil {
						break
					}
					if err != nil || !info.Running {
						_ = s.sandbox.Stop(context.Background(), d.ID)
						message := "runtime process exited"
						if info != nil && info.ExitCode != nil {
							message = fmt.Sprintf("runtime process exited with code %d", *info.ExitCode)
						}
						_, _ = s.fail(&d, model.Crashed, errors.New(message))
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
