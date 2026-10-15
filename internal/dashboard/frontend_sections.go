package dashboard

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	ui "paas/web"
)

func releasesSection(name string, app map[string]any) ui.Section {
	s := ui.Section{ID: "releases", Title: "Releases", Table: ui.Table{Headers: []string{"Release", "Runtime", "Created", "Status", "Actions"}, Empty: "No releases available."}}
	for _, release := range maps(app["releases"]) {
		id := str(release, "id")
		status := "Available"
		if release["isActive"] == true {
			status = "Active"
		} else if release["artifactAvailable"] != true {
			status = "Artifact removed"
		}
		row := ui.Row{Cells: []ui.Cell{{Text: shortID(id)}, {Text: str(release, "runtime")}, {Text: date(release["createdAt"])}, {Text: status}}}
		if release["isActive"] != true && release["artifactAvailable"] == true {
			row.Actions = []ui.Form{{ID: "rollback-" + id, Function: "rollbackApp", Submit: "Roll back", Inline: true, Fields: []ui.Field{hidden("name", name), hidden("releaseID", id)}}}
		} else {
			row.Cells = append(row.Cells, ui.Cell{})
		}
		s.Table.Rows = append(s.Table.Rows, row)
	}
	return s
}
func historySection(name string, history map[string]any, offset int) ui.Section {
	s := ui.Section{ID: "attempts", Title: "All deployment attempts", Description: "Failed builds, candidates, restarts, and rollbacks are included.", Table: ui.Table{Headers: []string{"Attempt", "State", "Created", "Stage durations", "Actions"}, Empty: "No deployment attempts."}}
	for _, entry := range maps(history["deployments"]) {
		d := obj(entry["deployment"])
		id := str(d, "id")
		durations := []string{}
		for stage, v := range obj(entry["stages"]) {
			n, _ := numberValue(v)
			durations = append(durations, fmt.Sprintf("%s: %.2fs", strings.TrimSuffix(stage, "Milliseconds"), n/1000))
		}
		row := ui.Row{Cells: []ui.Cell{{Text: id, URL: projectURL(name, "deployments") + "&deployment=" + url.QueryEscape(id)}, {Text: str(d, "state"), Badge: str(d, "state")}, {Text: date(d["createdAt"])}, {Text: strings.Join(durations, "\n")}}}
		state := str(d, "state")
		if state == "BUILDING" || state == "STARTING" || state == "HEALTH_CHECKING" || state == "QUEUED" || state == "BUILT" {
			row.Actions = []ui.Form{{ID: "cancel-" + id, Function: "operations", Action: "cancelCandidate", Submit: "Cancel candidate", Inline: true, Fields: []ui.Field{hidden("appName", name), hidden("deploymentId", id)}}}
		} else {
			row.Cells = append(row.Cells, ui.Cell{Text: str(d, "error")})
		}
		s.Table.Rows = append(s.Table.Rows, row)
	}
	if offset > 0 {
		s.Links = append(s.Links, ui.Link{Label: "Previous attempts", URL: projectURL(name, "deployments") + "&offset=" + strconv.Itoa(max(0, offset-20))})
	}
	if history["hasMore"] == true {
		s.Links = append(s.Links, ui.Link{Label: "Next attempts", URL: projectURL(name, "deployments") + "&offset=" + strconv.Itoa(offset+20)})
	}
	return s
}
func jobsSection(data map[string]any) ui.Section {
	s := ui.Section{ID: "jobs", Title: "Repository deployment jobs", Table: ui.Table{Headers: []string{"Project", "Job", "State", "Stage", "Updated", "Actions"}, Empty: "No repository deployment jobs."}}
	for _, j := range maps(data["jobs"]) {
		id := str(j, "id")
		name := str(j, "name")
		state := str(j, "state")
		row := ui.Row{Cells: []ui.Cell{{Text: name, URL: projectURL(name, "")}, {Text: shortID(id)}, {Text: state, Badge: state}, {Text: str(j, "stage")}, {Text: date(j["updatedAt"])}}}
		row.Actions = append(row.Actions, ui.Form{ID: "job-" + id, Function: "getOnboardingJob", Submit: "View logs", Inline: true, Fields: []ui.Field{hidden("id", id)}})
		if state == "RUNNING" || state == "QUEUED" {
			row.Actions = append(row.Actions, ui.Form{ID: "cancel-job-" + id, Function: "cancelOnboardingJob", Submit: "Cancel", Inline: true, Fields: []ui.Field{hidden("id", id)}})
		}
		s.Table.Rows = append(s.Table.Rows, row)
	}
	return s
}
func operationForms(name string, meta map[string]any) []ui.Form {
	metadata := operationForm(name, "configureMetadata", "Save metadata")
	metadata.Title = "Project organization"
	metadata.Fields = append(metadata.Fields, check("favorite", "Favorite project", meta["favorite"] == true), ui.Field{Name: "tags", Label: "Tags · comma separated", Type: "list", Value: listText(meta["tags"])}, ui.Field{Name: "requiredEnvironmentKeys", Label: "Required environment keys", Type: "list", Value: listText(meta["requiredEnvironmentKeys"])})
	archiveAction, label := "archive", "Archive project"
	if meta["archived"] == true {
		archiveAction = "unarchive"
		label = "Unarchive project"
	}
	archive := operationForm(name, archiveAction, label)
	archive.Title = "Archive"
	health := operationForm(name, "healthPolicy", "Save health policy")
	health.Title = "Ongoing health checks"
	health.Description = "Set the interval to 0 to disable probes."
	health.Fields = append(health.Fields, numberField("intervalSeconds", "Interval (seconds)", meta["healthProbeIntervalSeconds"], "0", "86400", "1"), numberField("timeoutSeconds", "Probe timeout (seconds)", meta["healthProbeTimeoutSeconds"], "1", "300", "1"), numberField("failureThreshold", "Failure threshold", meta["healthFailureThreshold"], "1", "100", "1"))
	restart := operationForm(name, "restartPolicy", "Save recovery policy")
	restart.Title = "Crash recovery"
	restart.Fields = append(restart.Fields, check("enabled", "Enable automatic restart", meta["automaticRestartEnabled"] == true), numberField("maxAttempts", "Maximum attempts", meta["automaticRestartMaxAttempts"], "1", "20", "1"), numberField("baseDelaySeconds", "Initial delay (seconds)", meta["automaticRestartBaseDelaySeconds"], "1", "300", "1"))
	preflight := operationForm(name, "preflight", "Check deployment readiness")
	preflight.Title = "Deployment preflight"
	retention := operationForm(name, "retention", "Save retention and prune")
	retention.Title = "Release retention"
	retention.Description = "Active, protected rollback, and in-flight releases are retained."
	retention.Fields = append(retention.Fields, numberField("retainReleaseCount", "Releases to retain", meta["retainReleaseCount"], "2", "1000", "1"))
	prune := operationForm(name, "pruneReleases", "Prune eligible releases")
	prune.Title = "Prune old artifacts"
	remove := operationForm(name, "delete", "Delete project")
	remove.Title = "Delete project"
	remove.Description = "Stops the project and removes its routes and history. Type the project name to confirm."
	remove.Confirm = name
	remove.Redirect = "/"
	remove.Fields = append(remove.Fields, check("preserveData", "Preserve database files", true), field("_confirmation", "Project name", "text", "", true))
	return []ui.Form{metadata, archive, health, restart, preflight, retention, prune, remove}
}
func sourceForm(name string, data map[string]any) ui.Form {
	src := obj(data["source"])
	f := ui.Form{ID: "source-settings", Title: "Repository and build settings", Description: "Build environment values are write-only. Leave them blank to preserve stored values.", Function: "automation", Action: "updateSource", Submit: "Save source settings"}
	f.Fields = []ui.Field{hidden("source.name", name), field("source.repoURL", "GitHub repository URL", "url", str(src, "repoURL"), true), field("source.ref", "Branch, tag, or commit", "text", str(src, "ref"), false), field("source.rootDir", "Root directory", "text", str(src, "rootDir"), false)}
	f.Fields = append(f.Fields, configFields("source.config.", obj(src["selection"]))...)
	timeout := src["buildTimeoutSeconds"]
	if timeout == nil || timeout == float64(0) {
		timeout = 300
	}
	f.Fields = append(f.Fields, numberField("source.buildTimeoutSeconds", "Build timeout (seconds)", timeout, "1", "3600", "1"), ui.Field{Name: "source.includePaths", Label: "Include paths · one pattern per line", Type: "list", Value: listText(src["includePaths"])}, ui.Field{Name: "source.ignorePaths", Label: "Ignore paths · one pattern per line", Type: "list", Value: listText(src["ignorePaths"])}, ui.Field{Name: "source.buildEnvironment", Label: "Build environment · KEY=value, one per line", Help: "Stored keys: " + listText(data["buildEnvironmentKeys"]), Type: "env", OmitEmpty: true}, check("_clearBuildEnvironment", "Clear all stored build variables", false))
	return f
}
func hooksSection(data map[string]any) ui.Section {
	s := ui.Section{ID: "hooks", Title: "Deployment hooks", Table: ui.Table{Headers: []string{"Hook", "Branch", "Created", "Actions"}, Empty: "No deployment hooks configured."}}
	for _, hook := range maps(data["hooks"]) {
		id := str(hook, "id")
		s.Table.Rows = append(s.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: shortID(id)}, {Text: str(hook, "branch")}, {Text: date(hook["createdAt"])}}, Actions: []ui.Form{{ID: "regenerate-" + id, Function: "automation", Action: "regenerateHook", Submit: "Rotate secret", Inline: true, Fields: []ui.Field{hidden("id", id)}}, {ID: "revoke-" + id, Function: "automation", Action: "revokeHook", Submit: "Revoke", Inline: true, Fields: []ui.Field{hidden("id", id)}}}})
	}
	return s
}
func serviceSections(data map[string]any) []ui.Section {
	s := ui.Section{ID: "services", Title: "Managed SQLite services", Table: ui.Table{Headers: []string{"Service", "State", "Database", "WAL", "Backup schedule", "Bindings"}, EmptyTitle: "A database for your projects", Empty: "Create a SQLite service, then bind it to a project. Harbor keeps its data across deployments.", EmptyLinks: []ui.Link{{Label: "Create a service", URL: "#create-service"}}}}
	backups := ui.Section{ID: "backups", Title: "Database backups", Table: ui.Table{Headers: []string{"Snapshot", "Project / service", "Created", "Size", "Verification", "Actions"}, EmptyTitle: "No backups yet", Empty: "Create a snapshot of a project database, or set a schedule to keep backups automatically.", EmptyLinks: []ui.Link{{Label: "Create a snapshot", URL: "#service-backup"}}}}
	for _, svc := range maps(data["services"]) {
		bindings := []string{}
		for _, binding := range maps(svc["bindings"]) {
			bindings = append(bindings, str(binding, "appName"))
		}
		policy := obj(svc["backupPolicy"])
		schedule := "Disabled"
		if policy["enabled"] == true {
			schedule = str(policy, "frequency") + " · retain " + value(policy["retention"])
		}
		s.Table.Rows = append(s.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: str(svc, "name")}, {Text: str(svc, "state"), Badge: str(svc, "state")}, {Text: bytesText(svc["databaseBytes"])}, {Text: bytesText(svc["walBytes"])}, {Text: schedule}, {Text: strings.Join(bindings, ", ")}}})
		for _, b := range maps(svc["backups"]) {
			id := str(b, "id")
			backups.Table.Rows = append(backups.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: shortID(id)}, {Text: str(b, "appName") + " / " + str(b, "serviceName")}, {Text: date(b["createdAt"])}, {Text: bytesText(b["sizeBytes"])}, {Text: str(b, "verification"), Badge: str(b, "verification")}}, Actions: []ui.Form{{ID: "verify-" + id, Function: "dataServices", Action: "verifyBackup", Submit: "Verify", Inline: true, Fields: []ui.Field{hidden("backupId", id)}}, {ID: "download-" + id, Function: "dataServices", Action: "downloadBackup", Submit: "Download", Inline: true, Fields: []ui.Field{hidden("backupId", id)}}}})
		}
	}
	return []ui.Section{s, backups}
}
func serviceForms(data map[string]any, name string) []ui.Form {
	service := ui.Field{Name: "serviceName", Label: "Service", Type: "select", Required: true, Options: []ui.Option{{Value: "", Label: "Choose a service"}}}
	for _, svc := range maps(data["services"]) {
		service.Options = append(service.Options, ui.Option{Value: str(svc, "name"), Label: str(svc, "name")})
	}
	appField := ui.Field{Name: "appName", Label: "Project", Type: "select", Required: true, Options: []ui.Option{{Label: "Choose a project"}}}
	for _, app := range maps(data["apps"]) {
		project := str(obj(app["Application"]), "Name")
		appField.Options = append(appField.Options, ui.Option{Value: project, Label: project})
	}
	if name != "" {
		appField = hidden("appName", name)
	}
	forms := []ui.Form{{ID: "create-service", Title: "Create SQLite service", Function: "dataServices", Action: "create", Submit: "Create service", Fields: []ui.Field{hidden("serviceType", "sqlite"), field("name", "Service name", "text", "", true)}}}
	for _, item := range []struct{ action, title, label string }{{"link", "Bind a service", "Bind service"}, {"unlink", "Remove a binding", "Unbind service"}, {"backup", "Create a snapshot", "Create snapshot"}} {
		forms = append(forms, ui.Form{ID: "service-" + item.action, Title: item.title, Description: "Binding changes apply on the next restart or deployment.", Function: "dataServices", Action: item.action, Submit: item.label, Fields: []ui.Field{appField, service}})
	}
	forms = append(forms, ui.Form{ID: "backup-policy", Title: "Scheduled snapshots", Function: "dataServices", Action: "setBackupPolicy", Submit: "Save schedule", Fields: []ui.Field{service, selectField("frequency", "Frequency", "daily", []string{"hourly", "daily", "weekly"}), numberField("retention", "Snapshots to retain", 7, "1", "1000", "1"), check("enabled", "Enable schedule", true)}})
	backup := ui.Field{Name: "backupId", Label: "Snapshot", Type: "select", Required: true, Options: []ui.Option{{Value: "", Label: "Choose a snapshot"}}}
	for _, svc := range maps(data["services"]) {
		for _, b := range maps(svc["backups"]) {
			backup.Options = append(backup.Options, ui.Option{Value: str(b, "id"), Label: str(b, "appName") + " · " + str(b, "id")})
		}
	}
	forms = append(forms, ui.Form{ID: "restore-backup", Title: "Restore to a new project", Description: "Restores into a new project without overwriting a running database.", Function: "dataServices", Action: "restoreBackup", Submit: "Restore snapshot", Fields: []ui.Field{backup, field("newAppName", "New project name", "text", "", true), field("serviceName", "New service name", "text", "", true)}}, ui.Form{ID: "export", Title: "Encrypted Harbor export", Function: "dataServices", Action: "export", Submit: "Download encrypted export", Fields: []ui.Field{field("passphrase", "Passphrase (12+ bytes)", "password", "", true), {Name: "appNames", Label: "Project names · comma separated (blank includes all)", Type: "list"}}})
	for i := range forms {
		f := &forms[i]
		if f.Action == "link" || f.Action == "unlink" || f.Action == "backup" || f.Action == "setBackupPolicy" {
			if len(maps(data["services"])) == 0 {
				f.Disabled = true
				f.Unavailable = "Create a SQLite service first."
			}
			if (f.Action == "link" || f.Action == "unlink" || f.Action == "backup") && name == "" && len(appField.Options) < 2 {
				f.Disabled = true
				f.Unavailable = "Import a project first, then choose its database service."
			}
		}
		if f.Action == "restoreBackup" && len(backup.Options) < 2 {
			f.Disabled = true
			f.Unavailable = "Create a snapshot before restoring a new project."
		}
		if f.Action == "backup" {
			f.Description = "Take a consistent snapshot of a project's SQLite database."
		}
		if f.Action == "export" {
			f.Description = "Download an encrypted archive of your projects and databases. Keep the passphrase somewhere safe; it is required to restore the archive."
		}
	}
	return forms
}
func capacitySections(c map[string]any) []ui.Section {
	host, disk, builds, headroom := obj(c["host"]), obj(c["disk"]), obj(c["builds"]), obj(c["headroom"])
	cpu := "Unavailable"
	if n, ok := numberValue(host["cpuUsagePercent"]); ok {
		cpu = fmt.Sprintf("%.1f%%", n)
	}
	sections := []ui.Section{{ID: "host-capacity", Title: "Host", Description: "Sampled " + date(c["sampledAt"]), Metrics: []ui.Metric{{Label: "Memory available", Value: bytesText(host["memoryAvailableBytes"]), Detail: "Total: " + bytesText(host["memoryTotalBytes"])}, {Label: "CPU use", Value: cpu, Detail: value(host["logicalCPUs"]) + " logical CPUs"}, {Label: "Disk available", Value: bytesText(disk["filesystemAvailableBytes"]), Detail: "Total: " + bytesText(disk["filesystemTotalBytes"])}, {Label: "Deployment headroom", Value: bytesText(headroom["estimatedMemoryAfterReserveBytes"]), Detail: "After host reserve"}}}, {ID: "storage", Title: "Harbor storage", Metrics: []ui.Metric{{Label: "Data", Value: bytesText(disk["harborDataBytes"]), Detail: "Change: " + bytesText(disk["harborDataChangeBytes"])}, {Label: "Releases", Value: bytesText(disk["releasesBytes"])}, {Label: "Build cache", Value: bytesText(disk["buildCacheBytes"])}, {Label: "Logs", Value: bytesText(disk["logsBytes"])}}}, {ID: "build-capacity", Title: "Active systemd builds", Metrics: []ui.Metric{{Label: "Active builds", Value: value(builds["systemdManagedActiveCount"])}, {Label: "Memory", Value: bytesText(builds["memoryCurrentBytes"]), Detail: "Limit: " + bytesText(builds["memoryLimitBytes"])}, {Label: "CPU cores", Value: value(builds["cpuUsageCores"])}}}}
	s := ui.Section{ID: "app-capacity", Title: "Applications", Table: ui.Table{Headers: []string{"Project", "State", "Memory / limit", "Peak", "CPU cores / limit", "Tasks / limit", "Source"}, Empty: "No application resource measurements."}}
	for _, a := range maps(c["apps"]) {
		s.Table.Rows = append(s.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: str(a, "name"), URL: projectURL(str(a, "name"), "")}, {Text: str(a, "state"), Badge: str(a, "state")}, {Text: bytesText(a["memoryCurrentBytes"]) + " / " + bytesText(a["memoryLimitBytes"])}, {Text: bytesText(a["memoryPeakBytes"])}, {Text: value(a["cpuUsageCores"]) + " / " + value(a["cpuQuotaCores"])}, {Text: value(a["tasksCurrent"]) + " / " + value(a["tasksLimit"])}, {Text: str(a, "metricsSource")}}})
	}
	sections = append(sections, s, ui.Section{ID: "capacity-caveats", Title: "Measurement notes", Text: listText(c["caveats"]) + "\n" + str(obj(c["platform"]), "reason")})
	return sections
}
func observeSections(name string, data map[string]any) []ui.Section {
	sections := []ui.Section{}
	history := maps(data["history"])
	for _, metric := range []struct {
		key, label string
		bytes      bool
	}{{"memoryCurrentBytes", "Memory history", true}, {"cpuUsageCores", "CPU history · cores", false}, {"tasksCurrent", "Task history", false}} {
		s := ui.Section{ID: "history-" + metric.key, Title: metric.label, Description: "Missing measurements are unavailable. Samples are retained on this server.", Links: []ui.Link{{Label: "Last hour", URL: projectURL(name, "observability") + "&window=hour"}, {Label: "Last 24 hours", URL: projectURL(name, "observability") + "&window=day"}}}
		type sample struct {
			i     int
			n     float64
			label string
		}
		samples := []sample{}
		peak := float64(0)
		for i, pt := range history {
			for _, app := range maps(obj(pt["snapshot"])["apps"]) {
				if str(app, "name") != name {
					continue
				}
				if n, ok := numberValue(app[metric.key]); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
					label := value(n)
					if metric.bytes {
						label = bytesText(n)
					}
					samples = append(samples, sample{i, n, date(pt["at"]) + " · " + label})
					peak = math.Max(peak, n)
				}
			}
		}
		for _, v := range samples {
			y := float64(110)
			if peak > 0 {
				y -= v.n / peak * 100
			}
			s.Chart = append(s.Chart, ui.Point{X: 10 + float64(len(history)-1-v.i)/float64(max(1, len(history)-1))*580, Y: y, Label: v.label})
		}
		if len(samples) == 0 {
			s.Text = "No measurements available yet."
		}
		sections = append(sections, s)
	}
	traffic := ui.Section{ID: "traffic", Title: "HTTP traffic", Description: "Counters cover the current daemon lifetime.", Table: ui.Table{Headers: []string{"Host", "Requests", "Transferred", "Mean latency", "Statuses"}, Empty: "No requests recorded yet."}}
	for _, t := range maps(data["traffic"]) {
		requests, _ := numberValue(t["requests"])
		latency, _ := numberValue(t["totalLatencyMs"])
		mean := "—"
		if requests > 0 {
			mean = fmt.Sprintf("%.1f ms", latency/requests)
		}
		statuses := []string{}
		for k, v := range obj(t["statusCodes"]) {
			statuses = append(statuses, k+": "+value(v))
		}
		traffic.Table.Rows = append(traffic.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: str(t, "host")}, {Text: value(t["requests"])}, {Text: bytesText(t["bytes"])}, {Text: mean}, {Text: strings.Join(statuses, ", ")}}})
	}
	notices := ui.Section{ID: "notifications", Title: "Notification deliveries", Table: ui.Table{Headers: []string{"Time", "Event", "Delivered"}, Empty: "No notifications sent."}}
	for _, n := range maps(data["notifications"]) {
		notices.Table.Rows = append(notices.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: date(n["at"])}, {Text: str(n, "event")}, {Text: value(n["success"])}}})
	}
	return append(sections, traffic, notices)
}
func observeForms(name string, s map[string]any) []ui.Form {
	f := ui.Form{ID: "observe-settings", Title: "Routing and notifications", Description: "Maintenance returns HTTP 503. Leave the webhook blank to preserve its stored value.", Function: "observe", Action: "configure", Submit: "Save settings"}
	f.Fields = []ui.Field{hidden("name", name), hidden("settings.name", name), check("settings.maintenance", "Enable maintenance mode", s["maintenance"] == true), field("settings.maintenanceMessage", "Maintenance message", "text", str(s, "maintenanceMessage"), false), numberField("settings.memoryPercent", "Memory alert threshold (%)", s["memoryPercent"], "0", "100", "1"), numberField("settings.diskPercent", "Disk alert threshold (%)", s["diskPercent"], "0", "100", "1"), numberField("settings.cooldownSeconds", "Notification cooldown (seconds)", s["cooldownSeconds"], "30", "86400", "1"), ui.Field{Name: "settings.events", Label: "Notification events · comma separated", Type: "list", Value: listText(s["events"]), Help: "BUILD_FAILED, START_FAILED, UNHEALTHY, CRASHED, OOM_KILLED, RUNNING, STOPPED"}, ui.Field{Name: "webhookURL", Label: "New notification webhook URL", Type: "password", OmitEmpty: true, Help: "Configured: " + value(s["webhookConfigured"])}, check("_clearWebhook", "Remove stored webhook", false)}
	return []ui.Form{f, {ID: "diagnostic-bundle", Title: "Diagnostic export", Description: "Download redacted status, logs, and metrics.", Function: "observe", Action: "bundle", Submit: "Download diagnostics", Fields: []ui.Field{hidden("name", name)}}}
}

// deploymentOverviewSection includes local/CLI deploys as well as repository jobs.
func deploymentOverviewSection(overview map[string]any) ui.Section {
	s := ui.Section{ID: "deployment-overview", Title: "Latest deployment attempts", Table: ui.Table{Headers: []string{"Project", "Production", "Latest attempt", "Started"}, Empty: "No deployments yet."}}
	for _, app := range maps(overview["apps"]) {
		name := str(obj(app["Application"]), "Name")
		production, latest := obj(app["Production"]), obj(app["LatestAttempt"])
		s.Table.Rows = append(s.Table.Rows, ui.Row{Cells: []ui.Cell{
			{Text: name, URL: projectURL(name, "deployments")},
			{Text: str(production, "State"), Badge: str(production, "State")},
			{Text: str(latest, "State"), Badge: str(latest, "State")},
			{Text: date(latest["CreatedAt"])},
		}})
	}
	return s
}
