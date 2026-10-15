package onboarding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"paas/internal/contracts"
	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/router"
	"paas/internal/store"
)

func TestManagerSubmitReturnsBeforeCheckoutAndSurvivesRequestCancellation(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "submit-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	checkoutRoot := t.TempDir()
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	checkoutCleanup := make(chan struct{})
	h.manager.checkout = func(ctx context.Context, _, _, _, _, _ string) (string, func(), error) {
		entered <- ctx
		select {
		case <-release:
			path, err := copyFixture(checkoutRoot, fixture)
			if err != nil {
				return "", nil, err
			}
			return path, func() { close(checkoutCleanup) }, nil
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	job, err := h.manager.Submit(requestCtx, deployRequest("submit-app", "https://github.com/example/submit-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if job.ID == "" || job.State != "QUEUED" {
		t.Fatalf("Submit() did not return a queued job: %+v", job)
	}
	cancelRequest()

	var checkoutCtx context.Context
	select {
	case checkoutCtx = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("checkout did not begin")
	}
	if err := checkoutCtx.Err(); err != nil {
		t.Fatalf("request cancellation propagated into accepted checkout: %v", err)
	}
	select {
	case <-checkoutCleanup:
		t.Fatal("checkout completed before it was released")
	default:
	}
	close(release)
	if got := awaitJobTerminal(t, h.manager, job.ID); got.State != "SUCCEEDED" {
		t.Fatalf("accepted job did not survive request cancellation: %+v", got)
	}
}

func TestGetSourceDistinguishesMissingSettingsFromUnavailableStorage(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	if _, _, err := h.manager.GetSource("missing-app"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("GetSource() for absent settings error = %v, want ErrJobNotFound", err)
	}
	settings := SourceSettings{
		Name:    "saved-app",
		RepoURL: "https://github.com/example/saved-app",
		Ref:     "main",
	}
	if err := h.manager.UpdateSource(context.Background(), settings, nil); err != nil {
		t.Fatalf("UpdateSource() error = %v", err)
	}
	if err := os.WriteFile(h.manager.sources.path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.manager.GetSource("saved-app"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("GetSource() for corrupt settings error = %v, want ErrSourceUnavailable", err)
	} else if errors.Is(err, ErrJobNotFound) {
		t.Fatalf("corrupt settings were misclassified as absent: %v", err)
	}
	if _, _, err := h.manager.CreateHook(context.Background(), "saved-app", "main"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("CreateHook() for corrupt settings error = %v, want ErrSourceUnavailable", err)
	}
}

func TestManagerCancellationWaitsForDeployAndCheckoutCleanup(t *testing.T) {
	gate := newBuildGate()
	h := newOnboardingHarnessWithBuilderGate(t, gate)
	fixture := authoritativeGoFixture(t, "cancel-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	checkoutRoot := t.TempDir()
	var checkoutCount atomic.Int32
	firstCleanup := make(chan struct{})
	secondCheckout := make(chan struct{})
	h.manager.checkout = func(_ context.Context, _, _, _, _, _ string) (string, func(), error) {
		switch checkoutCount.Add(1) {
		case 1:
			path, err := copyFixture(checkoutRoot, fixture)
			if err != nil {
				return "", nil, err
			}
			return path, func() { close(firstCleanup) }, nil
		case 2:
			close(secondCheckout)
			path, err := copyFixture(checkoutRoot, fixture)
			if err != nil {
				return "", nil, err
			}
			return path, func() {}, nil
		default:
			return "", nil, errors.New("unexpected additional checkout")
		}
	}

	first, err := h.manager.Submit(context.Background(), deployRequest("cancel-app", "https://github.com/example/cancel-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, gate.entered, "first build")
	canceled, err := h.manager.Cancel(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != "CANCELLING" {
		t.Fatalf("Cancel() state = %s, want CANCELLING", canceled.State)
	}
	waitSignal(t, gate.cancelSeen, "builder cancellation")

	second, err := h.manager.Submit(context.Background(), deployRequest("cancel-app", "https://github.com/example/cancel-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
	if err != nil {
		t.Fatalf("queue second job while deploy is draining: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := checkoutCount.Load(); got != 1 {
		t.Fatalf("next job began checkout before canceled Deploy completed: checkout count=%d", got)
	}
	if got, err := h.manager.Get(first.ID); err != nil || got.State != "CANCELLING" {
		t.Fatalf("blocked deployment state = %+v, err=%v; want CANCELLING", got, err)
	}
	select {
	case <-firstCleanup:
		t.Fatal("checkout cleanup ran before the blocked Deploy returned")
	default:
	}

	close(gate.release)
	if got := awaitJobTerminal(t, h.manager, first.ID); got.State != "CANCELED" {
		t.Fatalf("first canceled job = %+v", got)
	}
	waitSignal(t, firstCleanup, "checkout cleanup")
	waitSignal(t, secondCheckout, "next queued checkout")
	if got := awaitJobTerminal(t, h.manager, second.ID); got.State != "SUCCEEDED" {
		t.Fatalf("second job did not run after canceled Deploy completed: %+v", got)
	}
}

func TestManagerShutdownCancelsAndWaitsForWorker(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "shutdown-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	checkoutRoot := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	cleanupRan := make(chan struct{})
	h.manager.checkout = func(ctx context.Context, _, _, _, _, _ string) (string, func(), error) {
		close(entered)
		<-release
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		path, err := copyFixture(checkoutRoot, fixture)
		if err != nil {
			return "", nil, err
		}
		return path, func() { close(cleanupRan) }, nil
	}
	job, err := h.manager.Submit(context.Background(), deployRequest("shutdown-app", "https://github.com/example/shutdown-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered, "shutdown checkout")
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer shortCancel()
	if err := h.manager.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close while worker is blocked = %v, want deadline exceeded", err)
	}
	close(release)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	if err := h.manager.Close(closeCtx); err != nil {
		t.Fatalf("Close after worker release: %v", err)
	}
	if got := awaitJobTerminal(t, h.manager, job.ID); got.State != "CANCELED" {
		t.Fatalf("shutdown job state = %+v, want CANCELED", got)
	}
	if _, err := h.manager.Submit(context.Background(), deployRequest("shutdown-app", "https://github.com/example/shutdown-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi"))); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Submit after shutdown error = %v, want ErrManagerClosed", err)
	}
	select {
	case <-cleanupRan:
		t.Fatal("checkout cleanup should not run when checkout returns an error")
	default:
	}
}

func TestManagerQueueBoundAndJobHistoryLimit(t *testing.T) {
	queueManager := &Manager{service: &deployment.Service{}, ctx: context.Background(), queue: make(chan string, maxOutstandingJobs), jobs: make(map[string]*jobRecord)}
	var jobs []Job
	for i := 0; i < maxOutstandingJobs; i++ {
		job, err := queueManager.Submit(context.Background(), deployRequest("queue-app", "https://github.com/example/queue-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
		if err != nil {
			t.Fatalf("Submit #%d: %v", i+1, err)
		}
		jobs = append(jobs, job)
	}
	if got := len(queueManager.queue); got != maxOutstandingJobs {
		t.Fatalf("queue length = %d, want %d", got, maxOutstandingJobs)
	}
	if _, err := queueManager.Submit(context.Background(), deployRequest("queue-app", "https://github.com/example/queue-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi"))); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("ninth queued request error = %v, want ErrQueueFull", err)
	}
	for _, job := range jobs {
		if got, err := queueManager.Cancel(job.ID); err != nil || got.State != "CANCELED" {
			t.Fatalf("cancel queued job %+v: %v", got, err)
		}
	}
	if got := len(queueManager.queue); got != 0 {
		t.Fatalf("canceling queued jobs left %d queue entries", got)
	}
	if _, err := queueManager.Submit(context.Background(), deployRequest("queue-app", "https://github.com/example/queue-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi"))); err != nil {
		t.Fatalf("Submit after canceling queued jobs: %v", err)
	}

	historyManager := &Manager{service: &deployment.Service{}, ctx: context.Background(), queue: make(chan string, maxOutstandingJobs), jobs: make(map[string]*jobRecord)}
	var oldest string
	var newest string
	for i := 0; i < 101; i++ {
		job, err := historyManager.Submit(context.Background(), deployRequest("history-app", "https://github.com/example/history-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")))
		if err != nil {
			t.Fatalf("history Submit #%d: %v", i+1, err)
		}
		if i == 0 {
			oldest = job.ID
		}
		newest = job.ID
		<-historyManager.queue // simulate dequeue; cancel before a worker starts
		if _, err := historyManager.Cancel(job.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(historyManager.order) != 100 {
		t.Fatalf("retained job history = %d entries, want 100", len(historyManager.order))
	}
	if _, err := historyManager.Get(oldest); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("oldest job lookup error = %v, want ErrJobNotFound", err)
	}
	if got, err := historyManager.Get(newest); err != nil || got.ID != newest {
		t.Fatalf("newest retained job = %+v, err=%v", got, err)
	}
}

func TestPruneKeepsActiveOldestJobAndRemovesOldTerminalHistory(t *testing.T) {
	manager := &Manager{
		jobs:  make(map[string]*jobRecord),
		order: []string{"active-oldest"},
		dedup: make(map[string]dedupRecord),
	}
	manager.jobs["active-oldest"] = &jobRecord{Job: Job{ID: "active-oldest", State: "RUNNING"}}
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("terminal-%03d", i)
		manager.order = append(manager.order, id)
		manager.jobs[id] = &jobRecord{Job: Job{ID: id, State: "CANCELED"}, terminal: true}
		manager.dedup[id] = dedupRecord{JobID: id, CreatedAt: time.Now().UTC()}
	}

	manager.pruneLocked()
	if len(manager.order) != 100 || len(manager.jobs) != 100 {
		t.Fatalf("retained order/jobs = %d/%d, want 100/100", len(manager.order), len(manager.jobs))
	}
	if manager.jobs["active-oldest"] == nil || manager.jobs["active-oldest"].terminal {
		t.Fatal("pruning removed the oldest active job")
	}
	if manager.jobs["terminal-000"] != nil || manager.dedup["terminal-000"].JobID != "" {
		t.Fatal("pruning retained the oldest terminal job or its deduplication record")
	}
	if manager.jobs["terminal-119"] == nil {
		t.Fatal("pruning removed the newest terminal job")
	}
}

func TestForgetRemovesQueuedJobsFromBoundedQueue(t *testing.T) {
	dataDir := t.TempDir()
	manager := &Manager{
		ctx:         context.Background(),
		queue:       make(chan string, maxOutstandingJobs),
		jobs:        make(map[string]*jobRecord),
		order:       []string{"job-forgotten", "job-retained"},
		outstanding: 2,
		dedup:       make(map[string]dedupRecord),
		forgotten:   make(map[string]bool),
		sources:     newSourceStore(dataDir),
		protected:   newProtectedStore(dataDir),
	}
	if err := manager.sources.save(SourceSettings{Name: "forgotten-app", RepoURL: "https://github.com/example/forgotten-app"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.protected.update(func(state *protectedState) error {
		state.BuildEnvironment["forgotten-app"] = map[string]string{"API_TOKEN": "secret"}
		state.Hooks["hook_forgotten"] = storedHook{Metadata: HookMetadata{ID: "hook_forgotten", Name: "forgotten-app"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id, name string
	}{
		{id: "job-forgotten", name: "forgotten-app"},
		{id: "job-retained", name: "retained-app"},
	} {
		manager.jobs[item.id] = &jobRecord{
			Job:     Job{ID: item.id, Name: item.name, State: "QUEUED", Stage: "QUEUED"},
			request: deployRequest(item.name, "https://github.com/example/"+item.name, authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")),
		}
		manager.queue <- item.id
	}

	if err := manager.Forget("forgotten-app"); err != nil {
		t.Fatalf("Forget(): %v", err)
	}
	if got := len(manager.queue); got != 1 {
		t.Fatalf("queue length after Forget = %d, want 1", got)
	}
	if got := <-manager.queue; got != "job-retained" {
		t.Fatalf("remaining queued job = %q, want job-retained", got)
	}
	if manager.outstanding != 1 {
		t.Fatalf("outstanding after Forget = %d, want 1", manager.outstanding)
	}
	if _, err := manager.sources.get("forgotten-app"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source settings after Forget error = %v, want not found", err)
	}
	state, err := manager.protected.read()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.BuildEnvironment["forgotten-app"]; exists {
		t.Fatal("Forget retained encrypted build environment")
	}
	if _, exists := state.Hooks["hook_forgotten"]; exists {
		t.Fatal("Forget retained webhook secret")
	}
	if _, exists := state.Jobs["job-forgotten"]; exists {
		t.Fatal("Forget retained the persisted job")
	}
}

func TestManagerConcurrentQueuedCancelSubmitAndWorkerStart(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "concurrent-queue-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	checkoutRoot := t.TempDir()
	firstCheckout := make(chan struct{})
	releaseFirst := make(chan struct{})
	var checkoutCount atomic.Int32
	h.manager.checkout = func(ctx context.Context, _, _, _, _, _ string) (string, func(), error) {
		if checkoutCount.Add(1) == 1 {
			close(firstCheckout)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}
		}
		path, err := copyFixture(checkoutRoot, fixture)
		if err != nil {
			return "", nil, err
		}
		return path, func() {}, nil
	}

	selection := authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	submit := func() Job {
		job, err := h.manager.Submit(context.Background(), deployRequest("concurrent-queue-app", "https://github.com/example/concurrent-queue-app", selection))
		if err != nil {
			t.Fatalf("Submit(): %v", err)
		}
		return job
	}
	first := submit()
	waitSignal(t, firstCheckout, "first checkout")
	queued := submit()

	startRacers := make(chan struct{})
	cancelResult := make(chan Job, 1)
	cancelError := make(chan error, 1)
	go func() {
		<-startRacers
		job, err := h.manager.Cancel(queued.ID)
		cancelResult <- job
		cancelError <- err
	}()
	submitted := make(chan Job, 1)
	submitError := make(chan error, 1)
	go func() {
		<-startRacers
		job, err := h.manager.Submit(context.Background(), deployRequest("concurrent-queue-app", "https://github.com/example/concurrent-queue-app", selection))
		submitted <- job
		submitError <- err
	}()

	// Release the worker at the same time as queue compaction and enqueue. The
	// canceled item may still be dequeued, but it must never block later work.
	close(releaseFirst)
	close(startRacers)
	canceled := <-cancelResult
	if err := <-cancelError; err != nil {
		t.Fatalf("Cancel(): %v", err)
	}
	if canceled.State != "CANCELED" && canceled.State != "CANCELLING" {
		t.Fatalf("concurrent cancel state=%s, want CANCELED or CANCELLING", canceled.State)
	}
	third := <-submitted
	if err := <-submitError; err != nil {
		t.Fatalf("concurrent Submit(): %v", err)
	}
	if got := awaitJobTerminal(t, h.manager, first.ID); got.State != "SUCCEEDED" {
		t.Fatalf("first job = %+v", got)
	}
	if got := awaitJobTerminal(t, h.manager, queued.ID); got.State != "CANCELED" {
		t.Fatalf("canceled queued job = %+v", got)
	}
	if got := awaitJobTerminal(t, h.manager, third.ID); got.State != "SUCCEEDED" {
		t.Fatalf("newly submitted job was lost behind canceled queue item: %+v", got)
	}
}

func TestManagerGetKeepsLogsPinnedToDeploymentAttempt(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "logs-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	h.manager.checkout = singleFixtureCheckout(t, fixture)
	request := deployRequest("logs-app", "https://github.com/example/logs-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi"))

	first, err := h.manager.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	firstResult := awaitJobTerminal(t, h.manager, first.ID)
	if firstResult.State != "SUCCEEDED" || firstResult.Deployment == nil {
		t.Fatalf("first deployment = %+v", firstResult)
	}
	second, err := h.manager.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	secondResult := awaitJobTerminal(t, h.manager, second.ID)
	if secondResult.State != "SUCCEEDED" || secondResult.Deployment == nil || secondResult.Deployment.ID == firstResult.Deployment.ID {
		t.Fatalf("second deployment = %+v; expected a new healthy attempt", secondResult)
	}
	got, err := h.manager.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deployment == nil || got.Deployment.ID != firstResult.Deployment.ID {
		t.Fatalf("first job switched to a newer deployment: %+v", got)
	}
	joined := strings.Join(got.Logs, "\n")
	if !strings.Contains(joined, "builder-attempt-1") || strings.Contains(joined, "builder-attempt-2") {
		t.Fatalf("first job logs do not stay pinned to its attempt: %q", joined)
	}
}

func TestSafeLogTailRedactsSecretsBeforeTruncating(t *testing.T) {
	const limit = 64 << 10
	secret := "build-secret-that-crosses-the-log-tail-boundary"
	logs := strings.Repeat("p", 10) + secret + strings.Repeat("x", limit-16)
	got := safeLogTail(logs, limit, []string{secret})
	if len(got) > limit {
		t.Fatalf("safe log tail has %d bytes, limit is %d", len(got), limit)
	}
	if strings.Contains(got, secret) || strings.Contains(got, secret[len(secret)-16:]) {
		t.Fatalf("log tail exposed part of a truncated secret: %q", got[len(got)-40:])
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("redacted secret placeholder is missing: %q", got[len(got)-40:])
	}
}

func TestTailStringDoesNotSplitUTF8AtLimit(t *testing.T) {
	got := tailString("éxxy", 4)
	if !utf8.ValidString(got) {
		t.Fatalf("tail is not valid UTF-8: %q", got)
	}
	if got != "xxy" {
		t.Fatalf("tail=%q, want rune-aligned suffix %q", got, "xxy")
	}
}

func TestManagerRedeployUsesSavedSourceAndPreservesConfiguredSettings(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "retry-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	repoURL := "https://github.com/example/retry-app"
	h.manager.checkout = singleFixtureCheckout(t, fixture)
	selection := authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	initial, err := h.manager.Submit(context.Background(), deployRequest("retry-app", repoURL, selection))
	if err != nil {
		t.Fatal(err)
	}
	first := awaitJobTerminal(t, h.manager, initial.ID)
	if first.State != "SUCCEEDED" {
		t.Fatalf("initial deployment = %+v", first)
	}
	// Pre-override versions persisted inspection defaults as a non-empty
	// selection. Simulate that legacy source record to ensure the compatibility
	// flag defaults to repository-authoritative behavior after an upgrade.
	legacySettings, err := h.manager.sources.get("retry-app")
	if err != nil {
		t.Fatal(err)
	}
	legacySettings.Selection = selection
	legacySettings.ExplicitSelection = false
	if err := h.manager.sources.save(legacySettings); err != nil {
		t.Fatalf("save legacy source settings fixture: %v", err)
	}

	memory, cpu, tasks := "300Mi", 1.25, 24
	if _, err := h.service.Configure(context.Background(), "retry-app", deployment.ConfigureOptions{
		Environment: map[string]string{"API_TOKEN": "fixture-secret"},
		MemoryLimit: &memory, CPUQuota: &cpu, TasksLimit: &tasks,
	}); err != nil {
		t.Fatalf("configure application: %v", err)
	}

	closeManager(t, h.manager)
	h.manager = NewManager(h.service, h.services, "test-builder")
	if !h.manager.CanRedeploy("retry-app") {
		t.Fatal("repository settings did not survive manager recreation")
	}
	h.manager.checkout = singleFixtureCheckout(t, fixture)
	updateAuthoritativeGoFixture(t, fixture, "go build -o \"$PAAS_OUT/server\" ./cmd/server", "/app/new-server", "192Mi")

	job, err := h.manager.Redeploy(context.Background(), "retry-app")
	if err != nil {
		t.Fatalf("Redeploy() error = %v", err)
	}
	second := awaitJobTerminal(t, h.manager, job.ID)
	if second.State != "SUCCEEDED" || second.Deployment == nil {
		t.Fatalf("redeployment = %+v", second)
	}
	requests := h.builder.Requests()
	if len(requests) != 2 {
		t.Fatalf("builder requests = %d, want 2", len(requests))
	}
	if requests[1].BuildCommand != "go build -o \"$PAAS_OUT/server\" ./cmd/server" || requests[1].StartCommand != "/app/new-server" {
		t.Fatalf("redeploy did not use current authoritative paas.yaml: %+v", requests[1])
	}
	specs := h.sandbox.Specs()
	if len(specs) != 2 {
		t.Fatalf("sandbox starts = %d, want 2", len(specs))
	}
	if specs[1].MemoryMax != memory {
		t.Fatalf("redeployed memory = %q, want configured override %q", specs[1].MemoryMax, memory)
	}
	if !containsEnvironment(specs[1].Env, "API_TOKEN=fixture-secret") {
		t.Fatalf("configured environment did not survive redeploy: %v", specs[1].Env)
	}
	settings, err := h.service.Settings("retry-app")
	if err != nil || settings.MemoryLimit != memory || len(settings.EnvironmentKeys) != 1 || settings.EnvironmentKeys[0] != "API_TOKEN" {
		t.Fatalf("persisted settings after retry = %+v, err=%v", settings, err)
	}
}

func TestManagerRedeployAppliesExplicitSavedSourceOverrides(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixture := authoritativeGoFixture(t, "override-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	h.manager.checkout = singleFixtureCheckout(t, fixture)
	selection := authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	first, err := h.manager.Submit(context.Background(), deployRequest("override-app", "https://github.com/example/override-app", selection))
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitJobTerminal(t, h.manager, first.ID); result.State != "SUCCEEDED" {
		t.Fatalf("initial deployment = %+v", result)
	}

	settings := SourceSettings{
		Name: "override-app", RepoURL: "https://github.com/example/override-app", Ref: "main",
		Selection: authoritativeSelection("go build -o \"$PAAS_OUT/server\" ./cmd/custom", "/app/custom-server", "128Mi"),
	}
	settings.Selection.HealthPath = "/ready"
	if err := h.manager.UpdateSource(context.Background(), settings, nil); err != nil {
		t.Fatalf("UpdateSource(): %v", err)
	}

	job, err := h.manager.Redeploy(context.Background(), "override-app")
	if err != nil {
		t.Fatalf("Redeploy(): %v", err)
	}
	if result := awaitJobTerminal(t, h.manager, job.ID); result.State != "SUCCEEDED" {
		t.Fatalf("redeployment = %+v", result)
	}
	requests := h.builder.Requests()
	if len(requests) != 2 || requests[1].BuildCommand != settings.Selection.BuildCommand || requests[1].StartCommand != settings.Selection.StartCommand {
		t.Fatalf("redeploy ignored explicit source overrides: %+v", requests)
	}
	if !h.sandbox.HasCommand(settings.Selection.StartCommand) {
		t.Fatalf("sandbox did not start with the selected command %q", settings.Selection.StartCommand)
	}
}

func TestManagerDeploysGeneratedNodeAndGoConfiguration(t *testing.T) {
	h := newOnboardingHarness(t, nil)
	fixtures := map[string]string{}
	for _, project := range []struct {
		name string
		url  string
	}{
		{name: "node-generated", url: "https://github.com/example/node-generated"},
		{name: "go-generated", url: "https://github.com/example/go-generated"},
	} {
		dir := filepath.Join(t.TempDir(), project.name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if project.name == "node-generated" {
			writeProjectFile(t, dir, "package.json", `{"scripts":{"build":"vite build","start":"node server.js"}}`)
			writeProjectFile(t, dir, "package-lock.json", "{}\n")
		} else {
			writeProjectFile(t, dir, "go.mod", "module example.test/generated\n\ngo 1.25\n")
			writeProjectFile(t, dir, "main.go", "package main\nfunc main() {}\n")
		}
		fixtures[project.url] = dir
	}
	h.manager.checkout = mappedFixtureCheckout(t, fixtures)

	for _, project := range []struct {
		name, url, runtime, build, start string
	}{
		{name: "node-generated", url: "https://github.com/example/node-generated", runtime: "node", build: "npm ci && npm run build", start: "npm run start"},
		{name: "go-generated", url: "https://github.com/example/go-generated", runtime: "go", build: "go build -o \"$PAAS_OUT/server\" .", start: "/app/server"},
	} {
		inspection, err := Inspect(fixtures[project.url], project.name, ConfigSelection{})
		if err != nil {
			t.Fatalf("inspect %s fixture: %v", project.runtime, err)
		}
		job, err := h.manager.Submit(context.Background(), deployRequest(project.name, project.url, selectionFromView(inspection.Config)))
		if err != nil {
			t.Fatalf("Submit %s: %v", project.runtime, err)
		}
		if got := awaitJobTerminal(t, h.manager, job.ID); got.State != "SUCCEEDED" {
			t.Fatalf("%s generated deployment = %+v", project.runtime, got)
		}
	}

	requests := h.builder.Requests()
	if len(requests) != 2 {
		t.Fatalf("builder requests = %d, want 2", len(requests))
	}
	for _, want := range []struct {
		name, runtime, build, start string
	}{
		{"node-generated", "node", "npm ci && npm run build", "npm run start"},
		{"go-generated", "go", "go build -o \"$PAAS_OUT/server\" .", "/app/server"},
	} {
		var request *contracts.BuildRequest
		for i := range requests {
			if requests[i].AppName == want.name {
				request = &requests[i]
				break
			}
		}
		if request == nil || request.Runtime != want.runtime || request.BuildCommand != want.build || request.StartCommand != want.start {
			t.Errorf("%s generated build request = %+v", want.name, request)
		}
		if !h.sandbox.HasCommand(want.start) {
			t.Errorf("%s generated start command did not reach sandbox", want.name)
		}
	}
}

func TestManagerCreatesAndBindsReadyManagedSqlite(t *testing.T) {
	services := &fakeOnboardingServices{}
	h := newOnboardingHarness(t, services)
	fixture := authoritativeGoFixture(t, "sqlite-app", "go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi")
	h.manager.checkout = singleFixtureCheckout(t, fixture)
	request := deployRequest("sqlite-app", "https://github.com/example/sqlite-app", authoritativeSelection("go build -o \"$PAAS_OUT/server\" .", "/app/server", "128Mi"))
	request.CreateSqlite = true
	request.SqliteService = "primary-db"
	job, err := h.manager.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := awaitJobTerminal(t, h.manager, job.ID); got.State != "SUCCEEDED" {
		t.Fatalf("managed SQLite deployment = %+v", got)
	}
	events := services.Events()
	if len(events) != 2 || events[0] != "add:sqlite:primary-db:READY" || events[1] != "link:sqlite-app:primary-db" {
		t.Fatalf("managed SQLite create/bind events = %v", events)
	}
	createdServices, err := services.List(context.Background())
	if err != nil || len(createdServices) != 1 || createdServices[0].Port != 0 {
		t.Fatalf("SQLite service unexpectedly depends on a listening port: %+v (err=%v)", createdServices, err)
	}
}

func TestManagerValidatesSqliteServiceSelection(t *testing.T) {
	readySqlite := model.Service{Name: "ready-db", Type: "sqlite", State: model.ServiceReady}
	tests := []struct {
		name    string
		service *fakeOnboardingServices
		request DeployRequest
		wantErr bool
	}{
		{name: "no database", request: DeployRequest{}},
		{name: "create name must be valid", service: &fakeOnboardingServices{}, request: DeployRequest{CreateSqlite: true, SqliteService: "invalid name"}, wantErr: true},
		{name: "create cannot reuse an existing service name", service: &fakeOnboardingServices{services: []model.Service{readySqlite}}, request: DeployRequest{CreateSqlite: true, SqliteService: "ready-db"}, wantErr: true},
		{name: "ready sqlite without port", service: &fakeOnboardingServices{services: []model.Service{readySqlite}}, request: DeployRequest{SqliteService: "ready-db"}},
		{name: "wrong service type", service: &fakeOnboardingServices{services: []model.Service{{Name: "ready-db", Type: "postgres", State: model.ServiceReady}}}, request: DeployRequest{SqliteService: "ready-db"}, wantErr: true},
		{name: "not ready", service: &fakeOnboardingServices{services: []model.Service{{Name: "ready-db", Type: "sqlite", State: model.ServiceStarting}}}, request: DeployRequest{SqliteService: "ready-db"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{services: tt.service}
			err := manager.validateSqlite(context.Background(), tt.request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSqlite() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

type onboardingHarness struct {
	manager  *Manager
	service  *deployment.Service
	db       *store.Store
	builder  *managerTestBuilder
	sandbox  *managerTestSandbox
	services *fakeOnboardingServices
}

func newOnboardingHarness(t *testing.T, services *fakeOnboardingServices) *onboardingHarness {
	return newOnboardingHarnessConfigured(t, nil, services)
}

func newOnboardingHarnessWithBuilderGate(t *testing.T, gate *managerBuildGate) *onboardingHarness {
	return newOnboardingHarnessConfigured(t, gate, nil)
}

func newOnboardingHarnessConfigured(t *testing.T, gate *managerBuildGate, services *fakeOnboardingServices) *onboardingHarness {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	builder := &managerTestBuilder{artifactRoot: filepath.Join(root, "artifacts"), gate: gate}
	sandbox := newManagerTestSandbox()
	service, err := deployment.New(db, builder, sandbox, router.New(), deployment.Options{DataDir: filepath.Join(root, "data"), RuntimeUser: "nobody"})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	manager := NewManager(service, services, "test-builder")
	h := &onboardingHarness{manager: manager, service: service, db: db, builder: builder, sandbox: sandbox, services: services}
	t.Cleanup(func() {
		if h.manager != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := h.manager.Close(ctx); err != nil {
				t.Errorf("close onboarding manager: %v", err)
			}
			cancel()
		}
		if err := h.service.Close(); err != nil {
			t.Errorf("close deployment service: %v", err)
		}
		h.sandbox.Close()
		if err := h.db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return h
}

type managerTestBuilder struct {
	mu           sync.Mutex
	artifactRoot string
	requests     []contracts.BuildRequest
	gate         *managerBuildGate
}

func (b *managerTestBuilder) Build(ctx context.Context, request contracts.BuildRequest) (*contracts.Release, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	sequence := len(b.requests)
	gate := b.gate
	if sequence > 1 {
		gate = nil
	}
	b.mu.Unlock()

	if gate != nil {
		gate.enteredOnce.Do(func() { close(gate.entered) })
		select {
		case <-ctx.Done():
			gate.cancelOnce.Do(func() { close(gate.cancelSeen) })
			<-gate.release
			return nil, ctx.Err()
		case <-gate.release:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(b.artifactRoot, 0o700); err != nil {
		return nil, err
	}
	artifactDir := filepath.Join(b.artifactRoot, fmt.Sprintf("release-%d", sequence))
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(request.LogPath, []byte(fmt.Sprintf("builder-attempt-%d\n", sequence)), 0o600); err != nil {
		return nil, err
	}
	return &contracts.Release{
		ID: fmt.Sprintf("release-%d", sequence), SourceHash: fmt.Sprintf("hash-%d", sequence),
		Runtime: request.Runtime, ArtifactDir: artifactDir, Command: []string{request.StartCommand}, CreatedAt: time.Now().UTC(),
	}, nil
}

func (b *managerTestBuilder) Requests() []contracts.BuildRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]contracts.BuildRequest(nil), b.requests...)
}

type managerBuildGate struct {
	entered     chan struct{}
	cancelSeen  chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	cancelOnce  sync.Once
}

func newBuildGate() *managerBuildGate {
	return &managerBuildGate{entered: make(chan struct{}), cancelSeen: make(chan struct{}), release: make(chan struct{})}
}

type managerTestSandboxEntry struct {
	server *http.Server
	info   contracts.SandboxInfo
}

type managerTestSandbox struct {
	mu      sync.Mutex
	entries map[string]*managerTestSandboxEntry
	specs   []contracts.SandboxSpec
}

func newManagerTestSandbox() *managerTestSandbox {
	return &managerTestSandbox{entries: make(map[string]*managerTestSandboxEntry)}
}

func (*managerTestSandbox) Kind() string { return "host" }

func (s *managerTestSandbox) Start(ctx context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	port := 0
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "PORT=") {
			port, _ = strconv.Atoi(strings.TrimPrefix(entry, "PORT="))
			break
		}
	}
	if port == 0 {
		return nil, errors.New("test sandbox did not receive a valid PORT")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	copySpec := spec
	copySpec.Env = append([]string(nil), spec.Env...)
	copySpec.Command = append([]string(nil), spec.Command...)
	s.mu.Lock()
	s.specs = append(s.specs, copySpec)
	s.entries[spec.ID] = &managerTestSandboxEntry{server: server, info: contracts.SandboxInfo{ID: spec.ID, PID: 12345, Running: true}}
	s.mu.Unlock()
	go func() { _ = server.Serve(listener) }()
	return &contracts.SandboxHandle{ID: spec.ID, PID: 12345}, nil
}

func (s *managerTestSandbox) Stop(_ context.Context, id string) error {
	s.mu.Lock()
	entry := s.entries[id]
	delete(s.entries, id)
	s.mu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.server.Close()
}

func (s *managerTestSandbox) Inspect(_ context.Context, id string) (*contracts.SandboxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[id]
	if entry == nil {
		return nil, errors.New("test sandbox not found")
	}
	info := entry.info
	return &info, nil
}

func (s *managerTestSandbox) Specs() []contracts.SandboxSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contracts.SandboxSpec, len(s.specs))
	for i, spec := range s.specs {
		out[i] = spec
		out[i].Env = append([]string(nil), spec.Env...)
		out[i].Command = append([]string(nil), spec.Command...)
	}
	return out
}

func (s *managerTestSandbox) HasCommand(command string) bool {
	for _, spec := range s.Specs() {
		if len(spec.Command) == 1 && spec.Command[0] == command {
			return true
		}
	}
	return false
}

func (s *managerTestSandbox) Close() {
	s.mu.Lock()
	servers := make([]*http.Server, 0, len(s.entries))
	for _, entry := range s.entries {
		servers = append(servers, entry.server)
	}
	s.entries = make(map[string]*managerTestSandboxEntry)
	s.mu.Unlock()
	for _, server := range servers {
		_ = server.Close()
	}
}

type fakeOnboardingServices struct {
	mu       sync.Mutex
	services []model.Service
	events   []string
}

func (s *fakeOnboardingServices) AddService(_ context.Context, serviceType, name string) (*model.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service := model.Service{ID: "service-" + name, Name: name, Type: serviceType, State: model.ServiceReady}
	s.services = append(s.services, service)
	s.events = append(s.events, fmt.Sprintf("add:%s:%s:%s", serviceType, name, service.State))
	return &service, nil
}

func (s *fakeOnboardingServices) Link(_ context.Context, appName, serviceName string) (*model.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, fmt.Sprintf("link:%s:%s", appName, serviceName))
	return &model.Binding{AppID: appName, ServiceID: "service-" + serviceName}, nil
}

func (s *fakeOnboardingServices) List(_ context.Context) ([]model.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Service(nil), s.services...), nil
}

func (s *fakeOnboardingServices) Events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func authoritativeSelection(build, start, memory string) ConfigSelection {
	cpu, tasks := 0.5, 64
	return ConfigSelection{Runtime: "go", BuildCommand: build, StartCommand: start, MemoryLimit: memory, CPUQuota: &cpu, TasksLimit: &tasks}
}

func deployRequest(name, repoURL string, selection ConfigSelection) DeployRequest {
	return DeployRequest{Name: name, RepoURL: repoURL, Ref: "main", Config: selection}
}

func authoritativeGoFixture(t *testing.T, name, build, start, memory string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, dir, "go.mod", "module example.test/"+name+"\n\ngo 1.25\n")
	writeProjectFile(t, dir, "main.go", "package main\nfunc main() {}\n")
	writeProjectFile(t, dir, "paas.yaml", authoritativePaasYAML(name, build, start, memory))
	return dir
}

func updateAuthoritativeGoFixture(t *testing.T, dir, build, start, memory string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "paas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "name: ") {
			name = strings.Trim(strings.TrimPrefix(line, "name: "), "\"")
			break
		}
	}
	if name == "" {
		t.Fatal("fixture paas.yaml has no name")
	}
	writeProjectFile(t, dir, "paas.yaml", authoritativePaasYAML(name, build, start, memory))
}

func authoritativePaasYAML(name, build, start, memory string) string {
	return fmt.Sprintf("name: %q\nruntime: go\nbuild:\n  command: %q\nstart:\n  command: %q\nresources:\n  memory: %s\n  cpu: 0.5\n  tasks: 64\n", name, build, start, memory)
}

func singleFixtureCheckout(t *testing.T, fixture string) sourceCheckout {
	t.Helper()
	checkoutRoot := t.TempDir()
	return func(_ context.Context, _, _, _, _, _ string) (string, func(), error) {
		path, err := copyFixture(checkoutRoot, fixture)
		if err != nil {
			return "", nil, err
		}
		return path, func() { _ = os.RemoveAll(path) }, nil
	}
}

func mappedFixtureCheckout(t *testing.T, fixtures map[string]string) sourceCheckout {
	t.Helper()
	checkoutRoot := t.TempDir()
	return func(_ context.Context, repoURL, _, _, _, _ string) (string, func(), error) {
		fixture := fixtures[repoURL]
		if fixture == "" {
			return "", nil, fmt.Errorf("no local fixture for %s", repoURL)
		}
		path, err := copyFixture(checkoutRoot, fixture)
		if err != nil {
			return "", nil, err
		}
		return path, func() { _ = os.RemoveAll(path) }, nil
	}
}

func copyFixture(checkoutRoot, fixture string) (string, error) {
	clone, err := os.MkdirTemp(checkoutRoot, "checkout-")
	if err != nil {
		return "", fmt.Errorf("create fixture checkout: %w", err)
	}
	err = filepath.WalkDir(fixture, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(fixture, path)
		if err != nil || relative == "." {
			return err
		}
		target := filepath.Join(clone, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture includes non-regular file %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		_ = os.RemoveAll(clone)
		return "", fmt.Errorf("copy fixture checkout: %w", err)
	}
	return clone, nil
}

func awaitJobTerminal(t *testing.T, manager *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		job, err := manager.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		switch job.State {
		case "SUCCEEDED", "FAILED", "CANCELED":
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := manager.Get(id)
	t.Fatalf("job did not become terminal: %+v", job)
	return Job{}
}

func waitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("close manager: %v", err)
	}
}

func containsEnvironment(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
