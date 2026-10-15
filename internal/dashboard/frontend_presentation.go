package dashboard

import (
	"strings"

	ui "paas/web"
)

func shortID(id string) string {
	if len(id) > 16 {
		return id[:4] + "…" + id[len(id)-8:]
	}
	return id
}

// presentPage defines disclosure and hierarchy without changing API contracts.
// State-dependent controls are built from actual page data before presentation.
func presentPage(p ui.Page) ui.Page {
	switch p.Path {
	case "/", "/index.html", "/deployments":
		p.HeaderLinks = []ui.Link{{Label: "Import project", URL: "/new"}}
	case "/new":
		p.HeaderLinks = []ui.Link{{Label: "Cancel", URL: "/"}}
	}
	for i := range p.Forms {
		f := &p.Forms[i]
		switch f.ID {
		case "import-project":
			f.Title = "Repository"
			f.Description = "Enter a GitHub URL. Inspect it to detect the build settings before deploying."
		case "connect-github":
			f.Description = "Connect a personal access token to browse and deploy private repositories."
		case "runtime-settings":
			f.Description = "Limits apply the next time this project deploys or restarts."
		case "environment-settings":
			f.Description = "Add or update variables. Existing values stay private; leave a key out to keep it unchanged."
		case "live-logs":
			f.Title = "Live logs"
		case "operation-archive", "operation-healthPolicy", "operation-restartPolicy", "operation-retention", "operation-pruneReleases", "operation-delete", "service-unlink", "restore-backup", "source-settings", "create-hook", "diagnostic-bundle":
			f.Collapsible = true
		}
		if f.Confirm != "" || f.Action == "unlink" {
			f.Danger = true
		}
		if f.Confirm != "" {
			f.Disabled = true
		}
		for j := range f.Fields {
			v := &f.Fields[j]
			switch {
			case v.Name == "repoURL" || v.Name == "source.repoURL":
				v.Placeholder = "https://github.com/owner/repository"
			case v.Name == "name" && v.Type != "hidden":
				v.Placeholder = "my-project"
			case v.Type == "env":
				v.Placeholder = "API_KEY=your-value\nAPP_MODE=production"
				v.Help = strings.TrimSpace(v.Help + " One KEY=value per line. Values are never shown after saving.")
			}
			if f.Inspect {
				switch {
				case strings.HasPrefix(v.Name, "config."):
					v.Group = "Build settings"
				case v.Name == "ref" || v.Name == "rootDir":
					v.Group = "Repository options"
				case v.Name == "environment":
					v.Group = "Environment variables"
				case v.Name == "sqliteService" || v.Name == "createSqlite":
					v.Group = "Database"
				}
			}
			if v.Name == "ref" {
				v.Label = "Branch, tag, or commit"
				v.Help = "Leave blank to use the repository's default branch."
			}
			if v.Name == "rootDir" {
				v.Label = "Root directory"
				v.Placeholder = "apps/api"
				v.Help = "Leave blank if the app is in the repository root."
			}
			if v.Name == "removeEnvironment" {
				v.Placeholder = "OLD_API_KEY, UNUSED_SETTING"
				v.Help = "Separate variable names with commas or new lines."
			}
		}
	}
	// Ordinary saves have specific feedback and never force a page reload.
	for i := range p.Forms {
		f := &p.Forms[i]
		switch f.ID {
		case "runtime-settings":
			f.Success = "Resource limits saved. Deploy or restart to apply them."
		case "environment-settings":
			f.Success = "Environment variables saved. Deploy or restart to apply them."
		case "connect-github":
			f.Success = "GitHub connected."
		case "create-service":
			f.Success = "SQLite service created."
		case "operation-configureMetadata":
			f.Success = "Project organization saved."
		case "source-settings":
			f.Success = "Repository settings saved."
		}
	}
	return p
}
