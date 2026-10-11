package deployment

import (
	"errors"
	"fmt"
	"strings"

	"paas/internal/model"
	"paas/internal/store"
)

const diagnosticExcerptRunes = 4000

// Production returns only the deployment currently mapped to the app's
// default .localhost hostname. A failed candidate is never reported as
// production merely because it is the newest deployment record.
func (s *Service) Production(name string) (*model.Deployment, error) {
	deployment, err := s.db.ActiveDeployment(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return deployment, err
}

// LatestAttempt returns the newest deployment record, including failed
// candidates that left an older production deployment untouched.
func (s *Service) LatestAttempt(name string) (*model.Deployment, error) {
	deployment, err := s.db.LatestDeployment(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return deployment, err
}

// Diagnostics combines the live production route and latest attempt with a
// deterministic, evidence-based explanation. Logs are always passed through
// the encrypted secret history redactor before an excerpt is returned.
func (s *Service) Diagnostics(name string, canRedeploy bool) (*model.Diagnostics, error) {
	production, err := s.Production(name)
	if err != nil {
		return nil, err
	}
	latest, err := s.LatestAttempt(name)
	if err != nil {
		return nil, err
	}
	settings, err := s.db.AppSettings(name)
	if err != nil {
		return nil, err
	}

	result := &model.Diagnostics{
		Production:    production,
		LatestAttempt: latest,
		Actions:       []model.DiagnosticAction{},
	}
	result.Summary = summarizeAttempt(production, latest, settings.DesiredRunning)

	if latest != nil {
		canViewFailureLogs := isFailedState(latest.State)
		result.Actions = append(result.Actions, model.DiagnosticAction{
			ID: "view_attempt_logs", Label: "View failed attempt logs", Available: canViewFailureLogs,
			Reason: unavailableReason(canViewFailureLogs, "No failed deployment attempt is recorded."),
		})
		if latest.State == model.BuildFailed || latest.State == model.StartFailed || latest.State == model.Unhealthy || latest.State == model.Crashed || latest.State == model.OOMKilled {
			logs, logErr := s.LogsForDeployment(name, latest.ID)
			if logErr != nil {
				return nil, fmt.Errorf("read diagnostic logs: %w", logErr)
			}
			result.Excerpt = boundedExcerpt(logs)
		}
	} else {
		result.Actions = append(result.Actions, model.DiagnosticAction{
			ID: "view_attempt_logs", Label: "View failed attempt logs", Available: false,
			Reason: "No deployment attempt has been recorded.",
		})
	}
	result.Actions = append(result.Actions,
		model.DiagnosticAction{
			ID: "configure_settings", Label: "Configure app settings", Available: true,
		},
		model.DiagnosticAction{
			ID: "retry_github_deploy", Label: "Retry deployment from configured GitHub source", Available: canRedeploy,
			Reason: unavailableReason(canRedeploy, "No persisted GitHub source is configured."),
		},
	)

	rollbackAvailable := false
	rollbackReason := "A last good release can be restored when no production deployment is serving."
	rollbackReleaseID := ""
	if production == nil && latest != nil && isFailedState(latest.State) {
		if latest.ReleaseID != "" {
			rollbackReleaseID = latest.ReleaseID
		}
		if release, releaseErr := s.db.PreviousPromotedRelease(name, rollbackReleaseID); releaseErr == nil {
			rollbackAvailable = true
			rollbackReason = ""
			rollbackReleaseID = release.ID
		} else if !errors.Is(releaseErr, store.ErrNotFound) {
			return nil, fmt.Errorf("read rollback availability: %w", releaseErr)
		} else {
			rollbackReason = "No previously promoted release is available."
		}
	}
	result.Actions = append(result.Actions, model.DiagnosticAction{
		ID: "rollback_release", Label: "Roll back last good release", Available: rollbackAvailable,
		Reason: rollbackReason, ReleaseID: rollbackReleaseID,
	})
	return result, nil
}

func unavailableReason(available bool, reason string) string {
	if available {
		return ""
	}
	return reason
}

func summarizeAttempt(production, latest *model.Deployment, desiredRunning bool) string {
	if latest == nil {
		if production != nil {
			return "Production is serving traffic; no deployment attempt is recorded."
		}
		return "No deployment attempt has been recorded."
	}

	message := attemptSummary(latest, desiredRunning)
	if production != nil && production.ID != latest.ID {
		return "Production remains healthy and is serving traffic. Latest attempt: " + message
	}
	if production != nil && production.ID == latest.ID && production.State == model.Running {
		return "The latest deployment is healthy and serving production traffic."
	}
	if latest.State == model.Running && production == nil {
		return "The latest deployment reports RUNNING, but no production route is active."
	}
	return message
}

func attemptSummary(deployment *model.Deployment, desiredRunning bool) string {
	switch deployment.State {
	case model.BuildFailed:
		return "The latest build attempt failed; review the bounded log excerpt for build output."
	case model.StartFailed:
		return "The latest candidate did not start; inspect its logs and verify the configured start command."
	case model.Unhealthy:
		return "The latest candidate did not pass readiness. Check that it binds to Harbor's injected PORT and responds successfully at the configured health path."
	case model.Crashed:
		return "The latest runtime process exited after startup. Inspect runtime logs for application output and exit status."
	case model.OOMKilled:
		if deployment.SandboxType == "systemd" && deployment.MemoryLimit != "" {
			return fmt.Sprintf("The runtime was OOM-killed at the recorded enforced memory ceiling of %s. Increase memory, then redeploy.", deployment.MemoryLimit)
		}
		return "The runtime was classified as OOM-killed, but this host backend does not enforce a runtime memory ceiling; inspect host and runtime diagnostics."
	case model.Stopped:
		if !desiredRunning {
			return "The application is stopped and its desired state is stopped, so daemon restarts will keep it stopped."
		}
		return "The latest deployment is stopped while the application remains configured to run. Inspect its logs or redeploy."
	case model.Running:
		return "The latest deployment is running."
	default:
		return "The latest deployment attempt is " + strings.ToLower(string(deployment.State)) + "."
	}
}

func isFailedState(state model.State) bool {
	switch state {
	case model.BuildFailed, model.StartFailed, model.Unhealthy, model.Crashed, model.OOMKilled:
		return true
	default:
		return false
	}
}

func boundedExcerpt(logs string) string {
	logs = strings.TrimSpace(logs)
	if logs == "" {
		return ""
	}
	runes := []rune(logs)
	if len(runes) > diagnosticExcerptRunes {
		runes = runes[len(runes)-diagnosticExcerptRunes:]
	}
	return string(runes)
}
