package deployment

import (
	"paas/internal/model"
	"testing"
	"time"
)

func TestStageTimingsDoNotOverlapOrIncludeLaterRuntimeFailure(t *testing.T) {
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	event := func(kind model.EventType, milliseconds int) model.Event {
		return model.Event{Type: kind, CreatedAt: start.Add(time.Duration(milliseconds) * time.Millisecond)}
	}
	events := []model.Event{event(model.EventBuildStarted, 100), event(model.EventBuildCompleted, 900), event(model.EventSandboxStarting, 1000), event(model.EventSandboxStarted, 1500), event(model.EventHealthCheckPassed, 3500), event(model.EventDeploymentSucceeded, 4000), event(model.EventSandboxExited, 900000), event(model.EventDeploymentFailed, 900010)}
	result := stageDurations(model.Deployment{CreatedAt: start}, events)
	for label, pair := range map[string]struct {
		value *int64
		want  int64
	}{"build": {result.BuildMilliseconds, 800}, "startup": {result.StartupMilliseconds, 500}, "readiness": {result.ReadinessMilliseconds, 2000}, "promotion": {result.PromotionMilliseconds, 500}, "total": {result.TotalMilliseconds, 4000}} {
		if pair.value == nil || *pair.value != pair.want {
			t.Errorf("%s=%v expected %d", label, pair.value, pair.want)
		}
	}
}
func TestStartupFailureKeepsOnlyObservedTimings(t *testing.T) {
	start := time.Now()
	result := stageDurations(model.Deployment{CreatedAt: start}, []model.Event{{Type: model.EventSandboxStarting, CreatedAt: start.Add(time.Second)}, {Type: model.EventDeploymentFailed, CreatedAt: start.Add(2 * time.Second)}})
	if result.StartupMilliseconds == nil || *result.StartupMilliseconds != 1000 || result.TotalMilliseconds == nil || *result.TotalMilliseconds != 2000 || result.ReadinessMilliseconds != nil || result.PromotionMilliseconds != nil {
		t.Fatalf("failed startup timings=%+v", result)
	}
}

func TestStageDurationsAggregateStartupRetryAttempts(t *testing.T) {
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	event := func(kind model.EventType, milliseconds int) model.Event {
		return model.Event{Type: kind, CreatedAt: start.Add(time.Duration(milliseconds) * time.Millisecond)}
	}
	events := []model.Event{
		event(model.EventSandboxStarting, 1000),
		event(model.EventSandboxStarted, 1500),
		event(model.EventHealthCheckFailed, 2200),
		event(model.EventSandboxStarting, 2300),
		event(model.EventSandboxStarted, 2800),
		event(model.EventHealthCheckPassed, 3500),
		event(model.EventDeploymentSucceeded, 4000),
	}
	result := stageDurations(model.Deployment{CreatedAt: start}, events)
	for label, pair := range map[string]struct {
		value *int64
		want  int64
	}{
		"startup":   {result.StartupMilliseconds, 1000},
		"readiness": {result.ReadinessMilliseconds, 1400},
		"promotion": {result.PromotionMilliseconds, 500},
		"total":     {result.TotalMilliseconds, 4000},
	} {
		if pair.value == nil || *pair.value != pair.want {
			t.Errorf("%s=%v expected %d", label, pair.value, pair.want)
		}
	}
}
