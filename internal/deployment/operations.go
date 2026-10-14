package deployment

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"paas/internal/capacity"
	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/health"
	"paas/internal/model"
	"paas/internal/sandbox"
	"paas/internal/store"
)

var (
	metadataTagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)
	pathInMessage      = regexp.MustCompile(`(^|[\s=("'])/[^\s,;"')]+`)
)

// MetadataChanges applies only fields that are non-nil. Empty slices clear
// tags or required environment keys.
type MetadataChanges struct {
	Favorite                *bool
	Tags                    *[]string
	Archived                *bool
	RequiredEnvironmentKeys *[]string
}

type HealthPolicy struct {
	IntervalSeconds  int `json:"intervalSeconds"`
	TimeoutSeconds   int `json:"timeoutSeconds"`
	FailureThreshold int `json:"failureThreshold"`
}

type RestartPolicy struct {
	Enabled          bool `json:"enabled"`
	MaxAttempts      int  `json:"maxAttempts"`
	BaseDelaySeconds int  `json:"baseDelaySeconds"`
}

type CapacityPreflight struct {
	Supported             bool    `json:"supported"`
	CanFit                *bool   `json:"canFit,omitempty"`
	RequestedBytes        int64   `json:"requestedBytes"`
	CandidateBytes        int64   `json:"candidateBytes"`
	BuildBytes            *int64  `json:"buildBytes,omitempty"`
	EstimatedOverlapBytes *int64  `json:"estimatedOverlapBytes,omitempty"`
	BuildLimitKnown       bool    `json:"buildLimitKnown"`
	BuildRequired         bool    `json:"buildRequired"`
	AvailableBytes        *int64  `json:"availableBytes,omitempty"`
	ReservePercent        float64 `json:"reservePercent"`
	Message               string  `json:"message"`
}

type PreflightResult struct {
	Ready                   bool              `json:"ready"`
	MissingRequiredKeys     []string          `json:"missingRequiredEnvironmentKeys"`
	RequiredEnvironmentKeys []string          `json:"requiredEnvironmentKeys"`
	Capacity                CapacityPreflight `json:"capacity"`
}

type StageDurations struct {
	BuildMilliseconds     *int64 `json:"buildMilliseconds,omitempty"`
	StartupMilliseconds   *int64 `json:"startupMilliseconds,omitempty"`
	ReadinessMilliseconds *int64 `json:"readinessMilliseconds,omitempty"`
	PromotionMilliseconds *int64 `json:"promotionMilliseconds,omitempty"`
	TotalMilliseconds     *int64 `json:"totalMilliseconds,omitempty"`
}

type HistoryPage struct {
	Deployments []HistoryEntry `json:"deployments"`
	Total       int            `json:"total"`
	Offset      int            `json:"offset"`
	Limit       int            `json:"limit"`
	HasMore     bool           `json:"hasMore"`
}

type EventPage struct {
	Events  []model.Event `json:"events"`
	Total   int           `json:"total"`
	Offset  int           `json:"offset"`
	Limit   int           `json:"limit"`
	HasMore bool          `json:"hasMore"`
}

type HistoryEntry struct {
	Deployment model.Deployment `json:"deployment"`
	Stages     StageDurations   `json:"stages"`
}

var ErrBuildCacheBusy = errors.New("build cache is in use by an active build")

func (s *Service) acquireBuildCache(ctx context.Context, exclusive bool) (func(), error) {
	for {
		var locked bool
		if exclusive {
			locked = s.buildCacheMu.TryLock()
		} else {
			locked = s.buildCacheMu.TryRLock()
		}
		if locked {
			if exclusive {
				return s.buildCacheMu.Unlock, nil
			}
			return s.buildCacheMu.RUnlock, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-s.ctx.Done():
			timer.Stop()
			return nil, errors.New("daemon shutting down")
		case <-timer.C:
		}
	}
}

// TrafficRouter exposes the router implementation for optional in-process
// traffic observations without expanding the stable router contract.
func (s *Service) TrafficRouter() contracts.Router { return s.router }

// Redact removes configured secret values and filesystem paths from text that
// crosses the private API boundary.
func (s *Service) Redact(name, value string) (string, error) {
	value, err := s.redactText(name, value)
	if err != nil {
		return "", err
	}
	return redactFilesystemPaths(value), nil
}

func redactFilesystemPaths(value string) string {
	return pathInMessage.ReplaceAllString(value, "$1[PATH]")
}

func (s *Service) OperationSettings(name string) (*model.OperationSettings, error) {
	return s.db.OperationSettings(name)
}

func (s *Service) ConfigureMetadata(ctx context.Context, name string, change MetadataChanges) (*model.OperationSettings, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	unlock, err := s.lockApp(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.db.OperationSettings(name)
	if err != nil {
		return nil, err
	}
	favorite, archived := current.Favorite, current.Archived
	tags := append([]string(nil), current.Tags...)
	required := append([]string(nil), current.RequiredEnvironmentKeys...)
	if change.Favorite != nil {
		favorite = *change.Favorite
	}
	if change.Archived != nil {
		archived = *change.Archived
	}
	if change.Tags != nil {
		tags, err = normalizeMetadataTags(*change.Tags)
		if err != nil {
			return nil, err
		}
	}
	if change.RequiredEnvironmentKeys != nil {
		required, err = normalizeRequiredKeys(*change.RequiredEnvironmentKeys)
		if err != nil {
			return nil, err
		}
	}
	if err := s.db.UpdateOperationMetadata(name, favorite, archived, tags, required); err != nil {
		return nil, err
	}
	return s.db.OperationSettings(name)
}

func normalizeMetadataTags(input []string) ([]string, error) {
	if len(input) > 32 {
		return nil, errors.New("at most 32 application tags are allowed")
	}
	seen := make(map[string]bool, len(input))
	result := make([]string, 0, len(input))
	for _, tag := range input {
		tag = strings.TrimSpace(tag)
		if !metadataTagPattern.MatchString(tag) {
			return nil, fmt.Errorf("invalid application tag %q", tag)
		}
		key := strings.ToLower(tag)
		if !seen[key] {
			seen[key] = true
			result = append(result, tag)
		}
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i]) < strings.ToLower(result[j]) })
	return result, nil
}

func normalizeRequiredKeys(input []string) ([]string, error) {
	if len(input) > 64 {
		return nil, errors.New("at most 64 required environment keys are allowed")
	}
	seen := make(map[string]bool, len(input))
	result := make([]string, 0, len(input))
	for _, key := range input {
		key = strings.TrimSpace(key)
		if !environmentKeyPattern.MatchString(key) || strings.HasPrefix(key, "PAAS_") {
			return nil, fmt.Errorf("invalid required environment key %q", key)
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, key)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (s *Service) ConfigureHealthPolicy(ctx context.Context, name string, policy HealthPolicy) (*model.OperationSettings, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	if policy.IntervalSeconds < 0 || policy.IntervalSeconds > 3600 || policy.IntervalSeconds > 0 && policy.IntervalSeconds < 5 {
		return nil, errors.New("health probe interval must be 0 or between 5 and 3600 seconds")
	}
	if policy.TimeoutSeconds < 1 || policy.TimeoutSeconds > 30 || policy.IntervalSeconds > 0 && policy.TimeoutSeconds >= policy.IntervalSeconds {
		return nil, errors.New("health probe timeout must be between 1 and 30 seconds and less than its interval when enabled")
	}
	if policy.FailureThreshold < 1 || policy.FailureThreshold > 10 {
		return nil, errors.New("health failure threshold must be between 1 and 10")
	}
	unlock, err := s.lockApp(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.db.SetHealthPolicy(name, policy.IntervalSeconds, policy.TimeoutSeconds, policy.FailureThreshold); err != nil {
		return nil, err
	}
	return s.db.OperationSettings(name)
}

func (s *Service) ConfigureRestartPolicy(ctx context.Context, name string, policy RestartPolicy) (*model.OperationSettings, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 20 {
		return nil, errors.New("automatic restart attempts must be between 1 and 20")
	}
	if policy.BaseDelaySeconds < 1 || policy.BaseDelaySeconds > 300 {
		return nil, errors.New("automatic restart base delay must be between 1 and 300 seconds")
	}
	unlock, err := s.lockApp(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.db.SetRestartPolicy(name, policy.Enabled, policy.MaxAttempts, policy.BaseDelaySeconds); err != nil {
		return nil, err
	}
	return s.db.OperationSettings(name)
}

func (s *Service) Preflight(ctx context.Context, name string, buildRequired ...bool) (*PreflightResult, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	settings, err := s.db.AppSettings(name)
	if err != nil {
		return nil, err
	}
	operations, err := s.db.OperationSettings(name)
	if err != nil {
		return nil, err
	}
	missing, err := s.MissingRequiredEnvironmentKeys(settings.AppID, operations.RequiredEnvironmentKeys)
	if err != nil {
		return nil, err
	}
	memory := settings.MemoryLimit
	if memory == "" {
		memory = config.DefaultMemory
	}
	willBuild := true
	if len(buildRequired) > 0 {
		willBuild = buildRequired[0]
	}
	capacityCheck, err := s.capacityPreflight(ctx, memory, willBuild)
	if err != nil {
		return nil, err
	}
	ready := len(missing) == 0 && (capacityCheck.CanFit == nil || *capacityCheck.CanFit)
	return &PreflightResult{Ready: ready, MissingRequiredKeys: missing, RequiredEnvironmentKeys: append([]string{}, operations.RequiredEnvironmentKeys...), Capacity: capacityCheck}, nil
}

func (s *Service) MissingRequiredEnvironmentKeys(appID string, required []string) ([]string, error) {
	environment, err := s.runtimeEnvironment(appID)
	if err != nil {
		return nil, err
	}
	bindings, err := s.db.GetBindingsForApp(appID)
	if err != nil {
		return nil, err
	}
	return missingRequiredEnvironmentKeys(environment, bindings, required), nil
}

func missingRequiredEnvironmentKeys(environment map[string]string, bindings []model.Binding, required []string) []string {
	present := make(map[string]bool, len(environment)+len(bindings))
	for key := range environment {
		present[key] = true
	}
	for _, binding := range bindings {
		present[binding.EnvKey] = true
	}
	missing := make([]string, 0)
	for _, key := range required {
		if !present[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

func (s *Service) validateRequiredEnvironment(appID string) error {
	operations, err := s.db.OperationSettingsByAppID(appID)
	if err != nil {
		return err
	}
	missing, err := s.MissingRequiredEnvironmentKeys(appID, operations.RequiredEnvironmentKeys)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("deployment preflight is missing required environment keys: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s *Service) capacityPreflight(ctx context.Context, memory string, buildRequired bool) (CapacityPreflight, error) {
	requested, err := config.ParseMemoryBytes(memory)
	if err != nil {
		return CapacityPreflight{}, fmt.Errorf("invalid memory limit for capacity preflight: %w", err)
	}
	result := CapacityPreflight{
		RequestedBytes: requested, CandidateBytes: requested, ReservePercent: 10, BuildRequired: buildRequired,
		BuildLimitKnown: true, Message: "host memory availability could not be measured; capacity is unknown",
	}
	var buildBytes int64
	if buildRequired {
		s.mu.Lock()
		sandboxKind := s.sandbox.Kind()
		s.mu.Unlock()
		if sandboxKind == "systemd" {
			buildBytes = 512 << 20
		} else {
			result.BuildLimitKnown = false
		}
	}
	if result.BuildLimitKnown {
		result.BuildBytes = &buildBytes
		overlap := requested + buildBytes
		result.EstimatedOverlapBytes = &overlap
	}
	snapshot, err := capacity.New("").Sample(ctx, nil)
	if err != nil {
		return result, err
	}
	result.Supported = snapshot.Platform.Supported
	available := snapshot.Headroom.EstimatedMemoryAfterReserveBytes
	if available == nil {
		return result, nil
	}
	result.AvailableBytes = available
	if !result.BuildLimitKnown {
		result.Message = "candidate memory can be estimated, but the host build memory limit is unavailable; total overlap is unknown. Estimates are advisory and resource limits are ceilings, not reservations"
		return result, nil
	}
	canFit := *result.EstimatedOverlapBytes <= *available
	result.CanFit = &canFit
	if canFit {
		result.Message = "estimated candidate/build overlap is within current host headroom after reserve; this is advisory and resource limits are ceilings, not reservations"
	} else {
		result.Message = "estimated candidate/build overlap exceeds current host headroom after reserve; this is advisory and resource limits are ceilings, not reservations"
	}
	return result, nil
}

func (s *Service) Start(ctx context.Context, name string) (*model.Deployment, error) {
	return s.startPromotedRelease(ctx, name, false)
}

func (s *Service) Restart(ctx context.Context, name string) (*model.Deployment, error) {
	return s.startPromotedRelease(ctx, name, true)
}

func (s *Service) startPromotedRelease(ctx context.Context, name string, force bool) (*model.Deployment, error) {
	return s.startPromotedReleaseWithSelectionHook(ctx, name, force, nil)
}

func (s *Service) startPromotedReleaseWithSelectionHook(ctx context.Context, name string, force bool, afterSelection func()) (*model.Deployment, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	unlock, generation, err := s.lockAppForAdmission(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.mu.Lock()
	if s.stopping[name] > 0 || s.stopGeneration[name] != generation {
		s.mu.Unlock()
		return nil, errors.New("operation was interrupted by an explicit stop")
	}
	settings, err := s.db.AppSettings(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.db.SetDesiredRunning(settings.AppID, true); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.db.ClearAutomaticRestart(settings.AppID); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !force {
		active, activeErr := s.db.ActiveDeployment(name)
		if activeErr == nil && active.State == model.Running {
			s.mu.Unlock()
			return active, nil
		}
		if activeErr != nil && !errors.Is(activeErr, store.ErrNotFound) {
			s.mu.Unlock()
			return nil, activeErr
		}
	}
	latest, err := s.db.LatestPromotedDeployment(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	if afterSelection != nil {
		afterSelection()
	}
	return s.deployObservedLocked(ctx, "", name, nil, &latest.ReleaseID, contracts.DeployOptions{}, nil, true, generation)
}

func (s *Service) deployDesiredRelease(ctx context.Context, name, releaseID string) (*model.Deployment, error) {
	return s.deployObserved(ctx, "", name, nil, &releaseID, contracts.DeployOptions{}, nil, true)
}

func (s *Service) deployLatestPromoted(ctx context.Context, name string) (*model.Deployment, error) {
	unlock, generation, err := s.lockAppForAdmission(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.mu.Lock()
	if s.stopping[name] > 0 || s.stopGeneration[name] != generation {
		s.mu.Unlock()
		return nil, errors.New("operation was interrupted by an explicit stop")
	}
	settings, err := s.db.AppSettings(name)
	if err != nil || !settings.DesiredRunning {
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("application is explicitly stopped")
	}
	if active, activeErr := s.db.ActiveDeployment(name); activeErr == nil && active.State == model.Running {
		_ = s.db.SetNextRestart(settings.AppID, nil)
		s.mu.Unlock()
		return active, nil
	} else if activeErr != nil && !errors.Is(activeErr, store.ErrNotFound) {
		s.mu.Unlock()
		return nil, activeErr
	}
	latest, err := s.db.LatestPromotedDeployment(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	return s.deployObservedLocked(ctx, "", name, nil, &latest.ReleaseID, contracts.DeployOptions{}, nil, true, generation)
}

func (s *Service) CancelCandidate(ctx context.Context, name, deploymentID string) (*model.Deployment, error) {
	if !config.ValidName(name) || deploymentID == "" {
		return nil, errors.New("application name and deployment id are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	d, err := s.db.Deployment(deploymentID)
	if err != nil || d.AppName != name {
		s.mu.Unlock()
		if err == nil {
			err = store.ErrNotFound
		}
		return nil, err
	}
	if d.PromotedAt != nil {
		s.mu.Unlock()
		return nil, errors.New("production deployments cannot be canceled as candidates")
	}
	activeRoute, routeErr := s.db.ActiveDeployment(name)
	if routeErr != nil && !errors.Is(routeErr, store.ErrNotFound) {
		s.mu.Unlock()
		return nil, routeErr
	}
	if routeErr == nil && activeRoute.ID == d.ID {
		s.mu.Unlock()
		return nil, errors.New("production deployments cannot be canceled as candidates")
	}
	pending, ok := s.pending[deploymentID]
	if !ok {
		s.mu.Unlock()
		if d.State.Active() {
			return nil, errors.New("candidate transition is no longer cancelable")
		}
		return d, nil
	}
	pending.cancel()
	done := pending.done
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}
	current, err := s.db.Deployment(deploymentID)
	if err != nil {
		return nil, err
	}
	if current.PromotedAt != nil {
		return nil, errors.New("candidate was promoted before cancellation completed")
	}
	s.recordEvent(current.ID, current.AppID, model.EventCandidateCanceled, "candidate cancellation requested")
	return current, nil
}

func (s *Service) ConfigureRetention(ctx context.Context, name string, retain int) (*model.OperationSettings, []string, error) {
	if retain < 1 || retain > 1000 {
		return nil, nil, errors.New("release retention must be between 1 and 1000")
	}
	if !config.ValidName(name) {
		return nil, nil, errors.New("invalid application name")
	}
	unlock, err := s.lockApp(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	if err := s.db.SetReleaseRetention(name, retain); err != nil {
		return nil, nil, err
	}
	pruned, err := s.pruneReleasesLocked(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	settings, err := s.db.OperationSettings(name)
	return settings, pruned, err
}

func (s *Service) PruneReleases(ctx context.Context, name string) ([]string, error) {
	if !config.ValidName(name) {
		return nil, errors.New("invalid application name")
	}
	unlock, err := s.lockApp(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.pruneReleasesLocked(ctx, name)
}

func (s *Service) pruneReleasesLocked(ctx context.Context, name string) ([]string, error) {
	settings, err := s.db.OperationSettings(name)
	if err != nil {
		return nil, err
	}
	candidates, err := s.db.ReleasePruneCandidates(name, settings.RetainReleaseCount)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(candidates))
	root := filepath.Join(s.opts.DataDir, "releases")
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if err := removeManagedPath(root, releaseArtifactCleanupPath(candidate.ArtifactDir)); err != nil {
			return removed, fmt.Errorf("prune release artifact: %w", err)
		}
		if err := s.db.MarkReleaseArtifactPruned(candidate.ID, time.Now().UTC()); err != nil {
			return removed, err
		}
		removed = append(removed, candidate.ID)
	}
	return removed, nil
}

// scheduleReleasePrune enforces the persisted retention policy after every
// successful promotion. It runs after the traffic switch and waits for the
// current app operation to release its admission gate before pruning.
func (s *Service) scheduleReleasePrune(name string) {
	if s.ctx.Err() != nil {
		return
	}
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()
		if _, err := s.PruneReleases(ctx, name); err != nil && s.ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("enforce release retention for %s: %v", name, s.redactError(name, err))
		}
	}()
}

func (s *Service) PruneBuildCache(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	cache, ok := s.builder.(interface{ CacheDir() string })
	if !ok {
		return false, errors.New("builder does not expose a safe cache directory")
	}
	cacheDir := cache.CacheDir()
	if err := validateManagedPath(filepath.Join(s.opts.DataDir, "build-cache"), cacheDir, true); err != nil {
		return false, errors.New("builder cache path is outside Harbor's managed cache directory")
	}
	if !s.buildCacheMu.TryLock() {
		return false, ErrBuildCacheBusy
	}
	defer s.buildCacheMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// The concrete builder also coordinates cache maintenance with builds
	// admitted by the onboarding worker, which do not pass through this service.
	if pruner, ok := s.builder.(interface{ PruneCache(context.Context) error }); ok {
		if err := pruner.PruneCache(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	info, err := os.Lstat(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, errors.New("inspect Harbor build cache")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("Harbor build cache path is not a regular directory")
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return false, errors.New("read Harbor build cache")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		entryPath := filepath.Join(cacheDir, entry.Name())
		if err := validateManagedPath(cacheDir, entryPath, false); err != nil {
			return false, errors.New("Harbor build cache contains an unsafe entry")
		}
		if err := os.RemoveAll(entryPath); err != nil {
			return false, errors.New("remove Harbor build cache entry")
		}
	}
	return true, nil
}

func validateManagedPath(root, candidate string, allowRoot bool) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || rel == "." && !allowRoot {
		return errors.New("path is outside the managed root")
	}
	current := root
	if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed root is a symlink")
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed path contains a symlink")
		}
	}
	return nil
}

func removeManagedPath(root, candidate string) error {
	if err := validateManagedPath(root, candidate, false); err != nil {
		return err
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(absolute); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.RemoveAll(absolute)
}

func releaseArtifactCleanupPath(artifactDir string) string {
	if filepath.Base(artifactDir) == "app" {
		return filepath.Dir(artifactDir)
	}
	return artifactDir
}

func (s *Service) Delete(ctx context.Context, name string, preserveData bool) error {
	if !config.ValidName(name) {
		return errors.New("invalid application name")
	}
	unlock, err := s.lockApp(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.Stop(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("stop application before deletion: %w", err)
	}
	if _, err := s.db.AppSettings(name); err != nil {
		return err
	}
	deployments, _, err := s.db.DeploymentHistory(name, 1_000_000, 0)
	if err != nil {
		return err
	}
	if !preserveData {
		for _, d := range deployments {
			if err := removeManagedPath(filepath.Join(s.opts.DataDir, "logs"), d.LogPath); err != nil {
				return errors.New("could not safely remove an application log file")
			}
		}
		releases, err := s.db.ReleasesForApp(name)
		if err != nil {
			return err
		}
		for _, release := range releases {
			if err := removeManagedPath(filepath.Join(s.opts.DataDir, "releases"), releaseArtifactCleanupPath(release.ArtifactDir)); err != nil {
				return errors.New("could not safely remove an application release artifact")
			}
		}
	}
	if err := s.db.DeleteApp(name); err != nil {
		return err
	}
	return nil
}

func (s *Service) SetArchived(ctx context.Context, name string, archived bool) (*model.OperationSettings, error) {
	return s.ConfigureMetadata(ctx, name, MetadataChanges{Archived: &archived})
}

func (s *Service) History(name string, limit, offset int) (*HistoryPage, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 || offset < 0 || offset > 10_000_000 {
		return nil, errors.New("history limit must be 1 to 500 and offset must be non-negative")
	}
	deployments, total, err := s.db.DeploymentHistory(name, limit, offset)
	if err != nil {
		return nil, err
	}
	entries := make([]HistoryEntry, 0, len(deployments))
	for _, deployment := range deployments {
		stages, err := s.StageDurations(name, deployment.ID)
		if err != nil {
			return nil, err
		}
		redacted, redactErr := s.Redact(name, deployment.Error)
		if redactErr != nil {
			deployment.Error = "[WITHHELD]"
		} else {
			deployment.Error = redacted
		}
		entries = append(entries, HistoryEntry{Deployment: deployment, Stages: stages})
	}
	return &HistoryPage{Deployments: entries, Total: total, Offset: offset, Limit: limit, HasMore: offset+len(entries) < total}, nil
}

func (s *Service) HistoryAll(limit, offset int) (*HistoryPage, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 || offset < 0 || offset > 10_000_000 {
		return nil, errors.New("history limit must be 1 to 500 and offset must be non-negative")
	}
	deployments, total, err := s.db.DeploymentHistoryAll(limit, offset)
	if err != nil {
		return nil, err
	}
	entries := make([]HistoryEntry, 0, len(deployments))
	for _, deployment := range deployments {
		stages, err := s.StageDurations(deployment.AppName, deployment.ID)
		if err != nil {
			return nil, err
		}
		redacted, redactErr := s.Redact(deployment.AppName, deployment.Error)
		if redactErr != nil {
			deployment.Error = "[WITHHELD]"
		} else {
			deployment.Error = redacted
		}
		entries = append(entries, HistoryEntry{Deployment: deployment, Stages: stages})
	}
	return &HistoryPage{Deployments: entries, Total: total, Offset: offset, Limit: limit, HasMore: offset+len(entries) < total}, nil
}

func (s *Service) Events(name, deploymentID string) ([]model.Event, error) {
	events, err := s.db.EventsForApp(name, deploymentID)
	if err != nil {
		return nil, err
	}
	return s.redactEvents(name, events)
}

func (s *Service) EventsPage(name, deploymentID string, limit, offset int) (*EventPage, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 || offset < 0 || offset > 10_000_000 {
		return nil, errors.New("event limit must be 1 to 500 and offset must be non-negative")
	}
	events, total, err := s.db.EventsForAppPage(name, deploymentID, limit, offset)
	if err != nil {
		return nil, err
	}
	events, err = s.redactEvents(name, events)
	if err != nil {
		return nil, err
	}
	return &EventPage{Events: events, Total: total, Offset: offset, Limit: limit, HasMore: offset+len(events) < total}, nil
}

func (s *Service) redactEvents(name string, events []model.Event) ([]model.Event, error) {
	for i := range events {
		payload, redactErr := s.Redact(name, events[i].Payload)
		if redactErr != nil {
			events[i].Payload = "[WITHHELD]"
		} else {
			events[i].Payload = payload
		}
	}
	return events, nil
}

func (s *Service) StageDurations(name, deploymentID string) (StageDurations, error) {
	deployment, err := s.DeploymentForApp(name, deploymentID)
	if err != nil {
		return StageDurations{}, err
	}
	events, err := s.db.Events(deploymentID)
	if err != nil {
		return StageDurations{}, err
	}
	first := make(map[model.EventType]time.Time)
	for _, event := range events {
		if _, ok := first[event.Type]; !ok {
			first[event.Type] = event.CreatedAt
		}
	}
	lastOf := func(types ...model.EventType) (time.Time, bool) {
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			for _, eventType := range types {
				if event.Type == eventType {
					return event.CreatedAt, true
				}
			}
		}
		return time.Time{}, false
	}
	var result StageDurations
	if start, ok := first[model.EventBuildStarted]; ok {
		if end, endOK := lastOf(model.EventBuildCompleted, model.EventBuildFailed); endOK && !end.Before(start) {
			result.BuildMilliseconds = durationMS(end.Sub(start))
		}
	}
	if start, ok := first[model.EventSandboxStarting]; ok {
		if end, endOK := lastOf(model.EventHealthCheckPassed, model.EventHealthCheckFailed); endOK && !end.Before(start) {
			result.StartupMilliseconds = durationMS(end.Sub(start))
		}
	}
	if start, ok := first[model.EventSandboxStarted]; ok {
		if end, endOK := lastOf(model.EventHealthCheckPassed, model.EventHealthCheckFailed); endOK && !end.Before(start) {
			result.ReadinessMilliseconds = durationMS(end.Sub(start))
		}
	}
	if start, ok := first[model.EventHealthCheckPassed]; ok {
		if end, endOK := lastOf(model.EventDeploymentSucceeded, model.EventDeploymentFailed); endOK && !end.Before(start) {
			result.PromotionMilliseconds = durationMS(end.Sub(start))
		}
	}
	if end, ok := lastOf(model.EventDeploymentSucceeded, model.EventDeploymentFailed); ok && !end.Before(deployment.CreatedAt) {
		result.TotalMilliseconds = durationMS(end.Sub(deployment.CreatedAt))
	}
	return result, nil
}

func durationMS(duration time.Duration) *int64 {
	value := duration.Milliseconds()
	return &value
}

func (s *Service) LogsBySource(name, deploymentID, source string) (string, error) {
	deployment, err := s.DeploymentForApp(name, deploymentID)
	if err != nil {
		return "", err
	}
	var logs string
	switch source {
	case "", "all":
		return s.LogsForDeployment(name, deploymentID)
	case "build":
		logs, err = logTail(deployment.LogPath)
	case "runtime":
		if deployment.SystemdUnit == "" || deployment.SandboxType != "systemd" {
			return "", errors.New("runtime-specific logs are unavailable for this host runtime; build and runtime output share one log")
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		logs, err = sandbox.JournalLogs(ctx, deployment.SystemdUnit)
		cancel()
	default:
		return "", errors.New("log source must be all, build, or runtime")
	}
	if err != nil {
		return "", err
	}
	redacted, err := s.Redact(name, logs)
	if err != nil {
		return "", err
	}
	return redacted, nil
}

func validateBuildOptions(options contracts.DeployOptions) error {
	if len(options.BuildEnvironment) > 64 {
		return errors.New("at most 64 build environment values are allowed")
	}
	bytes := 0
	for key, value := range options.BuildEnvironment {
		if !environmentKeyPattern.MatchString(key) || strings.HasPrefix(key, "PAAS_") || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid build environment key or value for %q", key)
		}
		bytes += len(key) + len(value)
	}
	if bytes > 32<<10 {
		return errors.New("build environment exceeds 32 KiB")
	}
	if options.BuildTimeout != 0 && (options.BuildTimeout < time.Second || options.BuildTimeout > 30*time.Minute) {
		return errors.New("build timeout must be between 1 second and 30 minutes")
	}
	return nil
}

func (s *Service) redactBuildError(name string, err error, options contracts.DeployOptions) error {
	if err == nil {
		return nil
	}
	message, redactErr := s.Redact(name, err.Error())
	if redactErr != nil {
		return redactedError{message: "build failed; diagnostic text withheld because secrets could not be loaded", cause: err}
	}
	secrets := values(options.BuildEnvironment)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if message == err.Error() {
		return err
	}
	return redactedError{message: message, cause: err}
}

func (s *Service) buildRequestOptions(name string, options contracts.DeployOptions) ([]string, error) {
	_ = name
	if err := validateBuildOptions(options); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(options.BuildEnvironment))
	for _, key := range sortedKeys(options.BuildEnvironment) {
		result = append(result, key+"="+options.BuildEnvironment[key])
	}
	return result, nil
}

func (s *Service) healthProbeDue(deployment model.Deployment, now time.Time) (bool, int, int, int, error) {
	settings, err := s.db.OperationSettings(deployment.AppName)
	if err != nil {
		return false, 0, 0, 0, err
	}
	if !settings.HealthProbeEnabled {
		return false, 0, 0, 0, nil
	}
	if settings.LastHealthCheckAt != nil && now.Sub(*settings.LastHealthCheckAt) < time.Duration(settings.HealthProbeIntervalSeconds)*time.Second {
		return false, 0, 0, 0, nil
	}
	return true, settings.HealthProbeTimeoutSeconds, settings.HealthFailureThreshold, settings.HealthFailureCount, nil
}

func (s *Service) runHealthProbe(deployment model.Deployment, timeoutSeconds, failureThreshold, priorFailures int) {
	defer s.inflight.Done()
	defer func() {
		s.mu.Lock()
		delete(s.healthProbes, deployment.ID)
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(timeoutSeconds)*time.Second)
	err := health.Probe(ctx, deployment.Port, deployment.HealthPath, time.Duration(timeoutSeconds)*time.Second)
	cancel()
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, readErr := s.db.Deployment(deployment.ID)
	if readErr != nil || current.State != model.Running {
		return
	}
	active, routeErr := s.db.ActiveDeployment(current.AppName)
	if routeErr != nil || active.ID != current.ID {
		return
	}
	appSettings, settingsErr := s.db.AppSettings(current.AppName)
	if settingsErr != nil {
		return
	}
	if err == nil {
		if err := s.db.UpdateHealthStatus(current.AppID, "healthy", 0, now); err != nil {
			return
		}
		if priorFailures > 0 {
			s.recordEvent(current.ID, current.AppID, model.EventOngoingHealthCheckPassed, "ongoing health probe recovered")
		}
		return
	}
	failures := priorFailures + 1
	status := "degraded"
	if failures >= failureThreshold {
		status = "unhealthy"
	}
	if updateErr := s.db.UpdateHealthStatus(current.AppID, status, failures, now); updateErr != nil {
		return
	}
	redactedErr := s.redactError(current.AppName, err)
	s.recordEvent(current.ID, current.AppID, model.EventOngoingHealthCheckFailed, redactFilesystemPaths(redactedErr.Error()))
	if failures < failureThreshold || s.stopping[current.AppName] > 0 || !appSettings.DesiredRunning {
		return
	}
	if _, failErr := s.fail(current, model.Unhealthy, fmt.Errorf("ongoing health probe failed %d consecutive times", failures)); failErr != nil {
		return
	}
	s.queueAutomaticRestart(current.AppName, current.AppID, current.ID)
}

func (s *Service) queueAutomaticRestart(name, appID, deploymentID string) {
	if s.ctx.Err() != nil {
		return
	}
	plan, err := s.db.RecordAutomaticRestart(appID, time.Now().UTC())
	if err != nil {
		return
	}
	if plan.CrashLoop {
		s.recordEvent(deploymentID, appID, model.EventCrashLoopDetected, "automatic restart limit reached; explicit start or restart is required")
		return
	}
	if !plan.ShouldRun || plan.RestartAt == nil {
		return
	}
	s.recordEvent(deploymentID, appID, model.EventAutomaticRestartScheduled, fmt.Sprintf("attempt=%d delay=%s", plan.Attempt, time.Until(*plan.RestartAt).Round(time.Second)))
	s.scheduleAutomaticRestartAtLocked(name, appID, *plan.RestartAt)
}

// scheduleAutomaticRestartAtLocked resumes a persisted backoff deadline. The
// caller holds s.mu so Close cannot begin waiting before this work is counted.
func (s *Service) scheduleAutomaticRestartAtLocked(name, appID string, restartAt time.Time) {
	if s.ctx.Err() != nil {
		return
	}
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		delay := time.Until(restartAt)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
		current, err := s.db.AppSettings(name)
		operations, operationErr := s.db.OperationSettings(name)
		if err != nil || operationErr != nil || !current.DesiredRunning || !operations.AutomaticRestartEnabled || operations.CrashLoop || operations.NextRestartAt == nil || !operations.NextRestartAt.Equal(restartAt) {
			return
		}
		_, err = s.deployLatestPromoted(s.ctx, name)
		if err != nil {
			current, settingsErr := s.db.AppSettings(name)
			operations, operationErr := s.db.OperationSettings(name)
			if settingsErr == nil && operationErr == nil && current.DesiredRunning && operations.AutomaticRestartEnabled && !operations.CrashLoop && s.ctx.Err() == nil {
				s.mu.Lock()
				latest, latestErr := s.db.LatestPromotedDeployment(name)
				if latestErr == nil {
					s.queueAutomaticRestart(name, appID, latest.ID)
				}
				s.mu.Unlock()
			}
		}
	}()
}
