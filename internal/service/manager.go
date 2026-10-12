package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"
	"paas/internal/service/sqlite"
	"paas/internal/store"
)

// Manager coordinates file-backed managed services and application bindings.
type Manager struct {
	store      *store.Store
	provider   contracts.ServiceProvider
	bindingMgr *BindingManager
}

// NewManager creates a manager for persistent SQLite services.
func NewManager(st *store.Store, dataDir string) *Manager {
	provider := sqlite.NewProvider(dataDir)
	return &Manager{
		store:      st,
		provider:   provider,
		bindingMgr: NewBindingManager(st),
	}
}

// AddService provisions and records a managed SQLite service.
func (m *Manager) AddService(ctx context.Context, serviceType, name string) (*model.Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if serviceType != "sqlite" {
		return nil, fmt.Errorf("unsupported service type %q: only sqlite is supported", serviceType)
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("service name must not be empty")
	}

	if existing, err := m.store.GetService(name); err == nil && existing != nil {
		return nil, fmt.Errorf("service %q already exists", name)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("check existing service %q: %w", name, err)
	}

	created, err := m.provider.Create(ctx, contracts.ServiceRequest{
		Name:    name,
		Type:    serviceType,
		Version: "3",
	})
	if err != nil {
		return nil, fmt.Errorf("create service %q: %w", name, err)
	}
	if created == nil {
		return nil, fmt.Errorf("create service %q: provider returned an empty service", name)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := m.discardUnpersistedService(created)
		return nil, errors.Join(err, cleanupErr)
	}
	svcModel := model.Service{
		ID:        created.ID,
		Name:      name,
		Type:      serviceType,
		Version:   created.Version,
		Port:      0,
		State:     model.ServiceStarting,
		VolumeDir: created.VolumeDir,
		CreatedAt: time.Now().UTC(),
	}

	if err := m.store.CreateService(svcModel); err != nil {
		cleanupErr := m.discardUnpersistedService(created)
		return nil, errors.Join(fmt.Errorf("persist service %q: %w", name, err), cleanupErr)
	}
	if err := m.provider.Start(ctx, created); err != nil {
		stateErr := m.store.UpdateServiceState(svcModel.ID, model.ServiceFailed)
		return nil, errors.Join(fmt.Errorf("start service %q: %w", name, err), stateErr)
	}
	if err := m.store.UpdateServiceState(svcModel.ID, model.ServiceReady); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr := m.provider.Stop(cleanupCtx, svcModel.ID)
		cleanupCancel()
		stateErr := m.store.UpdateServiceState(svcModel.ID, model.ServiceFailed)
		return nil, errors.Join(fmt.Errorf("record ready service %q: %w", name, err), cleanupErr, stateErr)
	}
	svcModel.State = model.ServiceReady

	return &svcModel, nil
}

func (m *Manager) discardUnpersistedService(created *contracts.Service) error {
	provider, ok := m.provider.(*sqlite.Provider)
	if !ok || created == nil || created.ID == "" {
		return nil
	}
	if _, err := m.store.GetService(created.ID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("confirm SQLite service is unpersisted before cleanup: %w", err)
	}
	return provider.DiscardUnpersisted(created)
}

// Link connects an application to a ready SQLite service.
func (m *Manager) Link(ctx context.Context, appName, serviceName string) (*model.Binding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	app, err := m.store.CreateApp(appName)
	if err != nil {
		return nil, fmt.Errorf("find application %q: %w", appName, err)
	}

	svc, err := m.store.GetService(serviceName)
	if err != nil {
		return nil, fmt.Errorf("find service %q: %w", serviceName, err)
	}

	binding, err := m.bindingMgr.Link(ctx, app, svc)
	if err != nil {
		return nil, fmt.Errorf("link application %q to service %q: %w", appName, serviceName, err)
	}

	return binding, nil
}

// List returns all managed services.
func (m *Manager) List(ctx context.Context) ([]model.Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.store.ListServices()
}

// Restore validates and restores persisted SQLite services before applications
// recover. Legacy PostgreSQL services remain on disk but are marked unavailable.
func (m *Manager) Restore(ctx context.Context) error {
	services, err := m.store.ListServices()
	if err != nil {
		return err
	}
	var failures []error
	// Clear readiness for every persisted service before checking any files.
	// If cancellation interrupts the validation pass, recovery must not trust a
	// READY value left by the previous daemon process.
	for _, stored := range services {
		if err := m.store.UpdateServiceState(stored.ID, model.ServiceFailed); err != nil {
			failures = append(failures, fmt.Errorf("clear readiness for service %s before restore: %w", stored.Name, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	for _, stored := range services {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if stored.Type != "sqlite" {
			var restoreErr error
			if stored.Type == "postgres" {
				restoreErr = legacyPostgresError(stored.Name)
			} else {
				restoreErr = fmt.Errorf("unsupported persisted service type %q", stored.Type)
			}
			stateErr := m.store.UpdateServiceState(stored.ID, model.ServiceFailed)
			failures = append(failures, errors.Join(fmt.Errorf("restore service %s: %w", stored.Name, restoreErr), stateErr))
			continue
		}
		contractService := &contracts.Service{
			ID: stored.ID, Name: stored.Name, Type: stored.Type, Version: stored.Version,
			Port: stored.Port, State: string(stored.State), VolumeDir: stored.VolumeDir,
		}
		if err := m.provider.Start(ctx, contractService); err != nil {
			stateErr := m.store.UpdateServiceState(stored.ID, model.ServiceFailed)
			failures = append(failures, errors.Join(fmt.Errorf("restore service %s: %w", stored.Name, err), stateErr))
			continue
		}
		if err := m.validateServiceBindings(ctx, &stored); err != nil {
			stateErr := m.store.UpdateServiceState(stored.ID, model.ServiceFailed)
			failures = append(failures, errors.Join(fmt.Errorf("restore service %s: %w", stored.Name, err), stateErr))
			continue
		}
		if err := m.store.UpdateServiceState(stored.ID, model.ServiceReady); err != nil {
			failures = append(failures, fmt.Errorf("record restored service %s: %w", stored.Name, err))
			continue
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) validateServiceBindings(ctx context.Context, svc *model.Service) error {
	apps, err := m.store.Apps()
	if err != nil {
		return fmt.Errorf("list applications with SQLite bindings: %w", err)
	}
	for _, app := range apps {
		if err := ctx.Err(); err != nil {
			return err
		}
		bindings, err := m.store.GetBindingsForApp(app.ID)
		if err != nil {
			return fmt.Errorf("list bindings for application %q: %w", app.Name, err)
		}
		for i := range bindings {
			binding := &bindings[i]
			if binding.ServiceID != svc.ID {
				continue
			}
			if binding.EnvKey != "DATABASE_URL" {
				return fmt.Errorf("binding %s for application %q does not use DATABASE_URL", binding.ID, app.Name)
			}
			path, err := DatabasePath(binding, svc)
			if err != nil {
				return fmt.Errorf("validate binding %s for application %q: %w", binding.ID, app.Name, err)
			}
			if err := validateSQLiteDatabase(ctx, path); err != nil {
				return fmt.Errorf("validate database for application %q: %w", app.Name, err)
			}
		}
	}
	return nil
}

// Close records SQLite services as stopped after dependent applications stop.
// Unsupported legacy services are left failed and their durable volumes are
// preserved for an explicit migration.
func (m *Manager) Close(ctx context.Context) error {
	services, err := m.store.ListServices()
	if err != nil {
		return err
	}
	var failures []error
	for i := len(services) - 1; i >= 0; i-- {
		stored := services[i]
		if stored.Type != "sqlite" || stored.State != model.ServiceReady {
			continue
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := m.provider.Stop(ctx, stored.ID); err != nil {
			stateErr := m.store.UpdateServiceState(stored.ID, model.ServiceFailed)
			failures = append(failures, errors.Join(fmt.Errorf("stop service %s: %w", stored.Name, err), stateErr))
			continue
		}
		if err := m.store.UpdateServiceState(stored.ID, model.ServiceStopped); err != nil {
			failures = append(failures, fmt.Errorf("record stopped service %s: %w", stored.Name, err))
		}
	}
	return errors.Join(failures...)
}
