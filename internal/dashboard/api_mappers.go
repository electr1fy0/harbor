package dashboard

import (
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	deploymentErrorPatterns = map[string]string{
		"BUILD_FAILED": "Build failed. Review the app logs for details.",
		"START_FAILED": "App failed to start. Review the app logs for details.",
		"UNHEALTHY":    "App did not pass its health check. Review the app logs for details.",
		"CRASHED":      "App stopped unexpectedly. Review the app logs for details.",
		"OOM_KILLED":   "Runtime was OOM-killed. Review app diagnostics and memory usage.",
	}
	deploymentStates = map[string]bool{
		"QUEUED": true, "BUILDING": true, "BUILT": true, "STARTING": true,
		"HEALTH_CHECKING": true, "RUNNING": true, "BUILD_FAILED": true,
		"START_FAILED": true, "UNHEALTHY": true, "CRASHED": true,
		"OOM_KILLED": true, "STOPPED": true,
	}
	deploymentJobStates = map[string]bool{
		"QUEUED": true, "RUNNING": true, "SUCCEEDED": true, "FAILED": true,
		"CANCELLING": true, "CANCELED": true,
	}
	deploymentJobStages = map[string]bool{
		"QUEUED": true, "CLONING": true, "PROVISIONING_DATABASE": true,
		"BUILDING": true, "STARTING": true, "HEALTH_CHECK": true,
		"COMPLETED": true, "FAILED": true, "CANCELED": true,
	}
	diagnosticActionLabels = map[string]string{
		"view_attempt_logs":   "View failed attempt logs",
		"configure_settings":  "Configure memory and environment",
		"retry_github_deploy": "Retry deployment from configured GitHub source",
		"rollback_release":    "Roll back last good release",
	}
	githubTokenPattern   = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{12,}\b`)
	githubPATPattern     = regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{12,}\b`)
	bearerPattern        = regexp.MustCompile(`(?i)(\bBearer\s+)[A-Za-z0-9._~+/-]+=*`)
	querySecretPattern   = regexp.MustCompile(`(?i)([?&](?:access_token|token|auth|key)=)[^&\s]+`)
	credentialPattern    = regexp.MustCompile(`(?i)\b(authorization|password|passwd|secret|token|api[_-]?key)\s*[:=]\s*([^\s,;]+)`)
	urlCredentialPattern = regexp.MustCompile(`(?i)(https?://)[^\s/@:]+:[^\s/@]+@`)
	pathPattern          = regexp.MustCompile(`(?:/Users/|/home/|/private/tmp/|/tmp/|/var/tmp/|/run/paas/|/opt/paas/|/var/lib/paas/|/srv/paas/)[^\s'"` + "`" + `),;]*`)
)

func object(value any) (map[string]any, bool) {
	valueMap, ok := value.(map[string]any)
	return valueMap, ok && valueMap != nil
}

func arrayOf(value any) ([]any, bool) {
	if value == nil {
		return []any{}, true
	}
	values, ok := value.([]any)
	return values, ok
}

func firstValue(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if candidate, ok := value[key]; ok {
			return candidate
		}
	}
	return nil
}

func safeString(value any, limit int) string {
	text, ok := value.(string)
	if !ok || limit < 0 {
		return ""
	}
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}

func safeNumber(value any) float64 {
	number, ok := numberValue(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0
	}
	return number
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case int32:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint64:
		return float64(number), true
	case uint32:
		return float64(number), true
	default:
		return 0, false
	}
}

func safeOptionalNumber(value any) any {
	number, ok := numberValue(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return nil
	}
	return number
}

func safeOptionalBytes(value any) any {
	number, ok := numberValue(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || math.Trunc(number) != number || number > 9007199254740991 {
		return nil
	}
	return number
}

func safeOptionalSignedBytes(value any) any {
	number, ok := numberValue(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || math.Abs(number) > 9007199254740991 {
		return nil
	}
	return number
}

func safeBoolean(value any) bool { return value == true }

func safeTime(value any) string {
	text, ok := value.(string)
	if !ok || len(text) > 128 {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		// Go timestamps emitted by time.Time are RFC3339. Keep a few common
		// JavaScript Date-compatible forms used by older persisted state.
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05Z07:00", "2006-01-02"} {
			if parsed, err = time.Parse(layout, text); err == nil {
				break
			}
		}
	}
	if err != nil {
		return ""
	}
	return parsed.UTC().Format("2006-01-02T15:04:05.000Z")
}

func safeNullableTime(value any) any {
	if value == nil {
		return nil
	}
	return safeTime(value)
}

func safeSlug(value any) string {
	candidate := safeString(value, 63)
	if !appNamePattern.MatchString(candidate) {
		return ""
	}
	return candidate
}

func mapDeployment(value any) map[string]any {
	if value == nil {
		return nil
	}
	deployment, ok := object(value)
	if !ok {
		return nil
	}
	stateValue := strings.ToUpper(safeString(deployment["State"], 32))
	state := stateValue
	if !deploymentStates[state] {
		state = "UNKNOWN"
	}
	return map[string]any{
		"ID":             safeString(deployment["ID"], 128),
		"AppID":          safeString(deployment["AppID"], 128),
		"AppName":        safeSlug(deployment["AppName"]),
		"ReleaseID":      safeString(deployment["ReleaseID"], 128),
		"Port":           safeNumber(deployment["Port"]),
		"State":          state,
		"Error":          deploymentErrorPatterns[state],
		"MemoryLimit":    safeString(deployment["MemoryLimit"], 64),
		"CPUQuota":       safeNumber(deployment["CPUQuota"]),
		"TasksLimit":     safeNumber(deployment["TasksLimit"]),
		"ReadinessKnown": safeBoolean(deployment["ReadinessKnown"]),
		"HealthPath":     safeString(deployment["HealthPath"], 256),
		"PromotedAt":     safeNullableTime(deployment["PromotedAt"]),
		"CreatedAt":      safeTime(deployment["CreatedAt"]),
		"StartedAt":      safeNullableTime(deployment["StartedAt"]),
		"StoppedAt":      safeNullableTime(deployment["StoppedAt"]),
	}
}

func mapAppView(value any) (map[string]any, bool) {
	view, ok := object(value)
	if !ok {
		return nil, false
	}
	application, ok := object(view["Application"])
	if !ok {
		return nil, false
	}
	deployment := mapDeployment(view["Deployment"])
	if view["Deployment"] != nil && deployment == nil {
		return nil, false
	}
	var production map[string]any
	if raw, exists := view["Production"]; exists {
		production = mapDeployment(raw)
		if raw != nil && production == nil {
			return nil, false
		}
	} else if deployment != nil && deployment["State"] == "RUNNING" {
		production = deployment
	}
	var latest map[string]any
	if raw, exists := view["LatestAttempt"]; exists {
		latest = mapDeployment(raw)
		if raw != nil && latest == nil {
			return nil, false
		}
	} else {
		latest = deployment
	}
	return map[string]any{
		"Application": map[string]any{
			"ID":        safeString(application["ID"], 128),
			"Name":      safeSlug(application["Name"]),
			"CreatedAt": safeTime(application["CreatedAt"]),
		},
		"Deployment":    deployment,
		"Production":    production,
		"LatestAttempt": latest,
	}, true
}

func mapDiagnostics(value any) map[string]any {
	diagnostics, ok := object(value)
	if !ok {
		return nil
	}
	production := mapDeployment(firstValue(diagnostics, "production", "Production"))
	latest := mapDeployment(firstValue(diagnostics, "latestAttempt", "LatestAttempt"))
	for _, name := range []string{"production", "Production", "latestAttempt", "LatestAttempt"} {
		if raw, exists := diagnostics[name]; exists && raw != nil && mapDeployment(raw) == nil {
			return nil
		}
	}
	actions := []any{}
	if raw := firstValue(diagnostics, "actions", "Actions"); raw != nil {
		values, valid := arrayOf(raw)
		if !valid {
			return nil
		} else {
			for _, item := range values {
				action, ok := object(item)
				if !ok {
					continue
				}
				id := safeString(firstValue(action, "id", "ID"), 64)
				label, valid := diagnosticActionLabels[id]
				if !valid {
					continue
				}
				actions = append(actions, map[string]any{
					"id":        id,
					"label":     label,
					"available": safeBoolean(firstValue(action, "available", "Available")),
					"reason":    safeString(firstValue(action, "reason", "Reason"), 300),
					"releaseID": safeString(firstValue(action, "releaseID", "ReleaseID"), 128),
				})
			}
		}
	}
	return map[string]any{
		"production":    production,
		"latestAttempt": latest,
		"summary":       safeString(firstValue(diagnostics, "summary", "Summary"), 1000),
		"excerpt":       tailString(sanitizeLogs(firstValue(diagnostics, "excerpt", "Excerpt")), 8_000),
		"actions":       actions,
	}
}

func mapDeploymentJob(value any) map[string]any {
	job, ok := object(value)
	if !ok {
		return nil
	}
	stateValue := strings.ToUpper(safeString(job["state"], 32))
	state := stateValue
	if !deploymentJobStates[state] {
		state = "UNKNOWN"
	}
	stageValue := strings.ToUpper(safeString(job["stage"], 64))
	stage := stageValue
	if !deploymentJobStages[stage] {
		stage = "UNKNOWN"
	}
	errorText := tailString(sanitizeLogs(job["error"]), 512)
	if errorText == "" {
		if state == "FAILED" {
			errorText = "The deployment failed. Review its logs and deployment diagnostics."
		}
		if state == "CANCELED" {
			errorText = "The deployment was canceled."
		}
	}
	logs := []any{}
	if raw := job["logs"]; raw != nil {
		if values, valid := arrayOf(raw); valid {
			start := 0
			if len(values) > 200 {
				start = len(values) - 200
			}
			for _, item := range values[start:] {
				if _, ok := item.(string); ok {
					logs = append(logs, tailString(sanitizeLogs(item), 4_000))
				}
			}
		}
	}
	deployment := mapJobDeployment(job["deployment"])
	if job["deployment"] != nil && deployment == nil {
		return nil
	}
	return map[string]any{
		"id":         safeString(job["id"], 128),
		"name":       safeSlug(job["name"]),
		"state":      state,
		"stage":      stage,
		"createdAt":  safeTime(job["createdAt"]),
		"updatedAt":  safeTime(job["updatedAt"]),
		"deployment": deployment,
		"error":      errorText,
		"logs":       logs,
	}
}

func mapJobDeployment(value any) map[string]any {
	if value == nil {
		return nil
	}
	deployment, ok := object(value)
	if !ok {
		return nil
	}
	return mapDeployment(map[string]any{
		"ID": deployment["ID"], "AppID": "", "AppName": deployment["AppName"],
		"ReleaseID": deployment["ReleaseID"], "Port": deployment["Port"], "State": deployment["State"],
		"MemoryLimit": deployment["MemoryLimit"], "CPUQuota": deployment["CPUQuota"],
		"TasksLimit": deployment["TasksLimit"], "ReadinessKnown": false,
		"HealthPath": "", "CreatedAt": deployment["CreatedAt"],
		"StartedAt": deployment["StartedAt"], "StoppedAt": deployment["StoppedAt"],
		"PromotedAt": deployment["PromotedAt"],
	})
}

func mapService(value any) (map[string]any, bool) {
	service, ok := object(value)
	if !ok {
		return nil, false
	}
	return map[string]any{
		"id":        safeString(service["id"], 128),
		"name":      safeString(service["name"], 128),
		"type":      safeString(service["type"], 64),
		"version":   safeString(service["version"], 64),
		"port":      safeNumber(service["port"]),
		"state":     safeString(service["state"], 32),
		"createdAt": safeTime(service["created_at"]),
	}, true
}

func mapRelease(value any) (map[string]any, bool) {
	release, ok := object(value)
	if !ok {
		return nil, false
	}
	return map[string]any{
		"id":         safeString(release["id"], 128),
		"appId":      safeString(release["app_id"], 128),
		"appName":    safeSlug(release["app_name"]),
		"sourceHash": safeString(release["source_hash"], 128),
		"runtime":    safeString(release["runtime"], 64),
		"createdAt":  safeTime(release["created_at"]),
		"isActive":   safeBoolean(release["is_active"]),
		"artifactAvailable": safeBoolean(release["artifactAvailable"]),
	}, true
}

func mapSettings(value any) map[string]any {
	settings, ok := object(value)
	if !ok {
		return emptySettings()
	}
	if nested, nestedOK := object(settings["settings"]); nestedOK {
		settings = nested
	} else if nested, nestedOK := object(settings["Settings"]); nestedOK {
		settings = nested
	}
	domains := []any{}
	seenDomains := map[string]bool{}
	if raw := firstValue(settings, "domains", "Domains"); raw != nil {
		if values, valid := arrayOf(raw); valid {
			for _, value := range values {
				if domain := safeDomain(value); domain != "" && !seenDomains[domain] {
					domains = append(domains, domain)
					seenDomains[domain] = true
				}
			}
		}
	}
	keys := []any{}
	seenKeys := map[string]bool{}
	if raw := firstValue(settings, "environmentKeys", "EnvironmentKeys"); raw != nil {
		if values, valid := arrayOf(raw); valid {
			for _, value := range values {
				key := safeString(value, 128)
				if environmentKeyPattern.MatchString(key) && !seenKeys[key] {
					keys = append(keys, key)
					seenKeys[key] = true
				}
			}
		}
	}
	return map[string]any{
		"domains":         domains,
		"environmentKeys": keys,
		"memoryLimit":     safeString(firstValue(settings, "memoryLimit", "MemoryLimit"), 64),
		"cpuQuota":        safeNumber(firstValue(settings, "cpuQuota", "CPUQuota")),
		"tasksLimit":      safeNumber(firstValue(settings, "tasksLimit", "TasksLimit")),
	}
}

func emptySettings() map[string]any {
	return map[string]any{"domains": []any{}, "environmentKeys": []any{}, "memoryLimit": "", "cpuQuota": float64(0), "tasksLimit": float64(0)}
}

func safeDomain(value any) string {
	candidate := strings.ToLower(safeString(value, 253))
	if !domainPattern.MatchString(candidate) {
		return ""
	}
	return candidate
}

func mapCapacity(value any) map[string]any {
	capacity, ok := object(value)
	if !ok {
		return nil
	}
	platform, _ := object(capacity["platform"])
	host, _ := object(capacity["host"])
	disk, _ := object(capacity["disk"])
	builds, _ := object(capacity["builds"])
	headroom, _ := object(capacity["headroom"])
	apps := []any{}
	if raw := capacity["apps"]; raw != nil {
		values, valid := arrayOf(raw)
		if !valid {
			return nil
		}
		for _, item := range values {
			app, valid := object(item)
			if !valid {
				return nil
			}
			apps = append(apps, map[string]any{
				"deploymentId":       safeString(app["deploymentId"], 128),
				"name":               safeSlug(app["name"]),
				"state":              safeString(app["state"], 32),
				"memoryLimitBytes":   safeOptionalBytes(app["memoryLimitBytes"]),
				"memoryCurrentBytes": safeOptionalBytes(app["memoryCurrentBytes"]),
				"memoryPeakBytes":    safeOptionalBytes(app["memoryPeakBytes"]),
				"cpuQuotaCores":      safeOptionalNumber(app["cpuQuotaCores"]),
				"cpuUsageCores":      safeOptionalNumber(app["cpuUsageCores"]),
				"tasksLimit":         safeOptionalBytes(app["tasksLimit"]),
				"tasksCurrent":       safeOptionalBytes(app["tasksCurrent"]),
				"metricsSource":      safeString(app["metricsSource"], 32),
			})
		}
	}
	caveats := []any{}
	if raw := capacity["caveats"]; raw != nil {
		if values, valid := arrayOf(raw); valid {
			for _, item := range values {
				if _, ok := item.(string); ok {
					caveats = append(caveats, safeString(item, 256))
				}
			}
		}
	}
	return map[string]any{
		"sampledAt": safeTime(capacity["sampledAt"]),
		"platform":  map[string]any{"supported": safeBoolean(platform["supported"]), "name": safeString(platform["name"], 32), "reason": safeString(platform["reason"], 256)},
		"host": map[string]any{
			"memoryTotalBytes": safeOptionalBytes(host["memoryTotalBytes"]), "memoryAvailableBytes": safeOptionalBytes(host["memoryAvailableBytes"]),
			"memoryUsedBytes": safeOptionalBytes(host["memoryUsedBytes"]), "cpuUsagePercent": safeOptionalNumber(host["cpuUsagePercent"]),
			"logicalCPUs": safeOptionalBytes(host["logicalCPUs"]),
		},
		"disk": map[string]any{
			"filesystemTotalBytes": safeOptionalBytes(disk["filesystemTotalBytes"]), "filesystemAvailableBytes": safeOptionalBytes(disk["filesystemAvailableBytes"]),
			"filesystemUsedBytes": safeOptionalBytes(disk["filesystemUsedBytes"]), "harborDataBytes": safeOptionalBytes(disk["harborDataBytes"]),
			"harborDataComplete": safeBoolean(disk["harborDataComplete"]), "harborDataChangeBytes": safeOptionalSignedBytes(disk["harborDataChangeBytes"]),
			"releasesChangeBytes": safeOptionalSignedBytes(disk["releasesChangeBytes"]), "buildCacheChangeBytes": safeOptionalSignedBytes(disk["buildCacheChangeBytes"]),
			"logsChangeBytes": safeOptionalSignedBytes(disk["logsChangeBytes"]), "changeIntervalSeconds": safeOptionalNumber(disk["changeIntervalSeconds"]),
			"releasesBytes": safeOptionalBytes(disk["releasesBytes"]), "buildCacheBytes": safeOptionalBytes(disk["buildCacheBytes"]), "logsBytes": safeOptionalBytes(disk["logsBytes"]),
		},
		"apps": apps,
		"builds": map[string]any{
			"systemdManagedActiveCount": safeOptionalBytes(builds["systemdManagedActiveCount"]), "memoryLimitBytes": safeOptionalBytes(builds["memoryLimitBytes"]),
			"memoryCurrentBytes": safeOptionalBytes(builds["memoryCurrentBytes"]), "memoryPeakBytes": safeOptionalBytes(builds["memoryPeakBytes"]),
			"cpuQuotaCores": safeOptionalNumber(builds["cpuQuotaCores"]), "cpuUsageCores": safeOptionalNumber(builds["cpuUsageCores"]),
		},
		"headroom": map[string]any{
			"memoryReservePercent":             numberOrZero(safeOptionalNumber(headroom["memoryReservePercent"])),
			"memoryReserveBytes":               safeOptionalBytes(headroom["memoryReserveBytes"]),
			"estimatedMemoryAfterReserveBytes": safeOptionalBytes(headroom["estimatedMemoryAfterReserveBytes"]),
		},
		"caveats": caveats,
	}
}

func numberOrZero(value any) float64 {
	if number, ok := value.(float64); ok {
		return number
	}
	return 0
}

func sanitizeLogs(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	if len(text) > 250_000 {
		text = text[len(text)-250_000:]
	}
	text = githubTokenPattern.ReplaceAllString(text, "[redacted token]")
	text = githubPATPattern.ReplaceAllString(text, "[redacted token]")
	text = bearerPattern.ReplaceAllString(text, "${1}[redacted]")
	text = querySecretPattern.ReplaceAllString(text, "${1}[redacted]")
	text = credentialPattern.ReplaceAllString(text, "${1}=[redacted]")
	text = urlCredentialPattern.ReplaceAllString(text, "${1}[redacted]@")
	return pathPattern.ReplaceAllString(text, "[redacted path]")
}

func tailString(value string, limit int) string {
	if len(value) > limit {
		return value[len(value)-limit:]
	}
	return value
}

func mapGitHubConnection(value any) map[string]any {
	connection, ok := object(value)
	if !ok {
		return map[string]any{"connected": false}
	}
	result := map[string]any{"connected": safeBoolean(connection["connected"])}
	if login, ok := connection["login"].(string); ok {
		result["login"] = safeString(login, 64)
	}
	if connection["connectedAt"] != nil {
		result["connectedAt"] = safeTime(connection["connectedAt"])
	}
	return result
}

func mapProjectInspection(value any) map[string]any {
	inspection, ok := object(value)
	if !ok {
		return nil
	}
	config, ok := object(inspection["config"])
	if !ok {
		return nil
	}
	runtime := config["runtime"]
	if runtime != "go" && runtime != "node" {
		return nil
	}
	keys := []any{}
	if values, valid := arrayOf(inspection["environmentKeys"]); valid {
		for _, key := range values {
			candidate := safeString(key, 128)
			if environmentKeyPattern.MatchString(candidate) {
				keys = append(keys, candidate)
			}
		}
	}
	warnings := []any{}
	if values, valid := arrayOf(inspection["warnings"]); valid {
		for _, warning := range values {
			if text := safeString(warning, 512); text != "" {
				warnings = append(warnings, text)
			}
		}
	}
	return map[string]any{
		"existingConfig": safeBoolean(inspection["existingConfig"]),
		"config": map[string]any{
			"runtime":      runtime,
			"buildCommand": safeString(config["buildCommand"], 4096),
			"startCommand": safeString(config["startCommand"], 4096),
			"healthPath":   safeString(config["healthPath"], 256),
			"memoryLimit":  safeString(config["memoryLimit"], 32),
			"cpuQuota":     safeNumber(config["cpuQuota"]),
			"tasksLimit":   safeNumber(config["tasksLimit"]),
		},
		"environmentKeys": keys,
		"warnings":        warnings,
	}
}

func mapGitHubRepo(value any) (map[string]any, bool) {
	repository, ok := object(value)
	if !ok {
		return nil, false
	}
	fullName := safeString(firstValue(repository, "fullName", "full_name"), 200)
	owner := ""
	name := safeString(repository["name"], 100)
	if fullName != "" {
		parts := strings.Split(fullName, "/")
		if len(parts) != 2 {
			return nil, false
		}
		owner = parts[0]
		if name == "" {
			name = parts[1]
		}
	} else if ownerData, ownerOK := object(repository["owner"]); ownerOK {
		owner = safeString(ownerData["login"], 39)
	}
	if !githubOwnerPattern.MatchString(owner) || !githubRepoPattern.MatchString(name) {
		return nil, false
	}
	description, ok := repository["description"].(string)
	if !ok {
		description = ""
	}
	language, ok := repository["language"].(string)
	if !ok {
		language = ""
	}
	defaultBranch := safeString(firstValue(repository, "defaultBranch", "default_branch"), 255)
	updatedAt := safeTime(firstValue(repository, "updatedAt", "updated_at"))
	result := map[string]any{
		"fullName":      owner + "/" + name,
		"name":          name,
		"description":   nullableString(description, 1000),
		"url":           "https://github.com/" + owner + "/" + name,
		"defaultBranch": defaultBranch,
		"language":      nullableString(language, 100),
		"private":       safeBoolean(repository["private"]),
		"updatedAt":     updatedAt,
	}
	if raw, exists := repository["branches"]; exists {
		branches := []any{}
		if values, valid := arrayOf(raw); valid {
			for _, item := range values {
				branch, valid := object(item)
				if !valid {
					continue
				}
				name := safeString(branch["name"], 255)
				commitSHA := safeString(branch["commitSha"], 64)
				if commitSHA == "" {
					if commit, ok := object(branch["commit"]); ok {
						commitSHA = safeString(commit["sha"], 64)
					}
				}
				if name != "" {
					branches = append(branches, map[string]any{"name": name, "commitSha": commitSHA})
				}
			}
		}
		result["branches"] = branches
	}
	return result, true
}

func nullableString(value string, limit int) any {
	if value == "" {
		return nil
	}
	return safeString(value, limit)
}

func routeURL(value any, deployment map[string]any, domains any) string {
	if deployment == nil || deployment["State"] != "RUNNING" {
		return ""
	}
	appName := safeSlug(deployment["AppName"])
	if appName == "" {
		return ""
	}
	text, ok := value.(string)
	if !ok || len(text) > 256 {
		return ""
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	allowedDomain := false
	if values, valid := arrayOf(domains); valid {
		for _, domain := range values {
			if safeDomain(domain) == host {
				allowedDomain = true
				break
			}
		}
	}
	if parsed.Scheme == "https" && allowedDomain && (port == "" || port == "443") {
		return "https://" + host
	}
	if parsed.Scheme != "http" || host != appName+".localhost" || port == "" {
		return ""
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return ""
	}
	return "http://" + host + ":" + strconv.Itoa(portNumber)
}

func configuredHTTPSURL(deployment map[string]any, domains any) string {
	if deployment == nil || deployment["State"] != "RUNNING" {
		return ""
	}
	values, valid := arrayOf(domains)
	if !valid {
		return ""
	}
	for _, value := range values {
		if domain := safeDomain(value); domain != "" {
			return "https://" + domain
		}
	}
	return ""
}
