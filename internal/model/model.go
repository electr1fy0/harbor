package model

import "time"

type State string

const (
	Queued         State = "QUEUED"
	Building       State = "BUILDING"
	Built          State = "BUILT"
	Starting       State = "STARTING"
	HealthChecking State = "HEALTH_CHECKING"
	Running        State = "RUNNING"
	BuildFailed    State = "BUILD_FAILED"
	StartFailed    State = "START_FAILED"
	Unhealthy      State = "UNHEALTHY"
	Crashed        State = "CRASHED"
	OOMKilled      State = "OOM_KILLED"
	Stopped        State = "STOPPED"
)

func (s State) Active() bool {
	return s == Queued || s == Building || s == Built || s == Starting || s == HealthChecking || s == Running
}

type Application struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// AppSettings is the private persisted configuration for an application.
// Environment is encrypted before it is stored and is never exposed by API
// responses. Empty resource values mean that the application has no override.
type AppSettings struct {
	AppID                     string
	AppName                   string
	DesiredRunning            bool
	MemoryLimit               string
	CPUQuota                  float64
	TasksLimit                int
	EncryptedEnvironment      []byte
	EncryptedRedactionHistory []byte
	Domains                   []string
}

// Settings is the safe, user-facing application configuration view.
type Settings struct {
	Domains         []string `json:"domains"`
	EnvironmentKeys []string `json:"environmentKeys"`
	MemoryLimit     string   `json:"memoryLimit"`
	CPUQuota        float64  `json:"cpuQuota"`
	TasksLimit      int      `json:"tasksLimit"`
}

type Deployment struct {
	ID             string
	AppID          string
	AppName        string
	ReleaseID      string
	Port           int
	State          State
	Error          string
	PID            int
	LogPath        string
	SystemdUnit    string
	SandboxType    string
	SandboxID      string
	MemoryLimit    string
	CPUQuota       float64
	TasksLimit     int
	ReadinessKnown bool
	HealthPath     string
	PromotedAt     *time.Time
	CreatedAt      time.Time
	StartedAt      *time.Time
	StoppedAt      *time.Time
}

// EventType represents a granular lifecycle transition.
type EventType string

const (
	EventBuildStarted         EventType = "BuildStarted"
	EventBuildCompleted       EventType = "BuildCompleted"
	EventBuildFailed          EventType = "BuildFailed"
	EventSandboxStarting      EventType = "SandboxStarting"
	EventSandboxStarted       EventType = "SandboxStarted"
	EventSandboxExited        EventType = "SandboxExited"
	EventHealthCheckPassed    EventType = "HealthCheckPassed"
	EventHealthCheckFailed    EventType = "HealthCheckFailed"
	EventRouteSwitched        EventType = "RouteSwitched"
	EventGracePeriodStarted   EventType = "GracePeriodStarted"
	EventOldDeploymentStopped EventType = "OldDeploymentStopped"
	EventDeploymentSucceeded  EventType = "DeploymentSucceeded"
	EventDeploymentFailed     EventType = "DeploymentFailed"
	EventRollbackTriggered    EventType = "RollbackTriggered"
	EventServiceCreated       EventType = "ServiceCreated"
	EventServiceStarted       EventType = "ServiceStarted"
	EventServiceLinked        EventType = "ServiceLinked"
)

type Event struct {
	ID           int64     `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	AppID        string    `json:"app_id"`
	Type         EventType `json:"type"`
	Payload      string    `json:"payload"`
	CreatedAt    time.Time `json:"created_at"`
}

type ReleaseView struct {
	ID          string    `json:"id"`
	AppID       string    `json:"app_id"`
	AppName     string    `json:"app_name"`
	SourceHash  string    `json:"source_hash"`
	Runtime     string    `json:"runtime"`
	ArtifactDir string    `json:"artifact_dir"`
	CreatedAt   time.Time `json:"created_at"`
	IsActive    bool      `json:"is_active"`
}

type ServiceState string

const (
	ServiceStarting ServiceState = "STARTING"
	ServiceReady    ServiceState = "READY"
	ServiceStopped  ServiceState = "STOPPED"
	ServiceFailed   ServiceState = "FAILED"
)

type Service struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Type      string       `json:"type"`
	Version   string       `json:"version"`
	Port      int          `json:"port"`
	State     ServiceState `json:"state"`
	VolumeDir string       `json:"volume_dir"`
	CreatedAt time.Time    `json:"created_at"`
}

type Binding struct {
	ID           string    `json:"id"`
	AppID        string    `json:"app_id"`
	ServiceID    string    `json:"service_id"`
	DatabaseName string    `json:"database_name"`
	Username     string    `json:"username"`
	Password     string    `json:"-"`
	EnvKey       string    `json:"env_key"`
	CreatedAt    time.Time `json:"created_at"`
}
