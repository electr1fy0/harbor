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
