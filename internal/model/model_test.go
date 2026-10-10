package model

import "testing"

func TestActiveStates(t *testing.T) {
	for _, state := range []State{Queued, Building, Built, Starting, HealthChecking, Running} {
		if !state.Active() {
			t.Errorf("%s must be active", state)
		}
	}
	for _, state := range []State{BuildFailed, StartFailed, Unhealthy, Crashed, Stopped, "", "UNKNOWN"} {
		if state.Active() {
			t.Errorf("%s must not be active", state)
		}
	}
}
