// Package contracts contains the stable seams between the PaaS subsystems.
// Implementations can evolve while callers keep these shapes.
package contracts

import (
	"context"
	"time"
)

// Builder turns an application source directory into an immutable release.
type Builder interface {
	Build(context.Context, BuildRequest) (*Release, error)
}

// BuildRequest is intentionally small at this stage. Runtime-specific build
// configuration can be added by the builder without changing the interface.
type BuildRequest struct {
	SourceDir    string
	AppName      string
	Runtime      string
	BuildCommand string
	StartCommand string
	LogPath      string
}

// Release identifies an immutable build artifact.
type Release struct {
	ID          string
	SourceHash  string
	Runtime     string
	ArtifactDir string
	Command     []string
	CreatedAt   time.Time
}

// Sandbox starts and manages one application process. The initial
// implementation is a host process; Bubblewrap and systemd implementations
// can follow without changing the interface.
type Sandbox interface {
	Start(context.Context, SandboxSpec) (*SandboxHandle, error)
	Stop(context.Context, string) error
	Inspect(context.Context, string) (*SandboxInfo, error)
}

// SandboxSpec describes the process that should be started.
type SandboxSpec struct {
	ID        string
	Command   []string
	Dir       string
	Env       []string
	User      string
	LogPath   string
	MemoryMax string
	CPUQuota  float64
	TasksMax  int
}

// SandboxHandle is returned after a process has been started.
type SandboxHandle struct {
	ID  string
	PID int
}

// SandboxInfo is the read-only runtime view used by orchestration and CLI.
type SandboxInfo struct {
	ID            string
	PID           int
	Running       bool
	ExitCode      *int
	OOMKilled     bool
	Result        string
	SystemdUnit   string
	MemoryCurrent int64
	TasksCurrent  int
}

// Router owns hostname-to-upstream routing.
type Router interface {
	Set(host string, upstream string)
	Delete(host string)
}

// ServiceProvider is reserved for later managed services, but is frozen now
// so higher-level code does not depend on a concrete PostgreSQL implementation.
type ServiceProvider interface {
	Create(context.Context, ServiceRequest) (*Service, error)
	Start(context.Context, Service) error
	ConnectionInfo(context.Context, Service) (map[string]string, error)
}

// ServiceRequest describes a future managed service request.
type ServiceRequest struct {
	AppName string
	Name    string
}

// Service is the provider-neutral service identity.
type Service struct {
	ID   string
	Name string
}
