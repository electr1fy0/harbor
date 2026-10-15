package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"paas/internal/contracts"
	"paas/internal/source"
)

const (
	apiPrefix             = "/api/dashboard/"
	maxDashboardBody      = 1 << 20
	maxControlResponse    = 4 << 20
	defaultControlWait    = 15 * time.Second
	deploymentControlWait = 10 * time.Minute
	onboardingControlWait = 150 * time.Second
	unboundedControlWait  = -1
)

var (
	appNamePattern         = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	githubOwnerPattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	environmentKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	releaseIDPattern       = regexp.MustCompile(`^(?:[A-Za-z0-9_-]{1,128})?$`)
	jobIDPattern           = regexp.MustCompile(`^job_[a-f0-9]{32}$`)
	deploymentJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	memoryLimitPattern     = regexp.MustCompile(`^[1-9][0-9]*(?:Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E|B)?$`)
)

// NewAPI adapts the dashboard's stable function contract to the daemon's
// control-plane HTTP handler. Calls stay in process and each daemon response
// is held in a bounded recorder before it is projected for browser use.
func NewAPI(control http.Handler) http.Handler {
	return newAPI(control, newFailureExplainer(strings.TrimSpace(os.Getenv("OPENAI_API_KEY")), &http.Client{Timeout: failureExplanationTimeout}))
}

func newAPI(control http.Handler, failureExplainer *failureExplainer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPrefix) {
			writeAPIJSON(w, http.StatusNotFound, map[string]string{"error": "Dashboard endpoint was not found."})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, apiPrefix)
		if name == "" || strings.ContainsAny(name, "/\\") {
			writeAPIJSON(w, http.StatusNotFound, map[string]string{"error": "Dashboard endpoint was not found."})
			return
		}
		if r.URL.RawQuery != "" {
			writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request data."})
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed."})
			return
		}
		if !knownDashboardFunction(name) {
			writeAPIJSON(w, http.StatusNotFound, map[string]string{"error": "Dashboard endpoint was not found."})
			return
		}
		body, fields, err := readDashboardInput(w, r)
		if err != nil {
			status, message := http.StatusBadRequest, "Invalid request data."
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status, message = http.StatusRequestEntityTooLarge, "Dashboard request is too large."
			}
			writeAPIJSON(w, status, map[string]string{"error": message})
			return
		}
		var result any
		var apiErr *apiFailure
		if name == "explainFailure" {
			result, apiErr = safeExplainFailure(r.Context(), control, body, failureExplainer)
		} else {
			result, apiErr = safeDispatchDashboard(r.Context(), control, name, body, fields)
		}
		if apiErr != nil {
			writeAPIJSON(w, apiErr.status, map[string]string{"error": apiErr.message})
			return
		}
		writeAPIJSON(w, http.StatusOK, result)
	})
}

func safeDispatchDashboard(ctx context.Context, control http.Handler, name string, body []byte, fields map[string]json.RawMessage) (result any, fail *apiFailure) {
	defer func() {
		if recover() != nil {
			result = nil
			fail = operationFailure("Harbor could not complete the request.")
		}
	}()
	return dispatchDashboard(ctx, control, name, body, fields)
}

func knownDashboardFunction(name string) bool {
	switch name {
	case "getOverview", "getApp", "getAppSettings", "configureApp", "getCapacity", "deployApp",
		"operations", "automation", "dataServices", "observe",
		"retryGitHubDeployment", "getDeploymentJob", "stopApp", "rollbackApp", "listGitHubRepos",
		"getGitHubRepo", "getGitHubConnection", "connectGitHub", "disconnectGitHub", "inspectProject",
		"submitOnboardingJob", "getOnboardingJob", "cancelOnboardingJob", "explainFailure":
		return true
	default:
		return false
	}
}

type apiFailure struct {
	status  int
	message string
}

func invalidRequest() *apiFailure { return &apiFailure{http.StatusBadRequest, "Invalid request data."} }
func notFoundFailure() *apiFailure {
	return &apiFailure{http.StatusNotFound, "Harbor resource was not found."}
}
func operationFailure(message string) *apiFailure {
	return &apiFailure{http.StatusBadGateway, message}
}
func unavailableFailure() *apiFailure {
	return &apiFailure{http.StatusServiceUnavailable, "Harbor daemon is unavailable."}
}

func readDashboardInput(w http.ResponseWriter, r *http.Request) ([]byte, map[string]json.RawMessage, error) {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			return nil, nil, errors.New("request must be JSON")
		}
	}
	limited := http.MaxBytesReader(w, r.Body, maxDashboardBody)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, err
	}
	if len(body) == 0 {
		return nil, nil, errors.New("missing request body")
	}
	if strictJSONInvalid(body) {
		return nil, nil, errors.New("request body contains duplicate keys or trailing data")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, nil, errors.New("request body must be an object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, errors.New("request body must contain one JSON object")
	}
	return body, fields, nil
}

func decodeDashboardInput[T any](body []byte) (T, bool) {
	var input T
	if !exactInputKeys[T](body) {
		return input, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, false
	}
	return input, true
}

func rejectNullOptional(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if raw, ok := fields[name]; ok && strings.TrimSpace(string(raw)) == "null" {
			return true
		}
	}
	return false
}

func rejectNullMapValues(fields map[string]json.RawMessage, name string) bool {
	raw, exists := fields[name]
	if !exists {
		return false
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return true
	}
	for _, value := range values {
		if strings.TrimSpace(string(value)) == "null" {
			return true
		}
	}
	return false
}

func validAppName(name string) bool { return appNamePattern.MatchString(name) }

func validGitHubRepoURL(value string) (owner, repo string, ok bool) {
	value = strings.TrimSpace(value)
	if len(value) > 512 || strings.Contains(value, "#") {
		return "", "", false
	}
	u, err := url.Parse(value)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" || u.User != nil || !strings.EqualFold(u.Hostname(), "github.com") || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(u.EscapedPath(), "%") {
		return "", "", false
	}
	path := strings.TrimPrefix(u.Path, "/")
	if !strings.HasPrefix(u.Path, "/") || strings.ContainsAny(path, "?#") || strings.Count(path, "/") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(path, "/", 2)
	owner, repo = parts[0], parts[1]
	if strings.HasSuffix(repo, ".git") {
		repo = strings.TrimSuffix(repo, ".git")
	}
	return owner, repo, githubOwnerPattern.MatchString(owner) && validGitHubRepoName(repo)
}

func validGitHubRepoName(name string) bool {
	return name != "." && name != ".." && githubRepoPattern.MatchString(name)
}

func validGitRef(ref string) bool {
	return len(ref) <= 255 && source.ValidateGitHubCheckoutRequest("https://github.com/owner/repo", ref, ".") == nil
}

func validRepositoryRoot(root string) bool {
	if root == "" || root == "." {
		return true
	}
	if len(root) > 256 || strings.HasPrefix(root, "/") || strings.Contains(root, "\\") || (len(root) >= 2 && ((root[0] >= 'A' && root[0] <= 'Z') || (root[0] >= 'a' && root[0] <= 'z')) && root[1] == ':') {
		return false
	}
	for _, r := range root {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(root, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return source.ValidateGitHubCheckoutRequest("https://github.com/owner/repo", "", root) == nil
}

func validateEnvironment(environment map[string]string) bool {
	for key, value := range environment {
		if !environmentKeyPattern.MatchString(key) || len(value) > 65_536 {
			return false
		}
	}
	return true
}

type configInput struct {
	Runtime      *string  `json:"runtime,omitempty"`
	BuildCommand *string  `json:"buildCommand,omitempty"`
	StartCommand *string  `json:"startCommand,omitempty"`
	HealthPath   *string  `json:"healthPath,omitempty"`
	MemoryLimit  *string  `json:"memoryLimit,omitempty"`
	CPUQuota     *float64 `json:"cpuQuota,omitempty"`
	TasksLimit   *int     `json:"tasksLimit,omitempty"`
}

func validateConfigInput(c configInput, fields map[string]json.RawMessage, prefix string) bool {
	for _, field := range []string{"runtime", "buildCommand", "startCommand", "healthPath", "memoryLimit", "cpuQuota", "tasksLimit"} {
		if raw, ok := fields[prefix+field]; ok && strings.TrimSpace(string(raw)) == "null" {
			return false
		}
	}
	if c.Runtime != nil && *c.Runtime != "go" && *c.Runtime != "node" {
		return false
	}
	if c.BuildCommand != nil && len(*c.BuildCommand) > 4096 || c.StartCommand != nil && len(*c.StartCommand) > 4096 || c.HealthPath != nil && len(*c.HealthPath) > 256 || c.MemoryLimit != nil && len(*c.MemoryLimit) > 32 {
		return false
	}
	if c.CPUQuota != nil && (*c.CPUQuota <= 0 || *c.CPUQuota > 256 || *c.CPUQuota != *c.CPUQuota) {
		return false
	}
	if c.TasksLimit != nil && (*c.TasksLimit <= 0 || *c.TasksLimit > 65_536) {
		return false
	}
	return true
}

type configureInput struct {
	Name              string             `json:"name"`
	Environment       *map[string]string `json:"environment,omitempty"`
	RemoveEnvironment *[]string          `json:"removeEnvironment,omitempty"`
	MemoryLimit       *string            `json:"memoryLimit,omitempty"`
	CPUQuota          *float64           `json:"cpuQuota,omitempty"`
	TasksLimit        *int               `json:"tasksLimit,omitempty"`
}

func validateConfigure(input configureInput, fields map[string]json.RawMessage) bool {
	if !validAppName(input.Name) || rejectNullOptional(fields, "environment", "removeEnvironment", "memoryLimit", "cpuQuota", "tasksLimit") {
		return false
	}
	if rejectNullMapValues(fields, "environment") || input.Environment != nil && !validateEnvironment(*input.Environment) {
		return false
	}
	if input.RemoveEnvironment != nil {
		if len(*input.RemoveEnvironment) > 100 {
			return false
		}
		for _, key := range *input.RemoveEnvironment {
			if !environmentKeyPattern.MatchString(key) {
				return false
			}
		}
	}
	if input.MemoryLimit != nil {
		*input.MemoryLimit = strings.TrimSpace(*input.MemoryLimit)
		if !memoryLimitPattern.MatchString(*input.MemoryLimit) {
			return false
		}
	}
	if input.CPUQuota != nil && (*input.CPUQuota <= 0 || *input.CPUQuota > 256 || *input.CPUQuota != *input.CPUQuota) {
		return false
	}
	if input.TasksLimit != nil && (*input.TasksLimit <= 0 || *input.TasksLimit > 65_536) {
		return false
	}
	return input.Environment != nil || input.RemoveEnvironment != nil || input.MemoryLimit != nil || input.CPUQuota != nil || input.TasksLimit != nil
}

type nameInput struct {
	Name string `json:"name"`
}
type optionalNameInput struct {
	Name *string `json:"name,omitempty"`
}
type idInput struct {
	ID string `json:"id"`
}
type rollbackInput struct {
	Name      string `json:"name"`
	ReleaseID string `json:"releaseID"`
}
type deployInput struct {
	Name    string  `json:"name"`
	RepoURL string  `json:"repoURL"`
	Ref     *string `json:"ref,omitempty"`
	RootDir *string `json:"rootDir,omitempty"`
}
type ownerInput struct {
	Owner *string `json:"owner,omitempty"`
}
type repoInput struct {
	RepoURL string `json:"repoURL"`
}
type tokenInput struct {
	Token string `json:"token"`
}
type inspectInput struct {
	Name    *string      `json:"name,omitempty"`
	RepoURL string       `json:"repoURL"`
	Ref     *string      `json:"ref,omitempty"`
	RootDir *string      `json:"rootDir,omitempty"`
	Config  *configInput `json:"config,omitempty"`
}
type submitInput struct {
	Name          string             `json:"name"`
	RepoURL       string             `json:"repoURL"`
	Ref           *string            `json:"ref,omitempty"`
	RootDir       *string            `json:"rootDir,omitempty"`
	Config        *configInput       `json:"config,omitempty"`
	Environment   *map[string]string `json:"environment,omitempty"`
	SQLiteService *string            `json:"sqliteService,omitempty"`
	CreateSQLite  *bool              `json:"createSqlite,omitempty"`
}

func validateRefRoot(ref, root *string, fields map[string]json.RawMessage) bool {
	if rejectNullOptional(fields, "ref", "rootDir") {
		return false
	}
	if ref != nil && !validGitRef(*ref) || root != nil && !validRepositoryRoot(*root) {
		return false
	}
	return true
}

func validateOptionalConfig(c *configInput, fields map[string]json.RawMessage) bool {
	if rejectNullOptional(fields, "config") {
		return false
	}
	if c == nil {
		return true
	}
	configFields := make(map[string]json.RawMessage)
	for _, field := range []string{"runtime", "buildCommand", "startCommand", "healthPath", "memoryLimit", "cpuQuota", "tasksLimit"} {
		if raw, ok := fields["config"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) != nil {
				return false
			}
			if nested != nil {
				if value, ok := nested[field]; ok {
					configFields[field] = value
				}
			}
		}
	}
	return validateConfigInput(*c, configFields, "")
}

func dispatchDashboard(ctx context.Context, control http.Handler, name string, body []byte, fields map[string]json.RawMessage) (any, *apiFailure) {
	if endpoint, ok := map[string]string{"operations": "/operations", "automation": "/automation", "dataServices": "/data-services", "observe": "/observe"}[name]; ok {
		var action string
		if json.Unmarshal(fields["action"], &action) != nil || action == "" {
			return nil, invalidRequest()
		}
		if name == "operations" && action == "delete" {
			input, ok := decodeDashboardInput[struct {
				Action       string `json:"action"`
				AppName      string `json:"appName"`
				PreserveData *bool  `json:"preserveData"`
			}](body)
			if !ok || !validAppName(input.AppName) || input.PreserveData == nil {
				return nil, invalidRequest()
			}
			forgotten, fail := invokeControlJSON(ctx, control, http.MethodPost, "/automation", map[string]any{"action": "forgetSource", "name": input.AppName}, defaultControlWait)
			if fail != nil {
				return nil, fail
			}
			if forgotten.status < 200 || forgotten.status >= 300 {
				return nil, operationFailure("Deployment jobs must finish canceling before this project can be deleted.")
			}
			observed, fail := invokeControlJSON(ctx, control, http.MethodPost, "/observe", map[string]any{"action": "forget", "name": input.AppName}, defaultControlWait)
			if fail != nil {
				return nil, fail
			}
			if observed.status < 200 || observed.status >= 300 {
				return nil, operationFailure("Routing and notification settings must be cleared before this project can be deleted.")
			}
		}
		// Each feature handler owns a strict request schema and a public response
		// projection. The gateway retains the dashboard's auth and size limits.
		limit := maxControlResponse
		if name == "dataServices" && action == "export" {
			limit = 70 << 20
		}
		timeout := deploymentControlWait
		if name == "operations" && action == "rollback" {
			timeout = unboundedControlWait
		}
		response, fail := invokeControlWithLimit(ctx, control, http.MethodPost, endpoint, body, timeout, limit)
		if fail != nil {
			return nil, fail
		}
		var payload map[string]any
		if json.Unmarshal(response.body, &payload) != nil || payload == nil {
			return nil, operationFailure("Harbor returned an invalid response.")
		}
		if response.status < 200 || response.status >= 300 {
			message, _ := payload["error"].(string)
			if message == "" {
				message = "Harbor could not complete the operation."
			}
			return nil, &apiFailure{response.status, message}
		}
		return payload, nil
	}
	switch name {
	case "getOverview":
		input, ok := decodeDashboardInput[struct{}](body)
		_ = input
		if !ok || len(fields) != 0 {
			return nil, invalidRequest()
		}
		return getOverview(ctx, control)
	case "getApp":
		input, ok := decodeDashboardInput[nameInput](body)
		if !ok || !validAppName(input.Name) {
			return nil, invalidRequest()
		}
		return getApp(ctx, control, input.Name)
	case "getAppSettings":
		input, ok := decodeDashboardInput[nameInput](body)
		if !ok || !validAppName(input.Name) {
			return nil, invalidRequest()
		}
		return getSettings(ctx, control, input.Name)
	case "configureApp":
		input, ok := decodeDashboardInput[configureInput](body)
		if !ok || !validateConfigure(input, fields) {
			return nil, invalidRequest()
		}
		payload := map[string]any{"Name": input.Name}
		if input.Environment != nil {
			payload["Environment"] = *input.Environment
		}
		if input.RemoveEnvironment != nil {
			payload["RemoveEnvironment"] = *input.RemoveEnvironment
		}
		if input.MemoryLimit != nil {
			payload["MemoryLimit"] = *input.MemoryLimit
		}
		if input.CPUQuota != nil {
			payload["CPUQuota"] = *input.CPUQuota
		}
		if input.TasksLimit != nil {
			payload["TasksLimit"] = *input.TasksLimit
		}
		response, fail := daemonPost(ctx, control, "/configure", payload, defaultControlWait, "Could not update app settings.", false)
		if fail != nil {
			return nil, fail
		}
		return mapSettings(response), nil
	case "getCapacity":
		input, ok := decodeDashboardInput[optionalNameInput](body)
		if !ok || rejectNullOptional(fields, "name") || (input.Name != nil && !validAppName(*input.Name)) {
			return nil, invalidRequest()
		}
		payload := map[string]any{}
		if input.Name != nil {
			payload["Name"] = *input.Name
		}
		response, fail := daemonPost(ctx, control, "/capacity", payload, defaultControlWait, "Could not load capacity metrics.", false)
		if fail != nil {
			return nil, fail
		}
		capacity := mapCapacity(response["Capacity"])
		if capacity == nil {
			return nil, operationFailure("Harbor returned invalid capacity data.")
		}
		return capacity, nil
	case "deployApp":
		input, ok := decodeDashboardInput[deployInput](body)
		input.RepoURL = strings.TrimSpace(input.RepoURL)
		owner, repo, validRepo := validGitHubRepoURL(input.RepoURL)
		_ = owner
		_ = repo
		if !ok || !validAppName(input.Name) || !validRepo || !validateRefRoot(input.Ref, input.RootDir, fields) {
			return nil, invalidRequest()
		}
		ref, root := "", ""
		if input.Ref != nil {
			ref = *input.Ref
		}
		if input.RootDir != nil {
			root = *input.RootDir
		}
		response, fail := daemonPost(ctx, control, "/deploy", map[string]any{"Name": input.Name, "RepoURL": input.RepoURL, "Ref": ref, "RootDir": root}, unboundedControlWait, "Could not deploy this app. Review the repository settings and app logs.", true)
		if fail != nil {
			return nil, fail
		}
		deployment := mapDeployment(response["Deployment"])
		if deployment == nil {
			return nil, operationFailure("Harbor did not return a deployment result.")
		}
		return map[string]any{"deployment": deployment, "url": getLocalRouteURL(ctx, control, input.Name, response["URL"], deployment)}, nil
	case "retryGitHubDeployment":
		input, ok := decodeDashboardInput[nameInput](body)
		if !ok || !validAppName(input.Name) {
			return nil, invalidRequest()
		}
		return onboardingJob(ctx, control, http.MethodPost, "/jobs/redeploy", map[string]any{"name": input.Name}, defaultControlWait)
	case "getDeploymentJob":
		input, ok := decodeDashboardInput[idInput](body)
		if !ok || !deploymentJobIDPattern.MatchString(input.ID) {
			return nil, invalidRequest()
		}
		return onboardingJob(ctx, control, http.MethodGet, "/jobs/"+url.PathEscape(input.ID), nil, defaultControlWait)
	case "stopApp":
		input, ok := decodeDashboardInput[nameInput](body)
		if !ok || !validAppName(input.Name) {
			return nil, invalidRequest()
		}
		response, fail := daemonPost(ctx, control, "/stop", map[string]any{"Name": input.Name}, defaultControlWait, "Could not stop this app.", false)
		if fail != nil {
			return nil, fail
		}
		deployment := mapDeployment(response["Deployment"])
		if deployment == nil {
			return nil, operationFailure("Harbor did not return a deployment result.")
		}
		return map[string]any{"deployment": deployment, "url": getLocalRouteURL(ctx, control, input.Name, response["URL"], deployment)}, nil
	case "rollbackApp":
		input, ok := decodeDashboardInput[rollbackInput](body)
		if !ok || fields["releaseID"] == nil || rejectNullOptional(fields, "releaseID") || !validAppName(input.Name) || !releaseIDPattern.MatchString(input.ReleaseID) {
			return nil, invalidRequest()
		}
		response, fail := daemonPost(ctx, control, "/rollback", map[string]any{"Name": input.Name, "ReleaseID": input.ReleaseID}, unboundedControlWait, "Could not roll back this app. Review the app logs.", true)
		if fail != nil {
			return nil, fail
		}
		deployment := mapDeployment(response["Deployment"])
		if deployment == nil {
			return nil, operationFailure("Harbor did not return a deployment result.")
		}
		return map[string]any{"deployment": deployment, "url": getLocalRouteURL(ctx, control, input.Name, response["URL"], deployment)}, nil
	case "listGitHubRepos":
		input, ok := decodeDashboardInput[ownerInput](body)
		if !ok || rejectNullOptional(fields, "owner") {
			return nil, invalidRequest()
		}
		owner := ""
		if input.Owner != nil {
			owner = strings.TrimSpace(*input.Owner)
			if owner != "" && !githubOwnerPattern.MatchString(owner) {
				return nil, invalidRequest()
			}
		}
		return listGitHubRepos(ctx, control, owner)
	case "getGitHubRepo":
		input, ok := decodeDashboardInput[repoInput](body)
		input.RepoURL = strings.TrimSpace(input.RepoURL)
		owner, repo, validRepo := validGitHubRepoURL(input.RepoURL)
		if !ok || !validRepo {
			return nil, invalidRequest()
		}
		return getGitHubRepo(ctx, control, owner, repo)
	case "getGitHubConnection":
		if len(fields) != 0 {
			return nil, invalidRequest()
		}
		return getGitHubConnection(ctx, control)
	case "connectGitHub":
		input, ok := decodeDashboardInput[tokenInput](body)
		if !ok || len(input.Token) < 10 || len(input.Token) > 4096 {
			return nil, invalidRequest()
		}
		response, fail := onboardingControl(ctx, control, http.MethodPost, "/github/connection", map[string]any{"token": input.Token}, defaultControlWait)
		if fail != nil {
			return nil, fail
		}
		return mapGitHubConnection(response["connection"]), nil
	case "disconnectGitHub":
		if len(fields) != 0 {
			return nil, invalidRequest()
		}
		response, fail := onboardingControl(ctx, control, http.MethodDelete, "/github/connection", nil, defaultControlWait)
		if fail != nil {
			return nil, fail
		}
		return mapGitHubConnection(response["connection"]), nil
	case "inspectProject":
		input, ok := decodeDashboardInput[inspectInput](body)
		input.RepoURL = strings.TrimSpace(input.RepoURL)
		if !ok || rejectNullOptional(fields, "name", "config") || !validOptionalName(input.Name) || !validGitHubRepoInput(input.RepoURL) || !validateRefRoot(input.Ref, input.RootDir, fields) || !validateOptionalConfig(input.Config, fields) {
			return nil, invalidRequest()
		}
		return inspectProject(ctx, control, input)
	case "submitOnboardingJob":
		input, ok := decodeDashboardInput[submitInput](body)
		input.RepoURL = strings.TrimSpace(input.RepoURL)
		if !ok || !validAppName(input.Name) || !validGitHubRepoInput(input.RepoURL) || !validateRefRoot(input.Ref, input.RootDir, fields) || rejectNullOptional(fields, "config", "environment", "sqliteService", "createSqlite") || !validateOptionalConfig(input.Config, fields) {
			return nil, invalidRequest()
		}
		if rejectNullMapValues(fields, "environment") || input.Environment != nil && !validateEnvironment(*input.Environment) {
			return nil, invalidRequest()
		}
		if input.SQLiteService != nil && !validAppName(*input.SQLiteService) {
			return nil, invalidRequest()
		}
		return submitOnboardingJob(ctx, control, input)
	case "getOnboardingJob", "cancelOnboardingJob":
		input, ok := decodeDashboardInput[idInput](body)
		if !ok || !jobIDPattern.MatchString(input.ID) {
			return nil, invalidRequest()
		}
		method := http.MethodGet
		if name == "cancelOnboardingJob" {
			method = http.MethodDelete
		}
		return onboardingJob(ctx, control, method, "/jobs/"+url.PathEscape(input.ID), nil, defaultControlWait)
	default:
		return nil, &apiFailure{http.StatusNotFound, "Dashboard endpoint was not found."}
	}
}

func validGitHubRepoInput(value string) bool { _, _, ok := validGitHubRepoURL(value); return ok }
func validOptionalName(name *string) bool    { return name == nil || validAppName(*name) }

type boundedResponseWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int
	tooLarge bool
}

func newBoundedResponseWriter(limit int) *boundedResponseWriter {
	return &boundedResponseWriter{header: make(http.Header), limit: limit}
}
func (w *boundedResponseWriter) Header() http.Header { return w.header }
func (w *boundedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *boundedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	remaining := w.limit - w.body.Len()
	if remaining <= 0 {
		w.tooLarge = true
		return 0, errors.New("control response exceeds limit")
	}
	if len(p) > remaining {
		_, _ = w.body.Write(p[:remaining])
		w.tooLarge = true
		return remaining, errors.New("control response exceeds limit")
	}
	return w.body.Write(p)
}

type controlResponse struct {
	status  int
	body    []byte
	payload map[string]any
}

func invokeControl(ctx context.Context, control http.Handler, method, target string, body []byte, timeout time.Duration) (response controlResponse, fail *apiFailure) {
	return invokeControlWithLimit(ctx, control, method, target, body, timeout, maxControlResponse)
}

func invokeControlWithLimit(ctx context.Context, control http.Handler, method, target string, body []byte, timeout time.Duration, limit int) (response controlResponse, fail *apiFailure) {
	if control == nil {
		return response, unavailableFailure()
	}
	if len(body) > contracts.MaxControlRequestBytes {
		return response, &apiFailure{http.StatusRequestEntityTooLarge, "Control request is too large."}
	}
	if timeout == 0 {
		timeout = defaultControlWait
	}
	var callCtx context.Context
	var cancel context.CancelFunc
	if timeout < 0 {
		callCtx, cancel = context.WithCancel(ctx)
	} else {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, method, "http://paasd"+target, bytes.NewReader(body))
	if err != nil {
		return response, unavailableFailure()
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := newBoundedResponseWriter(limit)
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		control.ServeHTTP(recorder, request)
	}()
	if panicked {
		return response, operationFailure("Harbor could not complete the request.")
	}
	if recorder.tooLarge {
		return response, operationFailure("Harbor returned too much data.")
	}
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	return controlResponse{status: status, body: recorder.body.Bytes()}, nil
}

func invokeControlJSON(ctx context.Context, control http.Handler, method, target string, input any, timeout time.Duration) (controlResponse, *apiFailure) {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return controlResponse{}, operationFailure("Harbor could not complete the request.")
		}
	}
	response, fail := invokeControl(ctx, control, method, target, body, timeout)
	if fail != nil {
		return response, fail
	}
	if response.status == http.StatusNotFound {
		return response, notFoundFailure()
	}
	if len(bytes.TrimSpace(response.body)) == 0 {
		return response, operationFailure("Harbor returned an invalid response.")
	}
	if err := json.Unmarshal(response.body, &response.payload); err != nil || response.payload == nil {
		return response, operationFailure("Harbor returned an invalid response.")
	}
	return response, nil
}

func daemonPost(ctx context.Context, control http.Handler, endpoint string, input any, timeout time.Duration, message string, allowDeploymentError bool) (map[string]any, *apiFailure) {
	response, fail := invokeControlJSON(ctx, control, http.MethodPost, endpoint, input, timeout)
	if fail != nil {
		return nil, fail
	}
	hasError := response.status < 200 || response.status >= 300
	if messageValue, ok := response.payload["Error"].(string); ok && messageValue != "" {
		hasError = true
	}
	_, hasDeployment := object(response.payload["Deployment"])
	if hasError && !(allowDeploymentError && hasDeployment) {
		return nil, operationFailure(message)
	}
	return response.payload, nil
}

func onboardingControl(ctx context.Context, control http.Handler, method, endpoint string, input any, timeout time.Duration) (map[string]any, *apiFailure) {
	response, fail := invokeControlJSON(ctx, control, method, endpoint, input, timeout)
	if fail != nil {
		return nil, fail
	}
	if response.status < 200 || response.status >= 300 {
		message, _ := response.payload["error"].(string)
		message = sanitizeLogs(message)
		if message == "" {
			message = "Harbor could not complete onboarding."
		}
		status := http.StatusBadGateway
		// Preserve actionable client errors, including queue backpressure. An
		// internal adapter error must not turn "retry later" into a gateway fault.
		if response.status >= 400 && response.status < 500 || response.status == http.StatusServiceUnavailable {
			status = response.status
		}
		return nil, &apiFailure{status, safeString(message, 512)}
	}
	return response.payload, nil
}

func onboardingJob(ctx context.Context, control http.Handler, method, endpoint string, input any, timeout time.Duration) (any, *apiFailure) {
	response, fail := onboardingControl(ctx, control, method, endpoint, input, timeout)
	if fail != nil {
		return nil, fail
	}
	job := mapDeploymentJob(response["job"])
	if job == nil {
		return nil, operationFailure("Harbor returned invalid deployment job data.")
	}
	return job, nil
}

func getOverview(ctx context.Context, control http.Handler) (any, *apiFailure) {
	ping, fail := invokeControl(ctx, control, http.MethodGet, "/ping", nil, defaultControlWait)
	if fail != nil || ping.status != http.StatusOK || strings.TrimSpace(string(ping.body)) != "pong" {
		return map[string]any{"connected": false, "apps": []any{}, "services": []any{}, "error": "Harbor daemon is unavailable."}, nil
	}
	appsResponse, appsFail := daemonPost(ctx, control, "/apps", map[string]any{}, defaultControlWait, "Could not load applications.", false)
	servicesResponse, servicesFail := daemonPost(ctx, control, "/services", map[string]any{}, defaultControlWait, "Could not load services.", false)
	apps := []any{}
	services := []any{}
	var errorText string
	if appsFail == nil {
		if values, ok := arrayOf(appsResponse["Apps"]); ok {
			for _, value := range values {
				if app, ok := mapAppView(value); ok {
					apps = append(apps, app)
				} else {
					errorText = "Could not load applications."
					apps = []any{}
					break
				}
			}
		} else {
			errorText = "Could not load applications."
		}
	} else {
		errorText = "Could not load applications."
	}
	if servicesFail == nil {
		if values, ok := arrayOf(servicesResponse["Services"]); ok {
			for _, value := range values {
				if service, ok := mapService(value); ok {
					services = append(services, service)
				} else {
					if errorText == "" {
						errorText = "Service inventory is unavailable."
					} else {
						errorText += " Service inventory is unavailable."
					}
					services = []any{}
					break
				}
			}
		} else {
			if errorText == "" {
				errorText = "Service inventory is unavailable."
			} else {
				errorText += " Service inventory is unavailable."
			}
		}
	} else {
		if errorText == "" {
			errorText = "Service inventory is unavailable."
		} else {
			errorText += " Service inventory is unavailable."
		}
	}
	result := map[string]any{"connected": true, "apps": apps, "services": services}
	if errorText != "" {
		result["error"] = errorText
	}
	return result, nil
}

func getSettings(ctx context.Context, control http.Handler, name string) (any, *apiFailure) {
	response, fail := daemonPost(ctx, control, "/settings", map[string]any{"Name": name}, defaultControlWait, "Could not load app settings.", false)
	if fail != nil {
		return nil, fail
	}
	return mapSettings(response), nil
}

func inspectProject(ctx context.Context, control http.Handler, input inspectInput) (any, *apiFailure) {
	request := map[string]any{"repoURL": input.RepoURL}
	if input.Name != nil {
		request["name"] = *input.Name
	}
	if input.Ref != nil {
		request["ref"] = *input.Ref
	}
	if input.RootDir != nil {
		request["rootDir"] = *input.RootDir
	}
	if input.Config != nil {
		request["config"] = configToMap(*input.Config)
	}
	response, fail := onboardingControl(ctx, control, http.MethodPost, "/projects/inspect", request, onboardingControlWait)
	if fail != nil {
		return nil, fail
	}
	inspection := mapProjectInspection(response["inspection"])
	if inspection == nil {
		return nil, operationFailure("Harbor returned invalid project inspection data.")
	}
	return inspection, nil
}

func submitOnboardingJob(ctx context.Context, control http.Handler, input submitInput) (any, *apiFailure) {
	request := map[string]any{"name": input.Name, "repoURL": input.RepoURL}
	if input.Ref != nil {
		request["ref"] = *input.Ref
	}
	if input.RootDir != nil {
		request["rootDir"] = *input.RootDir
	}
	if input.Config != nil {
		request["config"] = configToMap(*input.Config)
	}
	if input.Environment != nil {
		request["environment"] = *input.Environment
	}
	if input.SQLiteService != nil {
		request["sqliteService"] = *input.SQLiteService
	}
	if input.CreateSQLite != nil {
		request["createSqlite"] = *input.CreateSQLite
	}
	response, fail := onboardingControl(ctx, control, http.MethodPost, "/jobs/deploy", request, onboardingControlWait)
	if fail != nil {
		return nil, fail
	}
	job := mapDeploymentJob(response["job"])
	if job == nil {
		return nil, operationFailure("Harbor returned invalid deployment job data.")
	}
	return job, nil
}

func configToMap(c configInput) map[string]any {
	result := map[string]any{}
	if c.Runtime != nil {
		result["runtime"] = *c.Runtime
	}
	if c.BuildCommand != nil {
		result["buildCommand"] = *c.BuildCommand
	}
	if c.StartCommand != nil {
		result["startCommand"] = *c.StartCommand
	}
	if c.HealthPath != nil {
		result["healthPath"] = *c.HealthPath
	}
	if c.MemoryLimit != nil {
		result["memoryLimit"] = *c.MemoryLimit
	}
	if c.CPUQuota != nil {
		result["cpuQuota"] = *c.CPUQuota
	}
	if c.TasksLimit != nil {
		result["tasksLimit"] = *c.TasksLimit
	}
	return result
}

func listGitHubRepos(ctx context.Context, control http.Handler, owner string) (any, *apiFailure) {
	connection, fail := getGitHubConnectionControl(ctx, control)
	if fail != nil {
		// The prior server contract allowed public browsing when connection
		// metadata was temporarily unavailable.
		if owner == "" {
			return nil, operationFailure("Connect GitHub or enter an owner to browse public repositories.")
		}
	} else if !safeBoolean(connection["connected"]) && owner == "" {
		return nil, operationFailure("Connect GitHub or enter an owner to browse public repositories.")
	}
	endpoint := "/github/repos"
	if owner != "" {
		endpoint += "?owner=" + url.QueryEscape(owner)
	}
	response, fail := onboardingControl(ctx, control, http.MethodGet, endpoint, nil, defaultControlWait)
	if fail != nil {
		return nil, fail
	}
	values, ok := arrayOf(response["repositories"])
	if !ok {
		return nil, operationFailure("GitHub returned invalid repository data.")
	}
	repositories := make([]any, 0, len(values))
	for _, value := range values {
		repository, valid := mapGitHubRepo(value)
		if !valid {
			return nil, operationFailure("GitHub returned invalid repository data.")
		}
		if owner == "" || strings.EqualFold(strings.Split(repository["fullName"].(string), "/")[0], owner) {
			repositories = append(repositories, repository)
		}
	}
	return repositories, nil
}

func getGitHubConnectionControl(ctx context.Context, control http.Handler) (map[string]any, *apiFailure) {
	response, fail := onboardingControl(ctx, control, http.MethodGet, "/github/connection", nil, defaultControlWait)
	if fail != nil {
		return nil, fail
	}
	connection, ok := object(response["connection"])
	if !ok {
		return nil, operationFailure("Harbor returned invalid GitHub connection data.")
	}
	return connection, nil
}

func getGitHubConnection(ctx context.Context, control http.Handler) (any, *apiFailure) {
	connection, fail := getGitHubConnectionControl(ctx, control)
	if fail != nil {
		return nil, fail
	}
	return mapGitHubConnection(connection), nil
}

func getGitHubRepo(ctx context.Context, control http.Handler, owner, repo string) (any, *apiFailure) {
	endpoint := "/github/repo?owner=" + url.QueryEscape(owner) + "&repo=" + url.QueryEscape(repo)
	response, fail := onboardingControl(ctx, control, http.MethodGet, endpoint, nil, defaultControlWait)
	if fail != nil {
		return nil, fail
	}
	value := response["repository"]
	if value == nil {
		value = response
	}
	result, ok := mapGitHubRepo(value)
	if !ok {
		return nil, operationFailure("GitHub returned invalid repository data.")
	}
	return result, nil
}

func getApp(ctx context.Context, control http.Handler, name string) (any, *apiFailure) {
	inspect, inspectFail := daemonPost(ctx, control, "/inspect", map[string]any{"Name": name}, defaultControlWait, "Could not load app status.", false)
	releases, releasesFail := daemonPost(ctx, control, "/releases", map[string]any{"Name": name}, defaultControlWait, "Could not load app releases.", false)
	settingsResponse, settingsFail := daemonPost(ctx, control, "/settings", map[string]any{"Name": name}, defaultControlWait, "Could not load app settings.", false)
	diagnosticsResponse, diagnosticsFail := daemonPost(ctx, control, "/diagnostics", map[string]any{"Name": name}, defaultControlWait, "Could not load deployment diagnostics.", false)
	if inspectFail != nil && inspectFail.status == http.StatusNotFound &&
		releasesFail != nil && releasesFail.status == http.StatusNotFound &&
		settingsFail != nil && settingsFail.status == http.StatusNotFound &&
		diagnosticsFail != nil && diagnosticsFail.status == http.StatusNotFound {
		return nil, notFoundFailure()
	}
	for _, fail := range []*apiFailure{inspectFail, releasesFail, settingsFail, diagnosticsFail} {
		if fail != nil && fail.status != http.StatusNotFound {
			return nil, fail
		}
	}
	var inspectData, releaseData map[string]any
	var diagnostics any
	var settings any = emptySettings()
	if inspectFail == nil {
		inspectData = inspect
	}
	if releasesFail == nil {
		releaseData = releases
	}
	if settingsFail == nil {
		settings = mapSettings(settingsResponse)
	}
	if diagnosticsFail == nil {
		diagnostics = diagnosticsResponse["diagnostics"]
		if diagnostics == nil {
			diagnostics = diagnosticsResponse["Diagnostics"]
		}
	}
	deployment := mapDeployment(nil)
	if inspectData != nil {
		deployment = mapDeployment(inspectData["Deployment"])
		if inspectData["Deployment"] != nil && deployment == nil {
			return nil, operationFailure("Could not load app status.")
		}
	}
	var mappedDiagnostics any
	if diagnostics != nil {
		mappedDiagnostics = mapDiagnostics(diagnostics)
		if mappedDiagnostics == nil {
			return nil, operationFailure("Could not load deployment diagnostics.")
		}
	}
	diagnosticsMap, _ := object(mappedDiagnostics)
	latestAttempt, _ := object(diagnosticsMap["latestAttempt"])
	logsPayload := map[string]any{"Name": name}
	if latestAttempt != nil && latestAttempt["ID"] != "" {
		logsPayload["DeploymentID"] = latestAttempt["ID"]
	}
	logs, logsFail := daemonPost(ctx, control, "/logs", logsPayload, defaultControlWait, "Could not load app logs.", false)
	if logsFail != nil && logsFail.status != http.StatusNotFound {
		return nil, logsFail
	}
	logText := ""
	if logsFail == nil {
		logText = sanitizeLogs(logs["Logs"])
	}
	releaseViews := []any{}
	if releaseData != nil {
		values, ok := arrayOf(releaseData["Releases"])
		if !ok {
			return nil, operationFailure("Could not load app releases.")
		}
		for _, value := range values {
			mapped, ok := mapRelease(value)
			if !ok {
				return nil, operationFailure("Could not load app releases.")
			}
			releaseViews = append(releaseViews, mapped)
		}
	}
	var routeDeployment map[string]any
	if diagnosticsMap != nil {
		routeDeployment, _ = object(diagnosticsMap["production"])
	} else if deployment != nil && deployment["State"] == "RUNNING" {
		routeDeployment = deployment
	}
	urlValue := ""
	if routeDeployment != nil && inspectData != nil {
		urlValue = routeURL(inspectData["URL"], routeDeployment)
	}
	return map[string]any{"deployment": deployment, "diagnostics": mappedDiagnostics, "releases": releaseViews, "logs": logText, "url": urlValue, "settings": settings}, nil
}

func getLocalRouteURL(ctx context.Context, control http.Handler, name string, value any, deployment map[string]any) string {
	if result := routeURL(value, deployment); result != "" {
		return result
	}
	if inspect, fail := daemonPost(ctx, control, "/inspect", map[string]any{"Name": name}, defaultControlWait, "Could not inspect the active app route.", false); fail == nil {
		return routeURL(inspect["URL"], mapDeployment(inspect["Deployment"]))
	}
	return ""
}

func writeAPIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
