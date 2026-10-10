package deployment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"
	"paas/internal/router"
	"paas/internal/store"
)

const testConfig = `name: demo
runtime: go
build:
  command: fake-build
start:
  command: fake-server
`

type buildBehavior struct {
	entered         chan struct{}
	cancelSeen      chan struct{}
	release         chan struct{}
	waitAfterCancel bool
	err             error
	enteredOnce     sync.Once
	cancelOnce      sync.Once
}

type blockedStartSandbox struct {
	contracts.Sandbox
	entered chan struct{}
}

func (s *blockedStartSandbox) Start(ctx context.Context, _ contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStopDuringSandboxStart(t *testing.T) {
	for _, sameApp := range []bool{true, false} {
		t.Run(fmt.Sprintf("same app=%v", sameApp), func(t *testing.T) {
			h := newServiceHarness(t, testConfig)
			name := "demo"
			if !sameApp {
				name = "other"
				if _, err := h.service.Deploy(context.Background(), h.source, name); err != nil {
					t.Fatal(err)
				}
			}
			blocked := &blockedStartSandbox{Sandbox: h.sandbox, entered: make(chan struct{})}
			h.service.mu.Lock()
			h.service.sandbox = blocked
			h.service.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deployDone := make(chan deployResult, 1)
			go func() {
				d, err := h.service.Deploy(ctx, h.source, "demo")
				deployDone <- deployResult{deployment: d, err: err}
			}()
			waitSignal(t, blocked.entered, "sandbox startup")
			stopDone := make(chan deployResult, 1)
			go func() {
				d, err := h.service.Stop(context.Background(), name)
				stopDone <- deployResult{deployment: d, err: err}
			}()
			select {
			case stopped := <-stopDone:
				if stopped.err != nil || stopped.deployment.State != model.Stopped {
					t.Errorf("Stop during startup = %+v", stopped)
				}
			case <-time.After(time.Second):
				t.Error("sandbox startup blocked Stop")
				cancel()
				awaitDeploy(t, stopDone)
			}
			cancel()
			if deployed := awaitDeploy(t, deployDone); !errors.Is(deployed.err, context.Canceled) {
				t.Errorf("startup cancellation = %v", deployed.err)
			}
		})
	}
}

func TestStopPreservesFinishedFailure(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compiler diagnostic")})
	failed, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil {
		t.Fatal("build unexpectedly succeeded")
	}
	stopped, err := h.service.Stop(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := h.db.Deployment(failed.ID)
	if err != nil || !reflect.DeepEqual(stopped, failed) || !reflect.DeepEqual(persisted, failed) {
		t.Fatalf("Stop rewrote completed failure: before=%+v returned=%+v stored=%+v error=%v", failed, stopped, persisted, err)
	}
}

func TestCanceledStopLeavesRuntimeUntouched(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	active, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	behavior := &buildBehavior{entered: make(chan struct{}), release: make(chan struct{})}
	h.builder.setBehavior("demo", behavior)
	done := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "demo")
		done <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "candidate build")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.service.Stop(ctx, "demo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Stop error = %v", err)
	}
	persisted, err := h.db.Deployment(active.ID)
	if err != nil || !reflect.DeepEqual(persisted, active) {
		t.Fatalf("canceled Stop changed runtime: %+v %v", persisted, err)
	}
	close(behavior.release)
	if candidate := awaitDeploy(t, done); candidate.err != nil {
		t.Fatalf("canceled Stop canceled candidate: %v", candidate.err)
	}
}

func TestHealthFailureIncludesInspectionError(t *testing.T) {
	h := newServiceHarness(t, testConfig+"health:\n  path: /ready\n")
	h.sandbox.ready = &atomic.Bool{}
	h.sandbox.inspectErr = errors.New("runtime manager unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	d, err := h.service.Deploy(ctx, h.source, "demo")
	if err == nil || !strings.Contains(err.Error(), "runtime manager unavailable") || !strings.Contains(d.Error, "runtime manager unavailable") {
		t.Fatalf("runtime inspection failure lost: %+v %v", d, err)
	}
}

type fakeBuilder struct {
	mu           sync.Mutex
	behaviors    map[string]*buildBehavior
	requests     []contracts.BuildRequest
	nextID       int
	artifactRoot string
}

func newFakeBuilder() *fakeBuilder {
	return &fakeBuilder{behaviors: make(map[string]*buildBehavior)}
}

func (b *fakeBuilder) setBehavior(app string, behavior *buildBehavior) {
	b.mu.Lock()
	b.behaviors[app] = behavior
	b.mu.Unlock()
}

func (b *fakeBuilder) Build(ctx context.Context, request contracts.BuildRequest) (*contracts.Release, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	b.nextID++
	id := b.nextID
	behavior := b.behaviors[request.AppName]
	b.mu.Unlock()

	if behavior != nil {
		if behavior.entered != nil {
			behavior.enteredOnce.Do(func() { close(behavior.entered) })
		}
		if behavior.release != nil {
			select {
			case <-ctx.Done():
				if behavior.cancelSeen != nil {
					behavior.cancelOnce.Do(func() { close(behavior.cancelSeen) })
				}
				if behavior.waitAfterCancel {
					<-behavior.release
				} else {
					return nil, ctx.Err()
				}
			case <-behavior.release:
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if behavior.err != nil {
			return nil, behavior.err
		}
	}

	artifactDir := filepath.Join(b.artifactRoot, fmt.Sprintf("artifact-%d", id))
	if err := os.MkdirAll(artifactDir, 0755); err != nil {
		return nil, err
	}

	return &contracts.Release{
		ID:          fmt.Sprintf("release-%d", id),
		SourceHash:  fmt.Sprintf("source-%d", id),
		Runtime:     "go",
		ArtifactDir: artifactDir,
		Command:     []string{"fake-server"},
		CreatedAt:   time.Now().UTC(),
	}, nil
}

func (b *fakeBuilder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

type fakeSandboxEntry struct {
	server *http.Server
	info   contracts.SandboxInfo
}

type fakeSandbox struct {
	mu           sync.Mutex
	entries      map[string]*fakeSandboxEntry
	started      chan string
	unreadyProbe chan struct{}
	ready        *atomic.Bool
	startErr     error
	stopErr      error
	inspectErr   error
	inspectCalls int
	specs        []contracts.SandboxSpec
	stopEntered  chan string
	stopRelease  chan struct{}
	slowEntered  chan struct{}
	slowRelease  chan struct{}
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{
		entries:      make(map[string]*fakeSandboxEntry),
		started:      make(chan string, 16),
		unreadyProbe: make(chan struct{}, 1),
	}
}

func (*fakeSandbox) Kind() string { return "host" }

func (s *fakeSandbox) Start(_ context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	if s.startErr != nil {
		return nil, s.startErr
	}
	port := 0
	for _, value := range spec.Env {
		if strings.HasPrefix(value, "PORT=") {
			var err error
			port, err = strconv.Atoi(strings.TrimPrefix(value, "PORT="))
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if port == 0 {
		return nil, errors.New("fake sandbox did not receive PORT")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" && s.slowEntered != nil {
			close(s.slowEntered)
			<-s.slowRelease
		}
		if r.URL.Path == "/ready" && s.ready != nil && !s.ready.Load() {
			select {
			case s.unreadyProbe <- struct{}{}:
			default:
			}
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "fake application response")
	})}

	s.mu.Lock()
	s.entries[spec.ID] = &fakeSandboxEntry{
		server: server,
		info:   contracts.SandboxInfo{ID: spec.ID, PID: 12345, Running: true},
	}
	s.mu.Unlock()
	go func() { _ = server.Serve(listener) }()
	s.started <- spec.ID
	return &contracts.SandboxHandle{ID: spec.ID, PID: 12345}, nil
}

func (s *fakeSandbox) Stop(_ context.Context, id string) error {
	if s.stopEntered != nil {
		s.stopEntered <- id
		<-s.stopRelease
	}
	s.mu.Lock()
	if s.stopErr != nil {
		err := s.stopErr
		s.mu.Unlock()
		return err
	}
	entry := s.entries[id]
	if entry != nil {
		entry.info.Running = false
	}
	s.mu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.server.Close()
}

func (s *fakeSandbox) Inspect(_ context.Context, id string) (*contracts.SandboxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspectCalls++
	if s.inspectErr != nil {
		return nil, s.inspectErr
	}
	entry := s.entries[id]
	if entry == nil {
		return nil, fmt.Errorf("unknown sandbox %q", id)
	}
	info := entry.info
	if info.ExitCode != nil {
		code := *info.ExitCode
		info.ExitCode = &code
	}
	return &info, nil
}

func (s *fakeSandbox) markExited(id string, code int) {
	s.mu.Lock()
	entry := s.entries[id]
	if entry != nil {
		entry.info.Running = false
		entry.info.ExitCode = &code
	}
	s.mu.Unlock()
}

func (s *fakeSandbox) markOOM(id string) {
	s.mu.Lock()
	entry := s.entries[id]
	if entry != nil {
		entry.info.Running = false
		entry.info.OOMKilled = true
		entry.info.Result = "oom-kill"
	}
	s.mu.Unlock()
}

func (s *fakeSandbox) close() {
	s.mu.Lock()
	servers := make([]*http.Server, 0, len(s.entries))
	for _, entry := range s.entries {
		servers = append(servers, entry.server)
	}
	s.mu.Unlock()
	for _, server := range servers {
		_ = server.Close()
	}
}

type serviceHarness struct {
	service *Service
	db      *store.Store
	router  *router.HostRouter
	builder *fakeBuilder
	sandbox *fakeSandbox
	source  string
	dbPath  string
}

func newServiceHarness(t *testing.T, contents string) *serviceHarness {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "paas.yaml"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	builder := newFakeBuilder()
	builder.artifactRoot = filepath.Join(root, "artifacts")
	sandbox := newFakeSandbox()
	routes := router.New()
	service, err := New(db, builder, sandbox, routes, Options{DataDir: filepath.Join(root, "data"), RuntimeUser: "nobody"})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = service.Close()
		sandbox.close()
		_ = db.Close()
	})
	return &serviceHarness{service: service, db: db, router: routes, builder: builder, sandbox: sandbox, source: source, dbPath: filepath.Join(root, "state.sqlite")}
}

func TestDeployPublishesRouteOnlyAfterHealthCheck(t *testing.T) {
	ready := &atomic.Bool{}
	h := newServiceHarness(t, strings.Replace(testConfig, "start:\n", "health:\n  path: /ready\nstart:\n", 1))
	h.sandbox.ready = ready

	result := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "")
		result <- deployResult{deployment: d, err: err}
	}()
	select {
	case <-h.sandbox.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox did not start")
	}
	waitSignal(t, h.sandbox.unreadyProbe, "an unsuccessful readiness probe")

	response := routerRequest(h.router, "demo.localhost", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("route response before health readiness = %d, want 404", response.Code)
	}
	ready.Store(true)

	got := awaitDeploy(t, result)
	if got.err != nil {
		t.Fatalf("Deploy() error = %v", got.err)
	}
	if got.deployment.State != model.Running {
		t.Fatalf("Deploy() state = %q, want %q", got.deployment.State, model.Running)
	}
	response = routerRequest(h.router, "demo.localhost", "/app")
	if response.Code != http.StatusOK || response.Body.String() != "fake application response" {
		t.Fatalf("routed response = %d %q, want 200 and application body", response.Code, response.Body.String())
	}
	routes, err := h.db.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if routes["demo.localhost"] != got.deployment.ID {
		t.Fatalf("persisted route = %q, want deployment %q", routes["demo.localhost"], got.deployment.ID)
	}
}

func TestDeployBuildAndStartFailures(t *testing.T) {
	t.Run("build failure", func(t *testing.T) {
		h := newServiceHarness(t, testConfig)
		h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compiler failed")})
		d, err := h.service.Deploy(context.Background(), h.source, "")
		if err == nil || !strings.Contains(err.Error(), "compiler failed") {
			t.Fatalf("Deploy() error = %v, want compiler failure", err)
		}
		if d.State != model.BuildFailed {
			t.Fatalf("deployment state = %q, want %q", d.State, model.BuildFailed)
		}
		assertNoRoute(t, h, "demo.localhost")
	})

	t.Run("start failure", func(t *testing.T) {
		h := newServiceHarness(t, testConfig)
		h.sandbox.startErr = errors.New("sandbox unavailable")
		d, err := h.service.Deploy(context.Background(), h.source, "")
		if err == nil || !strings.Contains(err.Error(), "sandbox unavailable") {
			t.Fatalf("Deploy() error = %v, want sandbox failure", err)
		}
		if d.State != model.StartFailed {
			t.Fatalf("deployment state = %q, want %q", d.State, model.StartFailed)
		}
		assertNoRoute(t, h, "demo.localhost")
	})
}

func TestDeployAllowsActiveApplicationRedeployAndSwapsRoute(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "")
	if err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}
	if first.State != model.Running {
		t.Fatalf("first Deploy() state = %s, want RUNNING", first.State)
	}

	second, err := h.service.Deploy(context.Background(), h.source, "")
	if err != nil {
		t.Fatalf("second Deploy() error = %v, want successful redeploy", err)
	}
	if second == nil || second.State != model.Running {
		t.Fatalf("second Deploy() state = %v, want RUNNING", second)
	}
	if second.ID == first.ID {
		t.Fatalf("second deployment ID = %s, want different from first %s", second.ID, first.ID)
	}

	// Verify route is swapped to second deployment
	routes, err := h.db.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if routes["demo.localhost"] != second.ID {
		t.Fatalf("route = %q, want second deployment %q", routes["demo.localhost"], second.ID)
	}

	// Wait for grace period and verify first deployment is retired
	waitFor(t, "first deployment graceful retirement", func() bool {
		dep, getErr := h.db.Deployment(first.ID)
		return getErr == nil && dep.State == model.Stopped
	})
}

func TestCandidateFailureLeavesActiveDeploymentUntouched(t *testing.T) {
	t.Run("candidate build failure", func(t *testing.T) {
		h := newServiceHarness(t, testConfig)
		first, err := h.service.Deploy(context.Background(), h.source, "")
		if err != nil {
			t.Fatalf("first Deploy() error = %v", err)
		}

		h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compiler syntax error")})
		second, err := h.service.Deploy(context.Background(), h.source, "")
		if err == nil || !strings.Contains(err.Error(), "compiler syntax error") {
			t.Fatalf("second Deploy() error = %v, want compiler error", err)
		}
		if second == nil || second.State != model.BuildFailed {
			t.Fatalf("second deployment state = %v, want BUILD_FAILED", second)
		}

		// First deployment must still be RUNNING and route intact!
		routes, err := h.db.Routes()
		if err != nil {
			t.Fatal(err)
		}
		if routes["demo.localhost"] != first.ID {
			t.Fatalf("route = %q, want first deployment %q untouched", routes["demo.localhost"], first.ID)
		}
		active, err := h.service.Inspect("demo")
		if err != nil || active.ID != first.ID || active.State != model.Running {
			t.Fatalf("active deployment = %#v, want first running deployment", active)
		}
	})

	t.Run("candidate health check failure", func(t *testing.T) {
		ready := &atomic.Bool{}
		ready.Store(true)
		h := newServiceHarness(t, strings.Replace(testConfig, "start:\n", "health:\n  path: /ready\nstart:\n", 1))
		h.sandbox.ready = ready

		first, err := h.service.Deploy(context.Background(), h.source, "")
		if err != nil {
			t.Fatalf("first Deploy() error = %v", err)
		}

		// Set ready to false so next candidate fails health check
		ready.Store(false)
		second, err := h.service.Deploy(context.Background(), h.source, "")
		if err == nil || !strings.Contains(err.Error(), "health check failed") {
			t.Fatalf("second Deploy() error = %v, want health check failure", err)
		}
		if second == nil || second.State != model.Unhealthy {
			t.Fatalf("second deployment state = %v, want UNHEALTHY", second)
		}

		// First deployment must still be RUNNING and route intact!
		routes, err := h.db.Routes()
		if err != nil {
			t.Fatal(err)
		}
		if routes["demo.localhost"] != first.ID {
			t.Fatalf("route = %q, want first deployment %q untouched", routes["demo.localhost"], first.ID)
		}
		active, err := h.service.Inspect("demo")
		if err != nil || active.ID != first.ID || active.State != model.Running {
			t.Fatalf("active deployment = %#v, want first running deployment", active)
		}
	})
}

func TestRollbackRestoresPreviousReleaseWithoutRebuild(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "")
	if err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}

	second, err := h.service.Deploy(context.Background(), h.source, "")
	if err != nil || second.State != model.Running {
		t.Fatalf("second Deploy() = %#v, %v; want running", second, err)
	}

	buildCountBefore := h.builder.count()

	// Rollback to previous release
	rollbackDep, err := h.service.Rollback(context.Background(), "demo", "")
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if rollbackDep.State != model.Running {
		t.Fatalf("rollback deployment state = %s, want RUNNING", rollbackDep.State)
	}
	if rollbackDep.ReleaseID != first.ReleaseID {
		t.Fatalf("rollback deployment release = %s, want first release %s", rollbackDep.ReleaseID, first.ReleaseID)
	}

	// Verify no builder invocation occurred!
	if h.builder.count() != buildCountBefore {
		t.Fatalf("builder called during rollback: count = %d, want %d", h.builder.count(), buildCountBefore)
	}

	// Verify route switched to rollback deployment
	routes, err := h.db.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if routes["demo.localhost"] != rollbackDep.ID {
		t.Fatalf("route = %q, want rollback deployment %q", routes["demo.localhost"], rollbackDep.ID)
	}
}

func TestRollbackIgnoresFailedCandidates(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "")
	if err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}

	second, err := h.service.Deploy(context.Background(), h.source, "")
	if err != nil || second.State != model.Running {
		t.Fatalf("second Deploy() = %#v, %v; want running", second, err)
	}

	// Trigger a failed candidate build
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compilation failed")})
	third, err := h.service.Deploy(context.Background(), h.source, "")
	if err == nil {
		t.Fatalf("third Deploy() unexpectedly succeeded: %#v", third)
	}

	// Clear builder error
	h.builder.setBehavior("demo", nil)

	// Rollback must restore first release, skipping the failed candidate
	rollbackDep, err := h.service.Rollback(context.Background(), "demo", "")
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if rollbackDep.State != model.Running {
		t.Fatalf("rollback deployment state = %s, want RUNNING", rollbackDep.State)
	}
	if rollbackDep.ReleaseID != first.ReleaseID {
		t.Fatalf("rollback selected release = %s, want first release %s", rollbackDep.ReleaseID, first.ReleaseID)
	}
}

func TestCrashSupervisionContinuesDuringAnotherBuild(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "alpha")
	if err != nil {
		t.Fatalf("Deploy(alpha) error = %v", err)
	}
	behavior := &buildBehavior{entered: make(chan struct{}), release: make(chan struct{})}
	h.builder.setBehavior("beta", behavior)
	secondResult := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "beta")
		secondResult <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "second build start")
	h.sandbox.markExited(first.ID, 17)

	waitFor(t, "first app crash supervision", func() bool {
		d, inspectErr := h.service.Inspect("alpha")
		return inspectErr == nil && d.State == model.Crashed && strings.Contains(d.Error, "code 17")
	})
	assertNoRoute(t, h, "alpha.localhost")

	close(behavior.release)
	second := awaitDeploy(t, secondResult)
	if second.err != nil || second.deployment.State != model.Running {
		t.Fatalf("Deploy(beta) = %#v, %v; want running", second.deployment, second.err)
	}
}

func TestCrashSupervisionContinuesDuringBlockedHealthCheck(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)
	healthConfig := testConfig + "health:\n  path: /ready\n"
	h := newServiceHarness(t, healthConfig)
	h.sandbox.ready = ready
	first, err := h.service.Deploy(context.Background(), h.source, "alpha")
	if err != nil {
		t.Fatalf("Deploy(alpha) error = %v", err)
	}

	ready.Store(false)
	secondResult := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "beta")
		secondResult <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, h.sandbox.unreadyProbe, "beta health check to block on readiness")
	h.sandbox.markExited(first.ID, 23)
	waitFor(t, "alpha crash supervision during beta health check", func() bool {
		d, inspectErr := h.service.Inspect("alpha")
		return inspectErr == nil && d.State == model.Crashed && strings.Contains(d.Error, "code 23")
	})
	assertNoRoute(t, h, "alpha.localhost")

	ready.Store(true)
	second := awaitDeploy(t, secondResult)
	if second.err != nil || second.deployment.State != model.Running {
		t.Fatalf("Deploy(beta) = %#v, %v; want running after readiness", second.deployment, second.err)
	}
}

func TestCrashSupervisionClassifiesOOM(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	d, err := h.service.Deploy(context.Background(), h.source, "oomapp")
	if err != nil {
		t.Fatalf("Deploy(oomapp) error = %v", err)
	}

	h.sandbox.markOOM(d.ID)

	waitFor(t, "oomapp crash supervision classifies OOM", func() bool {
		latest, inspectErr := h.service.Inspect("oomapp")
		return inspectErr == nil && latest.State == model.OOMKilled && strings.Contains(latest.Error, "OOM")
	})
	assertNoRoute(t, h, "oomapp.localhost")
}

func TestStopCancelsBuildAndWaitsForDeployment(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	behavior := &buildBehavior{entered: make(chan struct{}), release: make(chan struct{})}
	h.builder.setBehavior("demo", behavior)
	deployResultCh := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "")
		deployResultCh <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "build start")

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stopped, err := h.service.Stop(stopCtx, "demo")
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if stopped.State != model.Stopped {
		t.Fatalf("Stop() state = %q, want %q", stopped.State, model.Stopped)
	}
	build := awaitDeploy(t, deployResultCh)
	if !errors.Is(build.err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want context cancellation", build.err)
	}
	latest, err := h.service.Inspect("demo")
	if err != nil || latest.State != model.Stopped {
		t.Fatalf("latest deployment = %#v, %v; want stopped", latest, err)
	}
	assertNoRoute(t, h, "demo.localhost")
}

func TestStopCancelsHealthCheck(t *testing.T) {
	ready := &atomic.Bool{}
	healthConfig := testConfig + "health:\n  path: /ready\n"
	h := newServiceHarness(t, healthConfig)
	h.sandbox.ready = ready
	deployResultCh := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "")
		deployResultCh <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, h.sandbox.unreadyProbe, "health check to block on readiness")

	stopResult := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Stop(context.Background(), "demo")
		stopResult <- deployResult{deployment: d, err: err}
	}()
	var stopped deployResult
	select {
	case stopped = <-stopResult:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not cancel the blocked health check")
	}
	if stopped.err != nil || stopped.deployment.State != model.Stopped {
		t.Fatalf("Stop() = %#v, %v; want stopped", stopped.deployment, stopped.err)
	}
	build := awaitDeploy(t, deployResultCh)
	if !errors.Is(build.err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want health-check cancellation", build.err)
	}
	latest, err := h.service.Inspect("demo")
	if err != nil || latest.State != model.Stopped {
		t.Fatalf("latest deployment = %#v, %v; want stopped", latest, err)
	}
	assertNoRoute(t, h, "demo.localhost")
}

func TestStopFinishesCanceledDeploymentByID(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	behavior := &buildBehavior{
		entered:         make(chan struct{}),
		cancelSeen:      make(chan struct{}),
		release:         make(chan struct{}),
		waitAfterCancel: true,
	}
	h.builder.setBehavior("demo", behavior)
	deployResultCh := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "")
		deployResultCh <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "build start")

	stopResult := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Stop(context.Background(), "demo")
		stopResult <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.cancelSeen, "Stop() to cancel the original deployment")
	original, err := h.service.Inspect("demo")
	if err != nil {
		t.Fatal(err)
	}
	originalID := original.ID

	// A newer row forces the stop-wait window deterministically. Deploy's
	// active-app check means this interleaving is best isolated at persistence.
	apps, err := h.service.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %#v, %v; want the original app", apps, err)
	}
	newer := model.Deployment{
		ID:        "dep-newer-during-stop",
		AppID:     apps[0].ID,
		AppName:   "demo",
		State:     model.Queued,
		CreatedAt: time.Now().Add(time.Hour).UTC(),
		LogPath:   filepath.Join(t.TempDir(), "newer.log"),
	}
	if err := h.db.CreateDeployment(newer); err != nil {
		t.Fatalf("insert newer deployment for race window: %v", err)
	}
	close(behavior.release)

	var stopped deployResult
	select {
	case stopped = <-stopResult:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not finish")
	}
	if stopped.err != nil || stopped.deployment == nil {
		t.Fatalf("Stop() = %#v, %v", stopped.deployment, stopped.err)
	}
	build := awaitDeploy(t, deployResultCh)
	if !errors.Is(build.err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want context cancellation", build.err)
	}
	original, err = h.db.Deployment(originalID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.deployment.ID != originalID || build.deployment.ID != originalID || original.State != model.Stopped {
		t.Fatalf("Stop() finalized %q; original %q is %q", stopped.deployment.ID, original.ID, original.State)
	}
	latest, err := h.db.Deployment(newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != model.Queued {
		t.Fatalf("newer deployment state = %q, want untouched %q", latest.State, model.Queued)
	}
}

func TestCloseCancelsAndWaitsForBuild(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	behavior := &buildBehavior{
		entered:         make(chan struct{}),
		cancelSeen:      make(chan struct{}),
		release:         make(chan struct{}),
		waitAfterCancel: true,
	}
	h.builder.setBehavior("demo", behavior)
	deployResultCh := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "")
		deployResultCh <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "build start")

	closeResult := make(chan error, 1)
	go func() { closeResult <- h.service.Close() }()
	waitSignal(t, behavior.cancelSeen, "build cancellation")
	select {
	case err := <-closeResult:
		t.Fatalf("Close() returned while builder remained blocked: %v", err)
	default:
	}
	close(behavior.release)
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not finish after the builder was released")
	}
	build := awaitDeploy(t, deployResultCh)
	if !errors.Is(build.err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want context cancellation", build.err)
	}
	latest, err := h.service.Inspect("demo")
	if err != nil || latest.State != model.BuildFailed {
		t.Fatalf("latest deployment = %#v, %v; want canceled build recorded as failed", latest, err)
	}
}

func TestCloseStopsIndependentAppsConcurrently(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := h.service.Deploy(context.Background(), h.source, name); err != nil {
			t.Fatal(err)
		}
	}
	h.sandbox.stopEntered = make(chan string, 2)
	h.sandbox.stopRelease = make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(h.sandbox.stopRelease) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- h.service.Close() }()
	for range 2 {
		select {
		case <-h.sandbox.stopEntered:
		case <-time.After(2 * time.Second):
			release()
			<-done
			t.Fatal("shutdown serialized independent process stops")
		}
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type deployResult struct {
	deployment *model.Deployment
	err        error
}

func routerRequest(routes http.Handler, host, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	request.Host = host
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, request)
	return response
}

func assertNoRoute(t *testing.T, h *serviceHarness, host string) {
	t.Helper()
	response := routerRequest(h.router, host, "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("route %q returned %d, want 404", host, response.Code)
	}
	routes, err := h.db.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := routes[host]; exists {
		t.Fatalf("persisted route %q unexpectedly exists: %#v", host, routes[host])
	}
}

func awaitDeploy(t *testing.T, result <-chan deployResult) deployResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("Deploy() did not finish")
		return deployResult{}
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDeploymentPersistsActualBackendAndPassesConfiguredLimits(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	d, err := h.service.Deploy(context.Background(), h.source, "metadata")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := h.service.Inspect("metadata")
	if err != nil || latest.SandboxType != "host" || latest.SystemdUnit != "" || latest.SandboxID != d.ID {
		t.Fatalf("fictitious runtime metadata: %+v %v", latest, err)
	}
	h.sandbox.mu.Lock()
	spec := h.sandbox.specs[0]
	h.sandbox.mu.Unlock()
	if spec.MemoryMax != "128Mi" || spec.CPUQuota != 0.5 || spec.TasksMax != 64 || spec.User != "nobody" {
		t.Fatalf("runtime configuration lost: %+v", spec)
	}
}

func TestTransientInspectionFailureDoesNotKillHealthyDeployment(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	if _, err := h.service.Deploy(context.Background(), h.source, "transient"); err != nil {
		t.Fatal(err)
	}
	h.sandbox.mu.Lock()
	h.sandbox.inspectErr = errors.New("temporary manager outage")
	calls := h.sandbox.inspectCalls
	h.sandbox.mu.Unlock()
	waitFor(t, "monitor to see temporary inspection error", func() bool { h.sandbox.mu.Lock(); defer h.sandbox.mu.Unlock(); return h.sandbox.inspectCalls > calls })
	d, err := h.service.Inspect("transient")
	if err != nil || d.State != model.Running {
		t.Fatalf("healthy app killed on inspection error: %+v %v", d, err)
	}
	h.sandbox.mu.Lock()
	h.sandbox.inspectErr = nil
	h.sandbox.mu.Unlock()
}

func TestFailedCleanupIsPersistedRetriedAndBlocksReplacement(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	h.sandbox.startErr = errors.New("startup failure")
	h.sandbox.mu.Lock()
	h.sandbox.stopErr = errors.New("stop failure")
	h.sandbox.mu.Unlock()
	d, err := h.service.Deploy(context.Background(), h.source, "cleanup")
	if err == nil || !strings.Contains(err.Error(), "startup failure") || !strings.Contains(err.Error(), "stop failure") || d.StoppedAt != nil {
		t.Fatalf("cleanup failure swallowed: %+v %v", d, err)
	}
	if _, err := h.service.Deploy(context.Background(), h.source, "cleanup"); err == nil {
		t.Fatal("replacement admitted before cleanup")
	}
	h.sandbox.mu.Lock()
	h.sandbox.stopErr = nil
	h.sandbox.mu.Unlock()
	waitFor(t, "cleanup retry to record stop", func() bool { d, err := h.service.Inspect("cleanup"); return err == nil && d.StoppedAt != nil })
}

func TestRestartReclaimsSystemdRuntimeBeforeClearingState(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop failure=%v", fail), func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "stopped")
			t.Setenv("HARBOR_RECOVERY_MARKER", marker)
			script := `#!/bin/sh
case " $* " in
 *" show "*)
  if [ -f "$HARBOR_RECOVERY_MARKER" ]; then
   printf 'LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\n'
   exit 4
  fi
  printf 'LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=123\nExecMainCode=0\nResult=success\n'
  ;;
 *" stop "*) touch "$HARBOR_RECOVERY_MARKER" ;;
esac
`
			if fail {
				script = "#!/bin/sh\necho manager-unavailable >&2\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			db, err := store.Open(filepath.Join(root, "paas.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			app, err := db.CreateApp("restart")
			if err != nil {
				t.Fatal(err)
			}
			d := model.Deployment{ID: "dep_restart", AppID: app.ID, AppName: app.Name, State: model.Starting, CreatedAt: time.Now().UTC(), SandboxType: "systemd", SandboxID: "dep_restart", SystemdUnit: "paas-deploy-dep_restart.service"}
			if err := db.CreateDeployment(d); err != nil {
				t.Fatal(err)
			}
			service, err := New(db, &fakeBuilder{}, newFakeSandbox(), router.New(), Options{DataDir: root})
			if fail {
				if err == nil {
					service.Close()
					t.Fatal("startup accepted unreclaimed unit")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer service.Close()
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("unit not reclaimed: %v", err)
				}
			}
			latest, err := db.Deployment(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fail && latest.State != model.Starting || !fail && latest.State != model.Stopped {
				t.Fatalf("incorrect recovery state: %+v", latest)
			}
		})
	}
}

func TestSystemdLogsIncludeBuildOutputAndSurfaceJournalErrors(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	d, err := h.service.Deploy(context.Background(), h.source, "logs")
	if err != nil {
		t.Fatal(err)
	}
	d.SandboxType = "systemd"
	d.SystemdUnit = "paas-deploy-" + d.ID + ".service"
	if err := h.db.UpdateDeployment(*d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.LogPath, []byte("build output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	toolsDir := t.TempDir()
	journal := filepath.Join(toolsDir, "journalctl")
	if err := os.WriteFile(journal, []byte("#!/bin/sh\nprintf 'runtime stdout\\nruntime stderr\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	logs, err := h.service.Logs("logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"build output", "runtime stdout", "runtime stderr"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
	if err := os.WriteFile(journal, []byte("#!/bin/sh\necho journal-unavailable >&2\nexit 3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Logs("logs"); err == nil || !strings.Contains(err.Error(), "journal-unavailable") {
		t.Fatalf("journal failure silently swallowed: %v", err)
	}
}

func TestRollbackCannotUseAnotherApplicationsRelease(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	foreign, err := h.service.Deploy(context.Background(), h.source, "foreign")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo", "foreign-copy"} {
		if d, err := h.service.Rollback(context.Background(), name, foreign.ReleaseID); !errors.Is(err, store.ErrNotFound) || d != nil {
			t.Fatalf("cross-app rollback accepted: %+v %v", d, err)
		}
	}
	apps, err := h.db.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("rollback created an app: %+v %v", apps, err)
	}
	active, err := h.service.Inspect("foreign")
	if err != nil || active.ID != foreign.ID {
		t.Fatalf("foreign app disturbed: %+v %v", active, err)
	}
}

func TestRollbackUsesSavedReadinessAndResources(t *testing.T) {
	cfg := testConfig + "health:\n  path: /ready\nresources:\n  memory: 64Mi\n  cpu: 0.25\n  tasks: 16\n"
	h := newServiceHarness(t, cfg)
	ready := &atomic.Bool{}
	ready.Store(true)
	h.sandbox.ready = ready
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	ready.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	failed, err := h.service.Rollback(ctx, "demo", first.ReleaseID)
	if err == nil || failed == nil || failed.State == model.Running {
		t.Fatalf("unready rollback promoted: %+v %v", failed, err)
	}
	h.sandbox.mu.Lock()
	spec := h.sandbox.specs[len(h.sandbox.specs)-1]
	h.sandbox.mu.Unlock()
	if spec.MemoryMax != "64Mi" || spec.CPUQuota != .25 || spec.TasksMax != 16 || failed.HealthPath != "/ready" || failed.SandboxType != "host" || failed.SystemdUnit != "" {
		t.Fatalf("rollback lost settings: %+v %+v", failed, spec)
	}
	active, err := h.service.Inspect("demo")
	if err != nil || active.ID != second.ID {
		t.Fatalf("rollback failure replaced live app: %+v %v", active, err)
	}
}

func TestFailedPromotionPreservesBothRoutes(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite3", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TRIGGER reject_promotion BEFORE INSERT ON routes
		BEGIN SELECT RAISE(ABORT, 'disk write failed'); END`); err != nil {
		t.Fatal(err)
	}
	candidate, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil || candidate == nil || candidate.PromotedAt != nil || candidate.State != model.StartFailed {
		t.Fatalf("bad promotion result: %+v %v", candidate, err)
	}
	active, err := h.db.ActiveDeployment("demo")
	if err != nil || active.ID != first.ID {
		t.Fatalf("durable route lost: %+v %v", active, err)
	}
	response := routerRequest(h.router, "demo.localhost", "/")
	if response.Code != http.StatusOK {
		t.Fatalf("live proxy route lost: %d", response.Code)
	}
	info, err := h.sandbox.Inspect(context.Background(), candidate.ID)
	if err != nil || info.Running {
		t.Fatalf("failed candidate still running: %+v %v", info, err)
	}
}

func TestRedeployDrainsInFlightRequestBeforeStoppingRuntime(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	h.sandbox.slowEntered = make(chan struct{})
	h.sandbox.slowRelease = make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(h.sandbox.slowRelease) }) }
	t.Cleanup(releaseRequest)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { requestDone <- routerRequest(h.router, "demo.localhost", "/slow") }()
	waitSignal(t, h.sandbox.slowEntered, "in-flight old request")
	second, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	// Prove a real request survives the previous fixed 1.5-second cutoff.
	time.Sleep(1700 * time.Millisecond)
	info, err := h.sandbox.Inspect(context.Background(), first.ID)
	if err != nil || !info.Running {
		t.Fatalf("old runtime killed before request drained: %+v %v", info, err)
	}
	active, err := h.service.Inspect("demo")
	if err != nil || active.ID != second.ID {
		t.Fatalf("candidate not routed: %+v %v", active, err)
	}
	releaseRequest()
	select {
	case response := <-requestDone:
		if response.Code != http.StatusOK {
			t.Fatalf("old request interrupted: %d", response.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not complete")
	}
	waitFor(t, "old runtime stopped after draining", func() bool {
		d, err := h.db.Deployment(first.ID)
		return err == nil && d.State == model.Stopped && d.StoppedAt != nil
	})
}

func TestStopCancelsCandidateAndStopsActiveRuntime(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	behavior := &buildBehavior{entered: make(chan struct{}), cancelSeen: make(chan struct{}), release: make(chan struct{})}
	h.builder.setBehavior("demo", behavior)
	candidateResult := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "demo")
		candidateResult <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "candidate build")
	stopped, err := h.service.Stop(context.Background(), "demo")
	if err != nil || stopped.ID != first.ID || stopped.State != model.Stopped {
		t.Fatalf("stop active: %+v %v", stopped, err)
	}
	candidate := awaitDeploy(t, candidateResult)
	if !errors.Is(candidate.err, context.Canceled) {
		t.Fatalf("candidate was not canceled: %+v", candidate)
	}
	d, err := h.db.Deployment(candidate.deployment.ID)
	if err != nil || d.State != model.Stopped {
		t.Fatalf("candidate not stopped: %+v %v", d, err)
	}
	assertNoRoute(t, h, "demo.localhost")
}

func TestWaitingCandidateHonorsCancellationAndReleasesAppLock(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	behavior := &buildBehavior{entered: make(chan struct{}), release: make(chan struct{})}
	h.builder.setBehavior("demo", behavior)
	firstResult := make(chan deployResult, 1)
	go func() {
		d, err := h.service.Deploy(context.Background(), h.source, "demo")
		firstResult <- deployResult{deployment: d, err: err}
	}()
	waitSignal(t, behavior.entered, "first build")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := h.service.Deploy(ctx, h.source, "demo"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller ignored cancellation: %v", err)
	}
	close(behavior.release)
	first := awaitDeploy(t, firstResult)
	if first.err != nil {
		t.Fatal(first.err)
	}
	h.service.appLocksMu.Lock()
	count := len(h.service.appLocks)
	h.service.appLocksMu.Unlock()
	if count != 0 {
		t.Fatalf("unused app locks retained: %d", count)
	}
}

func TestFailedRetirementRetainsCleanupOwnership(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	h.sandbox.mu.Lock()
	h.sandbox.stopErr = errors.New("retirement denied")
	h.sandbox.mu.Unlock()
	second, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retirement failure recorded", func() bool {
		d, err := h.db.Deployment(first.ID)
		return err == nil && needsCleanup(*d) && strings.Contains(d.Error, "retirement denied")
	})
	if _, err := h.service.Deploy(context.Background(), h.source, "demo"); err == nil {
		t.Fatal("admitted candidate before failed retirement reclaimed")
	}
	active, err := h.service.Inspect("demo")
	if err != nil || active.ID != second.ID {
		t.Fatalf("retirement removed current route: %+v %v", active, err)
	}
	h.sandbox.mu.Lock()
	h.sandbox.stopErr = nil
	h.sandbox.mu.Unlock()
	waitFor(t, "retirement cleanup retry", func() bool { d, err := h.db.Deployment(first.ID); return err == nil && d.StoppedAt != nil })
}

func TestLegacyRollbackRefusesUnknownReadiness(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	first.ReadinessKnown = false
	if err := h.db.UpdateDeployment(*first); err != nil {
		t.Fatal(err)
	}
	before := h.builder.count()
	if _, err := h.service.Rollback(context.Background(), "demo", first.ReleaseID); err == nil || !strings.Contains(err.Error(), "no saved readiness") {
		t.Fatalf("legacy readiness guessed: %v", err)
	}
	if h.builder.count() != before {
		t.Fatal("rollback rebuilt legacy source")
	}
}

func TestFailedCandidateLogsRemainAccessible(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	h.builder.setBehavior("demo", &buildBehavior{err: errors.New("compiler failed")})
	candidate, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil {
		t.Fatal("candidate unexpectedly succeeded")
	}
	if err := os.WriteFile(candidate.LogPath, []byte("candidate compiler diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := h.service.Logs("demo")
	if err != nil || !strings.Contains(logs, "candidate compiler diagnostic") {
		t.Fatalf("candidate logs hidden: %q %v", logs, err)
	}
	active, err := h.service.Inspect("demo")
	if err != nil || active.ID != first.ID {
		t.Fatalf("inspection lost active app: %+v %v", active, err)
	}
}

func TestMonitorReclaimsCandidateAfterFailedTerminalWrite(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	first, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite3", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TRIGGER reject_terminal_state BEFORE UPDATE ON deployments
		WHEN NEW.state IN ('START_FAILED', 'UNHEALTHY')
		BEGIN SELECT RAISE(ABORT, 'terminal write failed'); END`); err != nil {
		t.Fatal(err)
	}
	ready := &atomic.Bool{}
	h.sandbox.ready = ready
	if err := os.WriteFile(filepath.Join(h.source, "paas.yaml"), []byte(testConfig+"health:\n  path: /ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	candidate, err := h.service.Deploy(ctx, h.source, "demo")
	if err == nil || !strings.Contains(err.Error(), "terminal write failed") {
		t.Fatalf("terminal failure hidden: %+v %v", candidate, err)
	}
	if _, err := conn.Exec(`DROP TRIGGER reject_terminal_state`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "interrupted candidate cleanup", func() bool {
		d, err := h.db.Deployment(candidate.ID)
		return err == nil && d.State == model.StartFailed && d.StoppedAt != nil
	})
	active, err := h.service.Inspect("demo")
	if err != nil || active.ID != first.ID {
		t.Fatalf("cleanup removed live route: %+v %v", active, err)
	}
}
