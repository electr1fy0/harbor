package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"
	"paas/internal/store"
)

type testServiceProvider struct {
	created   *contracts.Service
	createErr error
	startErr  error
	stopErr   error
	creates   int
	starts    int
	stops     int
}

func (p *testServiceProvider) Create(context.Context, contracts.ServiceRequest) (*contracts.Service, error) {
	p.creates++
	return p.created, p.createErr
}

func (p *testServiceProvider) Start(context.Context, *contracts.Service) error {
	p.starts++
	return p.startErr
}

func (p *testServiceProvider) Stop(context.Context, string) error {
	p.stops++
	return p.stopErr
}

func (*testServiceProvider) ConnectionInfo(context.Context, *contracts.Service) (map[string]string, error) {
	return nil, nil
}

func TestAddServicePersistsOwnershipBeforeStarting(t *testing.T) {
	for _, test := range []struct {
		name      string
		startErr  error
		wantState model.ServiceState
	}{
		{name: "startup succeeds", wantState: model.ServiceReady},
		{name: "startup fails", startErr: errors.New("postgres readiness failed"), wantState: model.ServiceFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := store.Open(t.TempDir() + "/state.sqlite")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			provider := &testServiceProvider{
				created:  &contracts.Service{ID: "svc_owned", Name: "db", Type: "postgres", Version: "16", Port: 5432, VolumeDir: "/data/volumes/svc_owned/data"},
				startErr: test.startErr,
			}
			manager := &Manager{store: db, provider: provider}
			created, err := manager.AddService(context.Background(), "postgres", "db")
			if test.startErr == nil {
				if err != nil || created == nil || created.State != model.ServiceReady {
					t.Fatalf("AddService() = %+v, %v", created, err)
				}
			} else if err == nil || created != nil {
				t.Fatalf("AddService() = %+v, %v; want startup error", created, err)
			}
			persisted, lookupErr := db.GetService("db")
			if lookupErr != nil || persisted.State != test.wantState {
				t.Fatalf("persisted service = %+v, %v; want state %s", persisted, lookupErr, test.wantState)
			}
			if provider.creates != 1 || provider.starts != 1 {
				t.Fatalf("provider calls = creates:%d starts:%d", provider.creates, provider.starts)
			}
		})
	}
}

func TestAddServiceDoesNotStartWhenOwnershipCannotBePersisted(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateService(model.Service{ID: "svc_duplicate", Name: "existing", Type: "postgres", Version: "16", Port: 5432, State: model.ServiceReady, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	provider := &testServiceProvider{created: &contracts.Service{ID: "svc_duplicate", Name: "new", Type: "postgres", Version: "16", Port: 5433}}
	manager := &Manager{store: db, provider: provider}
	if _, err := manager.AddService(context.Background(), "postgres", "new"); err == nil {
		t.Fatal("AddService unexpectedly succeeded with a conflicting persisted identity")
	}
	if provider.starts != 0 {
		t.Fatalf("provider started an unowned service %d times", provider.starts)
	}
	_ = db.Close()
}

func TestAddServicePropagatesExistingLookupErrors(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &testServiceProvider{}
	manager := &Manager{store: db, provider: provider}
	if _, err := manager.AddService(context.Background(), "postgres", "db"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AddService() error = %v; want database lookup failure", err)
	}
	if provider.creates != 0 {
		t.Fatalf("provider Create called %d times after lookup failure", provider.creates)
	}
}
