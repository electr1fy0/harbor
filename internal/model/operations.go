package model

import "time"

// OperationSettings contains durable app metadata and runtime supervision
// policy. It deliberately contains environment variable names only.
type OperationSettings struct {
	Favorite                   bool       `json:"favorite"`
	Tags                       []string   `json:"tags"`
	Archived                   bool       `json:"archived"`
	RequiredEnvironmentKeys    []string   `json:"requiredEnvironmentKeys"`
	SettingsRevision           int64      `json:"settingsRevision"`
	AppliedSettingsRevision    int64      `json:"appliedSettingsRevision"`
	PendingSettings            bool       `json:"pendingSettings"`
	HealthProbeIntervalSeconds int        `json:"healthProbeIntervalSeconds"`
	HealthProbeEnabled         bool       `json:"healthProbeEnabled"`
	HealthProbeTimeoutSeconds  int        `json:"healthProbeTimeoutSeconds"`
	HealthFailureThreshold     int        `json:"healthFailureThreshold"`
	HealthStatus               string     `json:"healthStatus"`
	HealthFailureCount         int        `json:"healthFailureCount"`
	LastHealthCheckAt          *time.Time `json:"lastHealthCheckAt,omitempty"`
	RestartAttempt             int        `json:"restartAttempt"`
	AutomaticRestartEnabled    bool       `json:"automaticRestartEnabled"`
	AutomaticRestartMaxAttempts int       `json:"automaticRestartMaxAttempts"`
	AutomaticRestartBaseDelaySeconds int  `json:"automaticRestartBaseDelaySeconds"`
	NextRestartAt              *time.Time `json:"nextRestartAt,omitempty"`
	CrashLoop                  bool       `json:"crashLoop"`
	RetainReleaseCount         int        `json:"retainReleaseCount"`
}

// RestartPlan describes a persisted automatic restart decision.
type RestartPlan struct {
	Attempt   int
	RestartAt *time.Time
	CrashLoop bool
	ShouldRun bool
}

// ReleasePruneCandidate is an old release artifact that can be removed while
// retaining its database metadata for deployment history.
type ReleasePruneCandidate struct {
	ID          string
	ArtifactDir string
}

const (
	EventOngoingHealthCheckPassed  EventType = "OngoingHealthCheckPassed"
	EventOngoingHealthCheckFailed  EventType = "OngoingHealthCheckFailed"
	EventAutomaticRestartScheduled EventType = "AutomaticRestartScheduled"
	EventCrashLoopDetected         EventType = "CrashLoopDetected"
	EventCandidateCanceled         EventType = "CandidateCanceled"
)
