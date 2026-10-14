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
	// BuildEnvironment contains explicitly selected KEY=VALUE pairs used only
	// while running the build command. Values must never be returned to clients.
	BuildEnvironment []string
	BuildTimeout     time.Duration
	ClearCache       bool
}

// DeployOptions carries per-deployment build controls through the lifecycle
// service to the builder. Empty values preserve the builder's configured
// defaults.
type DeployOptions struct {
	BuildEnvironment map[string]string
	BuildTimeout     time.Duration
	ClearCache       bool
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

// Sandbox executes and manages application runtimes. Kind identifies the
// backend used for persisted metadata and inspection.
type Sandbox interface {
	Kind() string
	Start(context.Context, SandboxSpec) (*SandboxHandle, error)
	Stop(context.Context, string) error
	Inspect(context.Context, string) (*SandboxInfo, error)
}

// SandboxInspection pairs one batch inspection result with its error. A
// SandboxBatchInspector returns one result per requested ID, in the same order.
type SandboxInspection struct {
	Info *SandboxInfo
	Err  error
}

// SandboxBatchInspector is an optional optimization for backends that can
// inspect multiple runtimes in one manager call. Other backends keep using
// Sandbox.Inspect.
type SandboxBatchInspector interface {
	InspectMany(context.Context, []string) []SandboxInspection
}

// BindMount specifies a host-to-sandbox directory mount.
type BindMount struct {
	HostPath  string
	MountPath string
	ReadOnly  bool
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
	RootFS    string
	Binds     []BindMount
	UID       int
	GID       int
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
	Transitioning bool
	ExitCode      *int
	OOMKilled     bool
	Result        string
	SystemdUnit   string
	ControlGroup  string
	MemoryCurrent int64
	TasksCurrent  int
}

// Router owns hostname-to-upstream routing.
type Router interface {
	Set(host string, upstream string)
	Delete(host string)
}

// ServiceProvider manages stateful platform services.
type ServiceProvider interface {
	Create(context.Context, ServiceRequest) (*Service, error)
	Start(context.Context, *Service) error
	Stop(context.Context, string) error
	ConnectionInfo(context.Context, *Service) (map[string]string, error)
}

// ServiceRequest describes a managed service creation request.
type ServiceRequest struct {
	Name    string
	Type    string
	Version string
}

// Service is the provider-neutral service identity.
type Service struct {
	ID        string
	Name      string
	Type      string
	Version   string
	Port      int
	State     string
	VolumeDir string
}
