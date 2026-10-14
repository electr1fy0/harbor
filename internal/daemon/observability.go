package daemon

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"paas/internal/capacity"
	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/deployment"
	"paas/internal/router"
)

type observeSettings struct {
	Name               string   `json:"name"`
	Maintenance        bool     `json:"maintenance"`
	MaintenanceMessage string   `json:"maintenanceMessage"`
	PrimaryDomain      string   `json:"primaryDomain"`
	MemoryPercent      float64  `json:"memoryPercent"`
	DiskPercent        float64  `json:"diskPercent"`
	CooldownSeconds    int      `json:"cooldownSeconds"`
	WebhookConfigured  bool     `json:"webhookConfigured"`
	Events             []string `json:"events"`
}
type storedObserveSettings struct {
	Settings observeSettings `json:"settings"`
	Webhook  []byte          `json:"webhook"`
}
type historicalPoint struct {
	At       time.Time         `json:"at"`
	Snapshot capacity.Snapshot `json:"snapshot"`
}
type Observatory struct {
	service    *deployment.Service
	collector  *capacity.Collector
	db         *sql.DB
	key        []byte
	mu         sync.Mutex
	sampleMu   sync.Mutex
	lastStates map[string]string
	lastNotice map[string]time.Time
	applied    map[string][]string
	client     *http.Client
}

func newObservatory(service *deployment.Service) (*Observatory, error) {
	if service == nil {
		return nil, errors.New("observability is unavailable")
	}
	dir := service.DataDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "observability.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, dbErr := os.Lstat(filepath.Join(dir, "observability.sqlite")); dbErr == nil {
			return nil, errors.New("observability key is missing for existing state")
		} else if !errors.Is(dbErr, os.ErrNotExist) {
			return nil, dbErr
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		err = os.WriteFile(keyPath, key, 0600)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("observability encryption key unavailable")
	}
	info, err := os.Lstat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("observability key must be a private regular file")
	}
	dbPath := filepath.Join(dir, "observability.sqlite")
	if info, err := os.Lstat(dbPath); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("observability database must be a regular file")
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite3", u.String()+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS observation_settings(name TEXT PRIMARY KEY,payload BLOB NOT NULL); CREATE TABLE IF NOT EXISTS observation_samples(id INTEGER PRIMARY KEY,at TEXT NOT NULL,payload BLOB NOT NULL); CREATE TABLE IF NOT EXISTS observation_states(name TEXT NOT NULL,deployment_id TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(name,deployment_id)); CREATE TABLE IF NOT EXISTS observation_notifications(id INTEGER PRIMARY KEY,at TEXT NOT NULL,name TEXT NOT NULL,event TEXT NOT NULL,success INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(dbPath, 0600); err != nil {
		db.Close()
		return nil, err
	}
	o := &Observatory{service: service, collector: capacity.New(dir), db: db, key: key, lastStates: map[string]string{}, lastNotice: map[string]time.Time{}, applied: map[string][]string{}, client: &http.Client{Transport: publicObserveTransport(), Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	// Restore traffic policies before the first dashboard request.
	if settings, err := o.allSettings(); err == nil {
		for _, s := range settings {
			o.applyTraffic(s.Settings)
		}
	}
	go o.run(service.LifetimeContext())
	return o, nil
}
func (o *Observatory) run(ctx context.Context) {
	defer o.db.Close()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			o.sample(ctx)
		}
	}
}
func (o *Observatory) settings(name string) (storedObserveSettings, error) {
	var data []byte
	err := o.db.QueryRow(`SELECT payload FROM observation_settings WHERE name=?`, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return storedObserveSettings{Settings: observeSettings{Name: name, CooldownSeconds: 300, Events: []string{"BUILD_FAILED", "START_FAILED", "UNHEALTHY", "CRASHED", "OOM_KILLED"}}}, nil
	}
	var result storedObserveSettings
	if err == nil {
		err = json.Unmarshal(data, &result)
	}
	return result, err
}
func (o *Observatory) allSettings() ([]storedObserveSettings, error) {
	rows, err := o.db.Query(`SELECT payload FROM observation_settings LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []storedObserveSettings{}
	for rows.Next() {
		var data []byte
		var item storedObserveSettings
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (o *Observatory) seal(value string) ([]byte, error) {
	block, err := aes.NewCipher(o.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(value), []byte("harbor-webhook")), nil
}
func (o *Observatory) unseal(value []byte) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(o.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	n := gcm.NonceSize()
	if len(value) < n {
		return "", errors.New("invalid webhook ciphertext")
	}
	plain, err := gcm.Open(nil, value[:n], value[n:], []byte("harbor-webhook"))
	return string(plain), err
}
func (o *Observatory) trafficRouter() *router.HostRouter {
	accessor, ok := any(o.service).(interface{ TrafficRouter() contracts.Router })
	if !ok {
		return nil
	}
	r, _ := accessor.TrafficRouter().(*router.HostRouter)
	return r
}
func (o *Observatory) applyTraffic(s observeSettings) {
	r := o.trafficRouter()
	if r == nil {
		return
	}
	settings, err := o.service.Settings(s.Name)
	if err != nil {
		return
	}
	if s.PrimaryDomain != "" && !containsObserve(settings.Domains, s.PrimaryDomain) {
		s.PrimaryDomain = ""
	}
	hosts := append([]string{s.Name + ".localhost"}, settings.Domains...)
	o.mu.Lock()
	old := o.applied[s.Name]
	o.applied[s.Name] = hosts
	o.mu.Unlock()
	r.UpdateTrafficPolicies(s.Name+".localhost", hosts, old, router.TrafficPolicy{Maintenance: s.Maintenance, Message: s.MaintenanceMessage, PrimaryDomain: s.PrimaryDomain})
}
func (o *Observatory) sample(ctx context.Context) {
	o.sampleMu.Lock()
	defer o.sampleMu.Unlock()
	sampleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	runtimes, err := o.service.CapacityRuntimes(sampleCtx, "")
	if err != nil {
		return
	}
	limits := make([]capacity.RuntimeLimit, 0, len(runtimes))
	for _, r := range runtimes {
		memory, _ := config.ParseMemoryBytes(r.MemoryLimit)
		limits = append(limits, capacity.RuntimeLimit{Identity: r.ID, Name: r.AppName, State: string(r.State), PID: r.PID, ControlGroup: r.ControlGroup, MemoryLimit: memory, CPUQuotaCores: r.CPUQuota, TasksLimit: r.TasksLimit})
	}
	snapshot, err := o.collector.Sample(sampleCtx, limits)
	if err != nil {
		return
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	// Bounded retention; 5760 samples represent 24 hours at 15 second intervals.
	if _, err = o.db.Exec(`INSERT INTO observation_samples(at,payload) VALUES(?,?)`, snapshot.SampledAt.Format(time.RFC3339Nano), data); err != nil {
		return
	}
	_, _ = o.db.Exec(`DELETE FROM observation_samples WHERE at < ? OR id NOT IN (SELECT id FROM observation_samples ORDER BY id DESC LIMIT 5760)`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339Nano))
	settings, err := o.allSettings()
	if err != nil {
		return
	}
	for _, s := range settings {
		o.applyTraffic(s.Settings)
		history, err := o.service.History(s.Settings.Name, 20, 0)
		if err != nil {
			continue
		}
		for _, entry := range history.Deployments {
			d := entry.Deployment
			state := string(d.State)
			var previous string
			readErr := o.db.QueryRow(`SELECT state FROM observation_states WHERE name=? AND deployment_id=?`, s.Settings.Name, d.ID).Scan(&previous)
			if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
				continue
			}
			if previous == state {
				continue
			}
			if _, err = o.db.Exec(`INSERT INTO observation_states(name,deployment_id,state) VALUES(?,?,?) ON CONFLICT(name,deployment_id) DO UPDATE SET state=excluded.state`, s.Settings.Name, d.ID, state); err != nil {
				continue
			}
			if containsObserve(s.Settings.Events, state) {
				o.notify(sampleCtx, s, state, "Application entered "+state)
			}
		}
		if s.Settings.DiskPercent > 0 && snapshot.Disk.FilesystemUsedBytes != nil && snapshot.Disk.FilesystemTotalBytes != nil && *snapshot.Disk.FilesystemTotalBytes > 0 && 100*float64(*snapshot.Disk.FilesystemUsedBytes)/float64(*snapshot.Disk.FilesystemTotalBytes) >= s.Settings.DiskPercent {
			o.notify(sampleCtx, s, "DISK_THRESHOLD", "Host disk usage exceeded the configured threshold.")
		}
		if s.Settings.MemoryPercent > 0 && snapshot.Host.MemoryUsedBytes != nil && snapshot.Host.MemoryTotalBytes != nil && *snapshot.Host.MemoryTotalBytes > 0 && 100*float64(*snapshot.Host.MemoryUsedBytes)/float64(*snapshot.Host.MemoryTotalBytes) >= s.Settings.MemoryPercent {
			o.notify(sampleCtx, s, "MEMORY_THRESHOLD", "Host memory usage exceeded the configured threshold.")
		}
	}
}
func containsObserve(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
func (o *Observatory) notify(ctx context.Context, s storedObserveSettings, event, message string) {
	if len(s.Webhook) == 0 {
		return
	}
	now := time.Now()
	key := s.Settings.Name + ":" + event
	var persisted string
	if err := o.db.QueryRow(`SELECT at FROM observation_notifications WHERE name=? AND event=? ORDER BY id DESC LIMIT 1`, s.Settings.Name, event).Scan(&persisted); err == nil {
		if at, err := time.Parse(time.RFC3339Nano, persisted); err == nil && now.Sub(at) < time.Duration(s.Settings.CooldownSeconds)*time.Second {
			return
		}
	}
	o.mu.Lock()
	if now.Sub(o.lastNotice[key]) < time.Duration(s.Settings.CooldownSeconds)*time.Second {
		o.mu.Unlock()
		return
	}
	o.lastNotice[key] = now
	o.mu.Unlock()
	target, err := o.unseal(s.Webhook)
	if err != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{"app": s.Settings.Name, "event": event, "message": message, "at": now.UTC()})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := o.client.Do(request)
	success := err == nil && response.StatusCode >= 200 && response.StatusCode < 300
	if response != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
	}
	_, _ = o.db.Exec(`INSERT INTO observation_notifications(at,name,event,success) VALUES(?,?,?,?)`, now.UTC().Format(time.RFC3339Nano), s.Settings.Name, event, success)
	_, _ = o.db.Exec(`DELETE FROM observation_notifications WHERE id NOT IN (SELECT id FROM observation_notifications ORDER BY id DESC LIMIT 500)`)
}

type observeRequest struct {
	Action       string           `json:"action"`
	Name         string           `json:"name"`
	DeploymentID string           `json:"deploymentId"`
	Source       string           `json:"source"`
	Cursor       string           `json:"cursor"`
	Limit        int              `json:"limit"`
	Window       string           `json:"window"`
	Settings     *observeSettings `json:"settings"`
	WebhookURL   *string          `json:"webhookURL"`
	Domain       string           `json:"domain"`
}

func registerObservabilityRoutes(mux *http.ServeMux, service *deployment.Service) {
	observatory, initErr := newObservatory(service)
	mux.HandleFunc("/observe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOnboardingError(w, 405, "Method not allowed.")
			return
		}
		if initErr != nil {
			writeOnboardingError(w, 503, "Observability storage is unavailable.")
			return
		}
		var request observeRequest
		if !decodeOnboardingRequest(w, r, &request) {
			return
		}
		if request.Name != "" && !config.ValidName(request.Name) {
			writeOnboardingError(w, 400, "Invalid app name.")
			return
		}
		output, err := observatory.dispatch(r.Context(), request)
		if err != nil {
			writeOnboardingError(w, 400, err.Error())
			return
		}
		writeOnboardingJSON(w, 200, output)
	})
}
func (o *Observatory) dispatch(ctx context.Context, request observeRequest) (any, error) {
	switch request.Action {
	case "get":
		if request.Name == "" {
			return nil, errors.New("app name is required")
		}
		if _, err := o.service.Settings(request.Name); err != nil {
			return nil, errors.New("application is unavailable")
		}
		s, err := o.settings(request.Name)
		if err != nil {
			return nil, errors.New("observability settings unavailable")
		}
		if request.Window != "" && request.Window != "hour" && request.Window != "day" {
			return nil, errors.New("invalid history window")
		}
		history, err := o.history(request.Name, request.Limit, request.Window)
		if err != nil {
			return nil, err
		}
		traffic := []router.TrafficStats{}
		if r := o.trafficRouter(); r != nil {
			settings, _ := o.service.Settings(request.Name)
			hosts := append([]string{request.Name + ".localhost"}, settings.Domains...)
			for _, stat := range r.Traffic() {
				if containsObserve(hosts, stat.Host) {
					traffic = append(traffic, stat)
				}
			}
		}
		notices := []map[string]any{}
		rows, err := o.db.Query(`SELECT at,event,success FROM observation_notifications WHERE name=? ORDER BY id DESC LIMIT 20`, request.Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var at, event string
			var success bool
			if err = rows.Scan(&at, &event, &success); err != nil {
				rows.Close()
				return nil, err
			}
			notices = append(notices, map[string]any{"at": at, "event": event, "success": success})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		return map[string]any{"settings": s.Settings, "history": history, "traffic": traffic, "notifications": notices}, nil
	case "configure":
		if request.Settings == nil || request.Settings.Name != request.Name {
			return nil, errors.New("settings must identify the application")
		}
		app, err := o.service.Settings(request.Name)
		if err != nil {
			return nil, errors.New("application unavailable")
		}
		s := *request.Settings
		if len(s.MaintenanceMessage) > 1024 || s.MemoryPercent < 0 || s.MemoryPercent > 100 || s.DiskPercent < 0 || s.DiskPercent > 100 || s.CooldownSeconds < 30 || s.CooldownSeconds > 86400 {
			return nil, errors.New("invalid maintenance, threshold, or cooldown settings")
		}
		if s.PrimaryDomain != "" && !containsObserve(app.Domains, s.PrimaryDomain) {
			return nil, errors.New("primary domain must be a configured custom domain")
		}
		if len(s.Events) > 10 {
			return nil, errors.New("too many notification events")
		}
		for _, event := range s.Events {
			if !containsObserve([]string{"BUILD_FAILED", "START_FAILED", "UNHEALTHY", "CRASHED", "OOM_KILLED", "RUNNING", "STOPPED"}, event) {
				return nil, errors.New("invalid notification event")
			}
		}
		stored, err := o.settings(request.Name)
		if err != nil {
			return nil, errors.New("settings unavailable")
		}
		if request.WebhookURL != nil {
			target := *request.WebhookURL
			if target == "" {
				stored.Webhook = nil
			} else {
				u, err := url.Parse(target)
				if err != nil || len(target) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
					return nil, errors.New("webhook must be an HTTPS URL without credentials or fragment")
				}
				stored.Webhook, err = o.seal(target)
				if err != nil {
					return nil, errors.New("webhook encryption failed")
				}
			}
		}
		s.WebhookConfigured = len(stored.Webhook) > 0
		stored.Settings = s
		data, _ := json.Marshal(stored)
		if _, err = o.db.Exec(`INSERT INTO observation_settings(name,payload) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET payload=excluded.payload`, request.Name, data); err != nil {
			return nil, errors.New("settings could not be saved")
		}
		o.applyTraffic(s)
		return map[string]any{"settings": s}, nil
	case "forget":
		if !config.ValidName(request.Name) {
			return nil, errors.New("valid app name required")
		}
		o.sampleMu.Lock()
		defer o.sampleMu.Unlock()
		settings, _ := o.service.Settings(request.Name)
		hosts := []string{request.Name + ".localhost"}
		if settings != nil {
			hosts = append(hosts, settings.Domains...)
		}
		tx, err := o.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, errors.New("observability cleanup unavailable")
		}
		defer tx.Rollback()
		for _, table := range []string{"observation_settings", "observation_states", "observation_notifications"} {
			if _, err = tx.Exec(`DELETE FROM `+table+` WHERE name=?`, request.Name); err != nil {
				return nil, errors.New("observability cleanup failed")
			}
		}
		if err = tx.Commit(); err != nil {
			return nil, errors.New("observability cleanup failed")
		}
		if r := o.trafficRouter(); r != nil {
			for _, host := range hosts {
				r.SetTrafficPolicy(host, router.TrafficPolicy{})
			}
		}
		o.mu.Lock()
		delete(o.applied, request.Name)
		for key := range o.lastNotice {
			if strings.HasPrefix(key, request.Name+":") {
				delete(o.lastNotice, key)
			}
		}
		o.mu.Unlock()
		return map[string]any{"forgotten": true}, nil
	case "logs":
		return o.logDelta(request)
	case "domains":
		return o.domainChecks(ctx, request.Name)
	case "bundle":
		settings, err := o.service.Settings(request.Name)
		if err != nil {
			return nil, errors.New("application unavailable")
		}
		diagnostics, err := o.service.Diagnostics(request.Name, false)
		if err != nil {
			return nil, errors.New("diagnostics unavailable")
		}
		logs, err := o.service.LogsForDeployment(request.Name, request.DeploymentID)
		if err != nil {
			return nil, errors.New("logs unavailable")
		}
		history, err := o.history(request.Name, 20)
		if err != nil {
			return nil, err
		}
		// Diagnostics only contains already-redacted summary/excerpt. Raw runtime paths/errors are omitted.
		return map[string]any{"app": request.Name, "settings": settings, "summary": diagnostics.Summary, "excerpt": diagnostics.Excerpt, "logs": logs, "capacityHistory": history, "createdAt": time.Now().UTC()}, nil
	default:
		return nil, errors.New("unknown observability action")
	}
}
func (o *Observatory) history(name string, limit int, windows ...string) ([]historicalPoint, error) {
	if limit <= 0 {
		limit = 120
	}
	if limit > 240 {
		limit = 240
	}
	window := time.Hour
	if len(windows) > 0 && windows[0] == "day" {
		window = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339Nano)
	var count int
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM observation_samples WHERE at>=?`, cutoff).Scan(&count); err != nil {
		return nil, errors.New("metric history unavailable")
	}
	stride := 1
	if count > limit {
		stride = (count + limit - 1) / limit
	}
	rows, err := o.db.Query(`SELECT payload FROM observation_samples WHERE at>=? AND (id % ? = 0 OR id=(SELECT MAX(id) FROM observation_samples)) ORDER BY id DESC LIMIT ?`, cutoff, stride, limit)
	if err != nil {
		return nil, errors.New("metric history unavailable")
	}
	defer rows.Close()
	result := []historicalPoint{}
	for rows.Next() {
		var data []byte
		var snapshot capacity.Snapshot
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &snapshot); err != nil {
			return nil, err
		}
		if name != "" {
			apps := []capacity.App{}
			for _, app := range snapshot.Apps {
				if app.Name == name {
					apps = append(apps, app)
				}
			}
			snapshot.Apps = apps
		}
		result = append(result, historicalPoint{At: snapshot.SampledAt, Snapshot: snapshot})
	}
	return result, rows.Err()
}

type logCursor struct {
	Length   int    `json:"length"`
	Digest   string `json:"digest"`
	Identity string `json:"identity"`
}

func (o *Observatory) logDelta(request observeRequest) (any, error) {
	if request.Source == "" {
		request.Source = "all"
	}
	if !containsObserve([]string{"all", "build", "runtime"}, request.Source) {
		return nil, errors.New("invalid log source")
	}
	var logs string
	var err error
	if accessor, ok := any(o.service).(interface {
		LogsBySource(string, string, string) (string, error)
	}); ok {
		logs, err = accessor.LogsBySource(request.Name, request.DeploymentID, request.Source)
	} else if request.Source == "all" {
		logs, err = o.service.LogsForDeployment(request.Name, request.DeploymentID)
	} else {
		return nil, errors.New("selected log source unavailable")
	}
	if err != nil {
		return nil, errors.New("selected logs unavailable")
	}
	identity := request.Name + ":" + request.DeploymentID + ":" + request.Source
	var cursor logCursor
	reset := true
	delta := logs
	if request.Cursor != "" && len(request.Cursor) <= 2048 {
		if data, e := base64.RawURLEncoding.DecodeString(request.Cursor); e == nil && json.Unmarshal(data, &cursor) == nil && cursor.Identity == identity && cursor.Length >= 0 && cursor.Length <= len(logs) {
			digest := sha256.Sum256([]byte(logs[:cursor.Length]))
			if hex.EncodeToString(digest[:]) == cursor.Digest {
				delta = logs[cursor.Length:]
				reset = false
			}
		}
	}
	digest := sha256.Sum256([]byte(logs))
	data, _ := json.Marshal(logCursor{Length: len(logs), Digest: hex.EncodeToString(digest[:]), Identity: identity})
	return map[string]any{"text": delta, "reset": reset, "cursor": base64.RawURLEncoding.EncodeToString(data), "source": request.Source}, nil
}
func (o *Observatory) domainChecks(ctx context.Context, name string) (any, error) {
	ctx, cancelAll := context.WithTimeout(ctx, 15*time.Second)
	defer cancelAll()
	settings, err := o.service.Settings(name)
	if err != nil {
		return nil, errors.New("application unavailable")
	}
	result := []map[string]any{}
	resolver := net.DefaultResolver
	// DNS targets are user configured; prohibit HTTPS probes to private addresses.
	client := &http.Client{Timeout: 5 * time.Second, Transport: publicObserveTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, domain := range settings.Domains {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, lookupErr := resolver.LookupIPAddr(probeCtx, domain)
		addresses := []string{}
		for _, ip := range ips {
			addresses = append(addresses, ip.IP.String())
		}
		item := map[string]any{"domain": domain, "addresses": addresses, "dnsResolved": lookupErr == nil && len(ips) > 0, "expected": "Point A/AAAA records to this Harbor server's public IP; for aliases use a CNAME to a hostname that resolves to it.", "httpsReachable": false}
		if lookupErr == nil {
			request, _ := http.NewRequestWithContext(probeCtx, http.MethodHead, "https://"+domain+"/", nil)
			response, probeErr := client.Do(request)
			if probeErr == nil {
				item["httpsReachable"] = true
				item["statusCode"] = response.StatusCode
				if response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
					item["certificateExpiresAt"] = response.TLS.PeerCertificates[0].NotAfter
				}
				response.Body.Close()
			} else {
				item["error"] = "HTTPS could not be verified from the Harbor host."
			}
		} else {
			item["error"] = "DNS resolution failed."
		}
		cancel()
		result = append(result, item)
	}
	return map[string]any{"domains": result}, nil
}
func publicObserveIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, prefix := range nonPublicObservePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var nonPublicObservePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/3"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicObserveTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("target has no public address")
		}
		for _, ip := range ips {
			if !publicObserveIP(ip.IP) {
				return nil, errors.New("target resolves to a private or reserved address")
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}}
}
