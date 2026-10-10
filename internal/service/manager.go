package service

import (
	"context"
	"fmt"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"
	"paas/internal/runtime"
	"paas/internal/service/postgres"
	"paas/internal/store"
)

// Manager coordinates managed services and application bindings.
type Manager struct {
	store            *store.Store
	runtimeMgr       *runtime.Manager
	sandbox          contracts.Sandbox
	postgresProvider *postgres.Provider
	bindingMgr       *BindingManager
}

// NewManager creates a managed service Manager.
func NewManager(st *store.Store, rm *runtime.Manager, sb contracts.Sandbox, dataDir string) *Manager {
	pgProvider := postgres.NewProvider(dataDir, rm, sb)
	bindingMgr := NewBindingManager(st, rm)
	return &Manager{
		store:            st,
		runtimeMgr:       rm,
		sandbox:          sb,
		postgresProvider: pgProvider,
		bindingMgr:       bindingMgr,
	}
}

// AddService provisions, initializes, and starts a managed service.
func (m *Manager) AddService(ctx context.Context, serviceType, name string) (*model.Service, error) {
	if serviceType != "postgres" {
		return nil, fmt.Errorf("unsupported service type %q: only postgres is supported", serviceType)
	}
	if name == "" {
		return nil, fmt.Errorf("service name must not be empty")
	}

	if existing, err := m.store.GetService(name); err == nil && existing != nil {
		return nil, fmt.Errorf("service %q already exists", name)
	}

	created, err := m.postgresProvider.Create(ctx, contracts.ServiceRequest{
		Name:    name,
		Type:    serviceType,
		Version: "16",
	})
	if err != nil {
		return nil, fmt.Errorf("create service %q: %w", name, err)
	}

	if err := m.postgresProvider.Start(ctx, created); err != nil {
		return nil, fmt.Errorf("start service %q: %w", name, err)
	}

	svcModel := model.Service{
		ID:        created.ID,
		Name:      name,
		Type:      serviceType,
		Version:   created.Version,
		Port:      created.Port,
		State:     model.ServiceReady,
		VolumeDir: created.VolumeDir,
		CreatedAt: time.Now().UTC(),
	}

	if err := m.store.CreateService(svcModel); err != nil {
		return nil, fmt.Errorf("persist service %q: %w", name, err)
	}

	_ = m.store.RecordEvent(svcModel.ID, "", model.EventServiceCreated, fmt.Sprintf("Created %s service on port %d", serviceType, svcModel.Port))
	_ = m.store.RecordEvent(svcModel.ID, "", model.EventServiceStarted, fmt.Sprintf("Started %s service", name))

	return &svcModel, nil
}

// Link connects an application to a managed service and provisions database credentials.
func (m *Manager) Link(ctx context.Context, appName, serviceName string) (*model.Binding, error) {
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

	_ = m.store.RecordEvent("", app.ID, model.EventServiceLinked, fmt.Sprintf("Linked to service %s (database %s)", serviceName, binding.DatabaseName))

	return binding, nil
}

// List returns all managed services.
func (m *Manager) List(ctx context.Context) ([]model.Service, error) {
	return m.store.ListServices()
}

// Provider returns the PostgreSQL provider.
func (m *Manager) PostgresProvider() *postgres.Provider {
	return m.postgresProvider
}
