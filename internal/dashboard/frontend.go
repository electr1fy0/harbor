package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	ui "paas/web"
)

func assetVersion(assets fs.FS, name string) string {
	data, err := fs.ReadFile(assets, name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// readPageData calls only explicit read operations through the same bounded,
// sanitized API used by browser actions. The page has already passed the host,
// peer and session checks; it never opens a second listener or control socket.
func (h *handler) readPageData(ctx context.Context, fn string, input any) (map[string]any, *apiFailure) {
	response, fail := invokeControlJSON(ctx, h.api, http.MethodPost, apiPrefix+fn, input, defaultControlWait)
	if fail != nil {
		return nil, fail
	}
	var result map[string]any
	if json.Unmarshal(response.body, &result) != nil || result == nil {
		return nil, operationFailure("Dashboard data could not be loaded.")
	}
	if response.status < 200 || response.status >= 300 {
		message, _ := result["error"].(string)
		if message == "" {
			message = "Dashboard data could not be loaded."
		}
		return nil, &apiFailure{response.status, message}
	}
	return result, nil
}

func (h *handler) frontendPage(r *http.Request) ui.Page {
	p := ui.Page{Path: r.URL.Path, Title: "Projects", Status: http.StatusOK, Public: h.config.Mode == ModePublic,
		CSS: "/assets/app.css?v=" + assetVersion(h.assets, "assets/app.css"), JS: "/assets/app.js?v=" + assetVersion(h.assets, "assets/app.js")}
	for _, l := range []ui.Link{{Label: "Projects", URL: "/"}, {Label: "Deployments", URL: "/deployments"}, {Label: "Services", URL: "/services"}, {Label: "Capacity", URL: "/capacity"}, {Label: "Settings", URL: "/settings"}} {
		l.Active = l.URL == p.Path || (l.URL == "/" && strings.HasPrefix(p.Path, "/projects/"))
		p.Nav = append(p.Nav, l)
	}
	if p.Path == "/login" {
		p.Title = "Sign in"
		return p
	}
	load := func(fn string, input map[string]any) map[string]any {
		data, fail := h.readPageData(r.Context(), fn, input)
		if fail != nil {
			if p.Error != "" {
				p.Error += "\n"
			}
			p.Error += fail.message
			if p.Status == 200 {
				p.Status = fail.status
			}
			return map[string]any{}
		}
		return data
	}
	empty := map[string]any{}
	switch p.Path {
	case "/", "/index.html":
		p.Description = ""
		p.Filter = ui.ProjectFilter{Query: r.URL.Query().Get("q"), Favorites: r.URL.Query().Get("favorites") == "1", Archived: r.URL.Query().Get("archived") == "1"}
		overview := load("getOverview", empty)
		apps := maps(overview["apps"])
		total, running, attention := len(apps), 0, 0
		for _, app := range apps {
			if str(obj(app["Production"]), "State") == "RUNNING" {
				running++
			}
			if deploymentErrorPatterns[str(obj(app["LatestAttempt"]), "State")] != "" {
				attention++
			}
		}
		if total > 0 {
			p.ShowProjectFilters = true
			p.Sections = append(p.Sections, ui.Section{ID: "summary", Title: "Workspace overview", Compact: true, Metrics: []ui.Metric{{Label: "Projects", Value: strconv.Itoa(total)}, {Label: "Running", Value: strconv.Itoa(running)}, {Label: "Needs attention", Value: strconv.Itoa(attention)}}})
		}
		if overview["connected"] == false {
			p.Error = str(overview, "error")
			if p.Error == "" {
				p.Error = "The control plane is unavailable."
			}
		}
		table := ui.Table{Headers: []string{"Project", "Production", "Latest attempt", "Created"}, EmptyTitle: "Deploy your first project", Empty: "Import a GitHub repository. Harbor will build it, check its health, and give it a URL.", EmptyLinks: []ui.Link{{Label: "Import project", URL: "/new"}}}
		type projectRow struct {
			row      ui.Row
			favorite bool
			name     string
		}
		var rows []projectRow
		for _, app := range apps {
			name := str(obj(app["Application"]), "Name")
			metadata, fail := h.readPageData(r.Context(), "operations", map[string]any{"action": "settings", "appName": name})
			if fail != nil {
				p.Error = "Project organization could not be loaded. Reload to try again."
			}
			meta := obj(metadata["operations"])
			if r.URL.Query().Get("archived") != "1" && meta["archived"] == true {
				continue
			}
			if r.URL.Query().Get("favorites") == "1" && meta["favorite"] != true {
				continue
			}
			if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))); q != "" && !strings.Contains(strings.ToLower(name+" "+listText(meta["tags"])), q) {
				continue
			}
			label := name
			if meta["favorite"] == true {
				label = "★ " + name
			}
			state := str(obj(app["Production"]), "State")
			if state == "" {
				state = "No production"
			}
			attempt := str(obj(app["LatestAttempt"]), "State")
			if attempt == "" {
				attempt = "No deployments"
			}
			rows = append(rows, projectRow{ui.Row{Cells: []ui.Cell{{Text: label, URL: projectURL(name, "")}, {Text: state, Badge: state}, {Text: attempt, Badge: attempt}, {Text: date(obj(app["Application"])["CreatedAt"])}}}, meta["favorite"] == true, name})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].favorite != rows[j].favorite {
				return rows[i].favorite
			}
			return rows[i].name < rows[j].name
		})
		for _, row := range rows {
			table.Rows = append(table.Rows, row.row)
		}
		if total > 0 && len(rows) == 0 {
			table.EmptyTitle = "No matching projects"
			table.Empty = "Try a different name or tag, or clear your filters to see all projects."
			table.EmptyLinks = []ui.Link{{Label: "Clear filters", URL: "/"}}
		}
		p.Sections = append(p.Sections, ui.Section{ID: "projects", Title: "Projects", Table: table})
	case "/new":
		p.Title = "Import project"
		p.Description = "Connect a repository, review its configuration, and deploy."
		fields := []ui.Field{field("name", "Project name", "text", "", true), field("repoURL", "GitHub HTTPS repository URL", "url", "", true), field("ref", "Branch, tag, or commit (blank uses default)", "text", "", false), field("rootDir", "Repository root directory", "text", "", false)}
		fields = append(fields, configFields("config.", empty)...)
		fields = append(fields, ui.Field{Name: "environment", Label: "Runtime environment · KEY=value, one per line", Type: "env", OmitEmpty: true}, field("sqliteService", "SQLite service name (optional)", "text", "", false), check("createSqlite", "Create the SQLite service if needed", false))
		fields[len(fields)-2].OmitEmpty = true
		p.Forms = append(p.Forms, ui.Form{ID: "import-project", Title: "Review and deploy", Description: "Inspect the repository before deploying. Changing the source or build settings requires a new inspection.", Function: "submitOnboardingJob", Submit: "Deploy project", Inspect: true, Disabled: true, Fields: fields})

	case "/deployments":
		p.Title = "Deployments"
		p.Description = "Follow repository builds and deployment jobs."
		jobs := load("automation", map[string]any{"action": "listJobs"})
		p.Sections = append(p.Sections, deploymentOverviewSection(load("getOverview", empty)), jobsSection(jobs))
	case "/capacity":
		p.Title = "Capacity"
		p.Description = "Resource use, storage growth, and deployment headroom."
		p.Sections = capacitySections(load("getCapacity", empty))
	case "/services":
		p.Title = "Services"
		p.Description = "Manage SQLite bindings, snapshots, and encrypted exports."
		data := load("dataServices", map[string]any{"action": "list"})
		data["apps"] = load("getOverview", empty)["apps"]
		view := r.URL.Query().Get("view")
		if view == "" {
			view = "services"
		}
		for _, tab := range []struct{ key, label string }{{"services", "Services"}, {"backups", "Backups"}, {"export", "Export"}} {
			p.Tabs = append(p.Tabs, ui.Link{Label: tab.label, URL: "/services?view=" + tab.key, Active: tab.key == view})
		}
		sections, forms := serviceSections(data), serviceForms(data, "")
		switch view {
		case "services":
			p.Sections = sections[:1]
			for _, form := range forms {
				if form.Action == "create" || form.Action == "link" || form.Action == "unlink" {
					p.Forms = append(p.Forms, form)
				}
			}
		case "backups":
			p.Sections = sections[1:]
			for _, form := range forms {
				if form.Action == "backup" || form.Action == "setBackupPolicy" || form.Action == "restoreBackup" {
					p.Forms = append(p.Forms, form)
				}
			}
		case "export":
			for _, form := range forms {
				if form.Action == "export" {
					p.Forms = append(p.Forms, form)
				}
			}
		default:
			p.Status = 404
			p.Error = "This services section could not be found."
		}
	case "/settings":
		p.Title = "Settings"
		p.Description = "Connections and workspace access."
		connection := load("getGitHubConnection", empty)
		status := "Not connected"
		if connection["connected"] == true {
			status = "Connected as " + str(connection, "login")
		}
		connectionSection := ui.Section{ID: "github-status", Title: "GitHub connection", Text: status}
		if connection["connected"] == true {
			connectionSection.Actions = []ui.Form{{ID: "disconnect-github", Function: "disconnectGitHub", Submit: "Disconnect GitHub", Inline: true}}
		}
		p.Sections = append(p.Sections, connectionSection)
		p.Forms = []ui.Form{{ID: "connect-github", Title: "Connect GitHub", Description: "Use a personal access token with access to the repositories you want to deploy. The token is stored encrypted and never returned.", Function: "connectGitHub", Submit: "Connect", Fields: []ui.Field{field("token", "Personal access token", "password", "", true)}}}
		if connection["connected"] == true {
			p.Forms[0].Title = "Update GitHub token"
			p.Forms[0].Submit = "Update token"
		}
	default:
		if !strings.HasPrefix(p.Path, "/projects/") {
			return p
		}
		p.Name = strings.TrimPrefix(p.Path, "/projects/")
		if !validAppName(p.Name) {
			p.Title = "Project not found"
			p.Status = 404
			p.Error = "This project could not be found."
			return p
		}
		p.Title = p.Name
		p.Description = ""
		p.Tab = r.URL.Query().Get("tab")
		if p.Tab == "" {
			p.Tab = "overview"
		}
		validTab := false
		for _, tab := range []struct{ key, label string }{{"overview", "Overview"}, {"deployments", "Deployments"}, {"logs", "Logs"}, {"settings", "Settings"}, {"operations", "Operations"}, {"automation", "Automation"}, {"observability", "Observability"}, {"services", "Data services"}} {
			p.Tabs = append(p.Tabs, ui.Link{Label: tab.label, URL: projectURL(p.Name, tab.key), Active: tab.key == p.Tab})
			if tab.key == p.Tab {
				validTab = true
			}
		}
		if !validTab {
			p.Status = 404
			p.Error = "Project section not found."
			return p
		}
		app := load("getApp", map[string]any{"name": p.Name})
		if p.Status != 200 {
			return p
		}
		h.projectPage(r, &p, app, load)
	}
	return presentPage(p)
}

func (h *handler) projectPage(r *http.Request, p *ui.Page, app map[string]any, load func(string, map[string]any) map[string]any) {
	name := p.Name
	switch p.Tab {
	case "overview":
		diag := obj(app["diagnostics"])
		production := obj(diag["production"])
		latest := obj(diag["latestAttempt"])
		if len(production) == 0 {
			d := obj(app["deployment"])
			if d["State"] == "RUNNING" {
				production = d
			}
		}
		prodState := str(production, "State")
		if prodState == "" {
			prodState = "Not serving"
		}
		attempt := str(latest, "State")
		if attempt == "" {
			attempt = "No attempts"
		}
		s := ui.Section{ID: "app-status", Title: "Deployment status", Text: str(diag, "summary"), Metrics: []ui.Metric{{Label: "Production", Value: ui.StatusLabel(prodState), Detail: shortID(str(production, "ID"))}, {Label: "Latest attempt", Value: ui.StatusLabel(attempt), Detail: shortID(str(latest, "ID"))}}}
		if target := str(app, "url"); target != "" {
			s.Links = append(s.Links, ui.Link{Label: "Open application ↗", URL: target})
		}
		p.Sections = append(p.Sections, s)
		if excerpt := str(diag, "excerpt"); excerpt != "" {
			p.Sections = append(p.Sections, ui.Section{ID: "diagnostics", Title: "Latest diagnostics", Log: excerpt})
		}
		actions := []string{}
		if prodState == "RUNNING" {
			actions = []string{"restart", "stop"}
		} else if str(obj(app["deployment"]), "ReleaseID") != "" {
			actions = []string{"start"}
		}
		for _, action := range actions {
			label := map[string]string{"start": "Start application", "restart": "Restart", "stop": "Stop"}[action]
			f := operationForm(name, action, label)
			f.Inline = true
			f.Danger = action == "stop"
			f.Success = map[string]string{"start": "Application started.", "restart": "Application restarted.", "stop": "Application stopped."}[action]
			p.Sections[0].Actions = append(p.Sections[0].Actions, f)
		}
		if _, fail := h.readPageData(r.Context(), "automation", map[string]any{"action": "getSource", "name": name}); fail == nil {
			p.Sections[0].Actions = append(p.Sections[0].Actions, ui.Form{ID: "retry-deploy", Function: "retryGitHubDeployment", Submit: "Deploy again", Inline: true, Fields: []ui.Field{hidden("name", name)}})
		}
		if id := str(latest, "ID"); id != "" && deploymentErrorPatterns[attempt] != "" {
			p.Forms = append(p.Forms, ui.Form{ID: "explain", Title: "Explain failure", Description: "Send redacted diagnostic evidence to the configured AI service.", Function: "explainFailure", Submit: "Explain failure", Fields: []ui.Field{hidden("name", name), hidden("deploymentID", id)}})
		}
		p.Sections = append(p.Sections, releasesSection(name, app))
	case "deployments":
		offset := pageOffset(r, "offset")
		history := load("operations", map[string]any{"action": "history", "appName": name, "limit": 20, "offset": offset})
		p.Sections = append(p.Sections, historySection(name, history, offset))
		id := r.URL.Query().Get("deployment")
		if id != "" && deploymentJobIDPattern.MatchString(id) {
			eventOffset := pageOffset(r, "eventsOffset")
			events := load("operations", map[string]any{"action": "events", "appName": name, "deploymentId": id, "limit": 50, "offset": eventOffset})
			s := ui.Section{ID: "events", Title: "Deployment events", Description: id, Table: ui.Table{Headers: []string{"Time", "Event", "Details"}, Empty: "No events recorded."}}
			for _, e := range maps(events["events"]) {
				s.Table.Rows = append(s.Table.Rows, ui.Row{Cells: []ui.Cell{{Text: date(e["createdAt"])}, {Text: str(e, "type")}, {Text: str(e, "payload")}}})
			}
			base := projectURL(name, "deployments") + "&deployment=" + url.QueryEscape(id)
			if eventOffset > 0 {
				s.Links = append(s.Links, ui.Link{Label: "Previous events", URL: base + "&eventsOffset=" + strconv.Itoa(max(0, eventOffset-50))})
			}
			if events["hasMore"] == true {
				s.Links = append(s.Links, ui.Link{Label: "Next events", URL: base + "&eventsOffset=" + strconv.Itoa(eventOffset+50)})
			}
			p.Sections = append(p.Sections, s)
		}
		p.Sections = append(p.Sections, releasesSection(name, app))
	case "logs":
		p.Description = "Follow application output or inspect a previous deployment."
		latestID := str(obj(obj(app["diagnostics"])["latestAttempt"]), "ID")
		if latestID == "" {
			latestID = str(obj(app["deployment"]), "ID")
		}
		fields := []ui.Field{hidden("name", name), {Name: "deploymentId", Label: "Deployment", Type: "select", Value: latestID, Options: []ui.Option{{Value: latestID, Label: "Latest attempt · " + shortID(latestID)}}}, selectField("source", "Log source", "all", []string{"all", "build", "runtime"}), check("paused", "Pause updates", false), check("follow", "Follow output", true), field("search", "Find in logs", "text", "", false)}
		history := load("operations", map[string]any{"action": "history", "appName": name, "limit": 100, "offset": 0})
		for _, entry := range maps(history["deployments"]) {
			d := obj(entry["deployment"])
			if str(d, "id") == latestID {
				continue
			}
			fields[1].Options = append(fields[1].Options, ui.Option{Value: str(d, "id"), Label: shortID(str(d, "id")) + " · " + ui.StatusLabel(str(d, "state"))})
		}
		logForm := ui.Form{ID: "live-logs", Title: "Live logs", Function: "observe", Action: "logs", Submit: "Refresh logs", Fields: fields}
		if latestID == "" {
			logForm.Disabled = true
			logForm.Unavailable = "Deploy this project before opening its logs."
		}
		p.Forms = append(p.Forms, logForm)
	case "settings":
		settings := obj(app["settings"])
		keyHelp := "No variables configured yet."
		if keys := listText(settings["environmentKeys"]); keys != "" {
			keyHelp = "Configured keys: " + keys + "."
		}
		p.Forms = append(p.Forms, ui.Form{ID: "runtime-settings", Title: "Resource limits", Function: "configureApp", Submit: "Save limits", Fields: []ui.Field{hidden("name", name), field("memoryLimit", "Memory limit", "text", str(settings, "memoryLimit"), true), numberField("cpuQuota", "CPU quota (cores)", settings["cpuQuota"], "0.01", "256", "0.01"), numberField("tasksLimit", "Task limit", settings["tasksLimit"], "1", "65536", "1")}}, ui.Form{ID: "environment-settings", Title: "Runtime environment", Description: "Add or replace variables with KEY=value entries. Use the remove list to delete existing keys.", Function: "configureApp", Submit: "Save environment", Fields: []ui.Field{hidden("name", name), {Name: "environment", Label: "Variables", Type: "env", Help: keyHelp, OmitEmpty: true}, {Name: "removeEnvironment", Label: "Remove variables", Type: "list", OmitEmpty: true}}})
	case "operations":
		result := load("operations", map[string]any{"action": "settings", "appName": name})
		meta := obj(result["operations"])
		p.Sections = append(p.Sections, ui.Section{ID: "operation-status", Title: "Application health", Metrics: []ui.Metric{{Label: "Health", Value: str(meta, "healthStatus")}, {Label: "Consecutive failures", Value: value(meta["healthFailureCount"])}, {Label: "Restart attempts", Value: value(meta["restartAttempt"])}, {Label: "Pending settings", Value: value(meta["pendingSettings"])}}})
		p.Forms = operationForms(name, meta)
	case "automation":
		source, fail := h.readPageData(r.Context(), "automation", map[string]any{"action": "getSource", "name": name})
		if fail != nil && fail.status != 404 {
			p.Error = fail.message
			p.Status = fail.status
		}
		if fail == nil {
			p.Forms = append(p.Forms, sourceForm(name, source))
		} else {
			p.Sections = append(p.Sections, ui.Section{ID: "no-source", Title: "Repository source", Text: "This project has no saved GitHub source."})
		}
		p.Forms = append(p.Forms, ui.Form{ID: "redeploy", Title: "Redeploy saved source", Function: "automation", Action: "redeploy", Submit: "Redeploy", Fields: []ui.Field{hidden("name", name), check("clearCache", "Clear build cache first", false)}}, ui.Form{ID: "create-hook", Title: "Create deployment hook", Description: "The secret is shown once. Use it for signed GitHub push events or authenticated CI deployments.", Function: "automation", Action: "createHook", Submit: "Create hook", Fields: []ui.Field{hidden("name", name), field("branch", "Branch", "text", "main", true)}})
		hooks := load("automation", map[string]any{"action": "listHooks", "name": name})
		p.Sections = append(p.Sections, hooksSection(hooks), jobsSection(load("automation", map[string]any{"action": "listJobs", "name": name})))
	case "observability":
		window := r.URL.Query().Get("window")
		if window != "day" {
			window = "hour"
		}
		data := load("observe", map[string]any{"action": "get", "name": name, "window": window, "limit": 120})
		p.Sections = observeSections(name, data)
		p.Forms = observeForms(name, obj(data["settings"]))
	case "services":
		data := load("dataServices", map[string]any{"action": "list"})
		data["apps"] = load("getOverview", map[string]any{})["apps"]
		p.Sections = serviceSections(data)
		p.Forms = serviceForms(data, name)
	}
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func maps(v any) []map[string]any {
	var out []map[string]any
	list, _ := v.([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func value(v any) string {
	if v == nil {
		return "Unavailable"
	}
	return fmt.Sprint(v)
}
func listText(v any) string {
	if list, ok := v.([]any); ok {
		a := []string{}
		for _, s := range list {
			a = append(a, fmt.Sprint(s))
		}
		return strings.Join(a, ", ")
	}
	return ""
}
func date(v any) string {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "—"
	}
	return t.UTC().Format("02 Jan 2006 15:04 UTC")
}
func bytesText(v any) string {
	n, ok := numberValue(v)
	if !ok {
		return "Unavailable"
	}
	if n < 1024 {
		return fmt.Sprintf("%.0f B", n)
	}
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		n /= 1024
		if math.Abs(n) < 1024 {
			return fmt.Sprintf("%.1f %s", n, unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", n)
}
func projectURL(name, tab string) string {
	u := "/projects/" + url.PathEscape(name)
	if tab != "" {
		u += "?tab=" + url.QueryEscape(tab)
	}
	return u
}
func pageOffset(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return min(max(n, 0), 1000000)
}
func hidden(name, v string) ui.Field { return ui.Field{Name: name, Value: v, Type: "hidden"} }
func field(name, label, typ, v string, required bool) ui.Field {
	return ui.Field{Name: name, Label: label, Type: typ, Value: v, Required: required}
}
func numberField(name, label string, v any, min, max, step string) ui.Field {
	f := field(name, label, "number", value(v), true)
	if v == nil {
		f.Value = ""
	}
	f.Min = min
	f.Max = max
	f.Step = step
	return f
}
func check(name, label string, v bool) ui.Field {
	return ui.Field{Name: name, Label: label, Type: "checkbox", Checked: v}
}
func selectField(name, label, v string, options []string) ui.Field {
	f := field(name, label, "select", v, true)
	for _, s := range options {
		f.Options = append(f.Options, ui.Option{Value: s, Label: s})
	}
	return f
}
func configFields(prefix string, c map[string]any) []ui.Field {
	fields := []ui.Field{selectField(prefix+"runtime", "Runtime", str(c, "runtime"), []string{"", "go", "node"}), field(prefix+"buildCommand", "Build command", "text", str(c, "buildCommand"), false), field(prefix+"startCommand", "Start command", "text", str(c, "startCommand"), false), field(prefix+"healthPath", "Health path", "text", str(c, "healthPath"), false), field(prefix+"memoryLimit", "Memory limit", "text", str(c, "memoryLimit"), false), numberField(prefix+"cpuQuota", "CPU quota (cores)", c["cpuQuota"], "0.01", "256", "0.01"), numberField(prefix+"tasksLimit", "Task limit", c["tasksLimit"], "1", "65536", "1")}
	for i := range fields {
		fields[i].Required = false
		fields[i].OmitEmpty = true
	}
	return fields
}
func operationForm(name, action, label string) ui.Form {
	return ui.Form{ID: "operation-" + action, Function: "operations", Action: action, Submit: label, Fields: []ui.Field{hidden("appName", name)}}
}
