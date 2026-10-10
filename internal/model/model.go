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
	OomKilled      State = "OOM_KILLED"
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

type Deployment struct {
	ID          string
	AppID       string
	AppName     string
	ReleaseID   string
	Port        int
	State       State
	Error       string
	PID         int
	LogPath     string
	SystemdUnit string
	SandboxID   string
	MemoryLimit string
	CPUQuota    float64
	TasksLimit  int
	CreatedAt   time.Time
	StartedAt   *time.Time
	StoppedAt   *time.Time
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

