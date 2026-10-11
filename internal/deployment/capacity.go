package deployment

import (
	"context"
	"errors"
	"time"

	"paas/internal/config"
	"paas/internal/model"
	"paas/internal/store"
)

// CapacityRuntime is a read-only view used by the daemon's capacity endpoint.
// It contains the configured ceilings separately from the live sandbox identity.
type CapacityRuntime struct {
	ID                string
	AppName           string
	State             model.State
	PID               int
	ControlGroup      string
	MemoryLimit       string
	CPUQuota          float64
	TasksLimit        int
	ConfigurationOnly bool
}

// CapacityDataDir returns the configured Harbor data root for filesystem
// inspection. The path itself is kept inside the daemon response boundary.
func (s *Service) CapacityDataDir() string { return s.opts.DataDir }

// CapacityRuntimes captures active app deployments under the service lock,
// inspects each sandbox without holding that lock, and discards stale results.
// A non-empty name limits the returned app list to that one application.
func (s *Service) CapacityRuntimes(ctx context.Context, name string) ([]CapacityRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	apps, err := s.db.Apps()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	nameFound := name == ""
	if name != "" {
		for _, app := range apps {
			if app.Name == name {
				nameFound = true
				break
			}
		}
	}
	refs, err := s.db.UnfinishedDeployments(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	represented := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		represented[ref.AppName] = struct{}{}
	}
	for _, app := range apps {
		if name != "" && app.Name != name {
			continue
		}
		if _, ok := represented[app.Name]; ok {
			continue
		}
		latest, latestErr := s.db.LatestDeployment(app.Name)
		if errors.Is(latestErr, store.ErrNotFound) {
			refs = append(refs, model.Deployment{AppID: app.ID, AppName: app.Name, State: model.State("UNKNOWN")})
		} else if latestErr != nil {
			s.mu.Unlock()
			return nil, latestErr
		} else {
			refs = append(refs, *latest)
		}
		represented[app.Name] = struct{}{}
	}
	s.mu.Unlock()
	if !nameFound {
		return nil, store.ErrNotFound
	}

	result := make([]CapacityRuntime, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := CapacityRuntime{
			ID: ref.ID, AppName: ref.AppName, State: ref.State,
			MemoryLimit: ref.MemoryLimit, CPUQuota: ref.CPUQuota, TasksLimit: ref.TasksLimit,
			ConfigurationOnly: !ref.State.Active() && (ref.SandboxID == "" || ref.StoppedAt != nil),
		}
		if entry.ConfigurationOnly {
			settings, err := s.db.AppSettings(ref.AppName)
			if err != nil {
				return nil, err
			}
			if settings.MemoryLimit != "" {
				entry.MemoryLimit = settings.MemoryLimit
			}
			if settings.CPUQuota > 0 {
				entry.CPUQuota = settings.CPUQuota
			}
			if settings.TasksLimit > 0 {
				entry.TasksLimit = settings.TasksLimit
			}
			if entry.MemoryLimit == "" {
				entry.MemoryLimit = config.DefaultMemory
			}
			if entry.CPUQuota <= 0 {
				entry.CPUQuota = config.DefaultCPU
			}
			if entry.TasksLimit <= 0 {
				entry.TasksLimit = config.DefaultTasks
			}
		}
		if !entry.ConfigurationOnly && ref.SandboxID != "" {
			inspectCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			info, inspectErr := s.sandbox.Inspect(inspectCtx, ref.SandboxID)
			cancel()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if inspectErr == nil && info != nil && info.Running && info.ID == ref.SandboxID {
				entry.PID = info.PID
				entry.ControlGroup = info.ControlGroup
			}
		}

		// A Stop, replacement, or deployment failure can race the sandbox probe.
		// Only expose a runtime probe that still belongs to the active deployment.
		s.mu.Lock()
		if entry.ConfigurationOnly {
			latest, latestErr := s.db.LatestDeployment(ref.AppName)
			stillCurrent := ref.ID == "" && errors.Is(latestErr, store.ErrNotFound) || latestErr == nil && latest.ID == ref.ID
			s.mu.Unlock()
			if stillCurrent {
				result = append(result, entry)
			}
			continue
		}
		unfinished, currentErr := s.db.UnfinishedDeployments(ref.AppName)
		stillCurrent := false
		if currentErr == nil {
			for _, current := range unfinished {
				if current.ID == ref.ID && (current.State.Active() || current.SandboxID != "" && current.StoppedAt == nil) {
					stillCurrent = true
					break
				}
			}
		}
		s.mu.Unlock()
		if stillCurrent {
			result = append(result, entry)
		}
	}
	return result, nil
}
