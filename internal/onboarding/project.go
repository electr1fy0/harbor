package onboarding

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"paas/internal/config"

	"gopkg.in/yaml.v3"
)

const (
	maxPackageJSONBytes = 1 << 20
	maxPaasConfigBytes  = 256 << 10
	maxEnvSampleBytes   = 256 << 10
	maxEnvSampleKeys    = 100
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type ConfigSelection struct {
	Runtime      string   `json:"runtime,omitempty"`
	BuildCommand string   `json:"buildCommand,omitempty"`
	StartCommand string   `json:"startCommand,omitempty"`
	HealthPath   string   `json:"healthPath,omitempty"`
	MemoryLimit  string   `json:"memoryLimit,omitempty"`
	CPUQuota     *float64 `json:"cpuQuota,omitempty"`
	TasksLimit   *int     `json:"tasksLimit,omitempty"`
}

type ProjectConfig struct {
	Runtime      string  `json:"runtime"`
	BuildCommand string  `json:"buildCommand"`
	StartCommand string  `json:"startCommand"`
	HealthPath   string  `json:"healthPath"`
	MemoryLimit  string  `json:"memoryLimit"`
	CPUQuota     float64 `json:"cpuQuota"`
	TasksLimit   int     `json:"tasksLimit"`
}

type Inspection struct {
	ExistingConfig  bool          `json:"existingConfig"`
	Config          ProjectConfig `json:"config"`
	EnvironmentKeys []string      `json:"environmentKeys"`
	Warnings        []string      `json:"warnings,omitempty"`
}

type packageManifest struct {
	Scripts        map[string]string `json:"scripts"`
	PackageManager string            `json:"packageManager"`
}

// Inspect suggests supported Go/Node settings without writing to the checkout.
func Inspect(sourceDir, appName string, selection ConfigSelection) (Inspection, error) {
	if !config.ValidName(appName) {
		return Inspection{}, errors.New("invalid application name")
	}
	paasPath := filepath.Join(sourceDir, "paas.yaml")
	if info, err := os.Lstat(paasPath); err == nil {
		if !info.Mode().IsRegular() || info.Size() > maxPaasConfigBytes {
			return Inspection{}, errors.New("paas.yaml must be a regular file under 256 KiB")
		}
		file, err := os.Open(paasPath)
		if err != nil {
			return Inspection{}, fmt.Errorf("read repository paas.yaml: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxPaasConfigBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return Inspection{}, fmt.Errorf("read repository paas.yaml: %w", readErr)
		}
		if closeErr != nil {
			return Inspection{}, fmt.Errorf("read repository paas.yaml: %w", closeErr)
		}
		if len(data) > maxPaasConfigBytes {
			return Inspection{}, errors.New("paas.yaml must be a regular file under 256 KiB")
		}
		cfg, err := config.LoadBytes(data)
		if err != nil {
			return Inspection{}, fmt.Errorf("repository paas.yaml is invalid: %w", err)
		}
		if err := requireAuthoritativeSelection(selection, cfg); err != nil {
			return Inspection{}, err
		}
		keys, warnings, err := readEnvironmentKeys(sourceDir)
		if err != nil {
			return Inspection{}, err
		}
		return Inspection{ExistingConfig: true, Config: configView(cfg), EnvironmentKeys: keys, Warnings: warnings}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Inspection{}, fmt.Errorf("inspect repository config: %w", err)
	}

	cfg, warnings, err := detectConfig(sourceDir, appName)
	if err != nil {
		return Inspection{}, err
	}
	cfg, err = applySelection(cfg, selection)
	if err != nil {
		return Inspection{}, err
	}
	if err := config.Validate(cfg); err != nil {
		return Inspection{}, fmt.Errorf("invalid suggested configuration: %w", err)
	}
	keys, envWarnings, err := readEnvironmentKeys(sourceDir)
	if err != nil {
		return Inspection{}, err
	}
	warnings = append(warnings, envWarnings...)
	return Inspection{Config: configView(cfg), EnvironmentKeys: keys, Warnings: warnings}, nil
}

// Prepare re-inspects source, validates the selected settings, and writes a
// generated paas.yaml only inside the caller's disposable checkout.
func Prepare(sourceDir, appName string, selection ConfigSelection) (*config.Config, error) {
	inspection, err := Inspect(sourceDir, appName, selection)
	if err != nil {
		return nil, err
	}
	if inspection.ExistingConfig {
		return config.Load(sourceDir)
	}
	cfg := &config.Config{
		Name: appName, Runtime: inspection.Config.Runtime,
		Build:     config.Build{Command: inspection.Config.BuildCommand},
		Start:     config.Start{Command: inspection.Config.StartCommand},
		Health:    config.Health{Path: inspection.Config.HealthPath},
		Resources: config.Resources{Memory: inspection.Config.MemoryLimit, CPU: inspection.Config.CPUQuota, Tasks: inspection.Config.TasksLimit},
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, errors.New("could not encode generated paas.yaml")
	}
	path := filepath.Join(sourceDir, "paas.yaml")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create generated paas.yaml: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, errors.New("could not write generated paas.yaml")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, errors.New("could not sync generated paas.yaml")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, errors.New("could not close generated paas.yaml")
	}
	return config.Load(sourceDir)
}

// PrepareConfigured prepares a disposable checkout using the persisted source
// selection. Repository paas.yaml still supplies defaults, while explicitly
// selected values override it for this deployment.
func PrepareConfigured(sourceDir, appName string, selection ConfigSelection) (*config.Config, error) {
	inspection, err := Inspect(sourceDir, appName, ConfigSelection{})
	if err != nil {
		return nil, err
	}
	if inspection.ExistingConfig {
		cfg, err := config.Load(sourceDir)
		if err != nil {
			return nil, err
		}
		cfg, err = applySelection(cfg, selection)
		if err != nil {
			return nil, err
		}
		if !selectionEmpty(selection) {
			if err := writePreparedConfig(sourceDir, cfg); err != nil {
				return nil, err
			}
		}
		return cfg, nil
	}
	return Prepare(sourceDir, appName, selection)
}

// writePreparedConfig persists the selected effective config into the
// disposable checkout so the deployment service's later config.Load sees the
// same settings PrepareConfigured validated and returned.
func writePreparedConfig(sourceDir string, cfg *config.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return errors.New("could not encode prepared paas.yaml")
	}
	path := filepath.Join(sourceDir, "paas.yaml")
	tmp, err := os.CreateTemp(sourceDir, ".paas.yaml-*")
	if err != nil {
		return fmt.Errorf("create prepared paas.yaml: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return errors.New("could not set permissions on prepared paas.yaml")
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return errors.New("could not write prepared paas.yaml")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errors.New("could not sync prepared paas.yaml")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("could not close prepared paas.yaml")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace prepared paas.yaml: %w", err)
	}
	return nil
}

func configFromSelection(name string, selection ConfigSelection) (*config.Config, error) {
	if selection.Runtime == "" || selection.BuildCommand == "" || selection.StartCommand == "" || selection.MemoryLimit == "" || selection.CPUQuota == nil || selection.TasksLimit == nil {
		return nil, errors.New("inspect the project and review its settings before deploying")
	}
	cfg := &config.Config{
		Name: name, Runtime: selection.Runtime,
		Build: config.Build{Command: selection.BuildCommand}, Start: config.Start{Command: selection.StartCommand},
		Health:    config.Health{Path: selection.HealthPath},
		Resources: config.Resources{Memory: selection.MemoryLimit, CPU: *selection.CPUQuota, Tasks: *selection.TasksLimit},
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validateSourceSelection(name string, selection ConfigSelection) error {
	if selectionEmpty(selection) {
		return nil
	}
	runtime := selection.Runtime
	if runtime == "" {
		runtime = "go"
	}
	cfg := &config.Config{Name: name, Runtime: runtime, Resources: config.Resources{Memory: config.DefaultMemory, CPU: config.DefaultCPU, Tasks: config.DefaultTasks}}
	switch runtime {
	case "go":
		cfg.Build.Command = `go build -o "$PAAS_OUT/server" .`
		cfg.Start.Command = "/app/server"
	case "node":
		cfg.Build.Command = "npm install"
		cfg.Start.Command = "node server.js"
	default:
		return errors.New("runtime must be go or node")
	}
	_, err := applySelection(cfg, selection)
	return err
}

func detectConfig(sourceDir, appName string) (*config.Config, []string, error) {
	goModule, err := regularFileExists(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		return nil, nil, err
	}
	rootMain, err := regularFileExists(filepath.Join(sourceDir, "main.go"))
	if err != nil {
		return nil, nil, err
	}
	cmdMain, err := regularFileExists(filepath.Join(sourceDir, "cmd", "server", "main.go"))
	if err != nil {
		return nil, nil, err
	}
	packagePath := filepath.Join(sourceDir, "package.json")
	packageInfo, packageErr := os.Lstat(packagePath)
	if packageErr != nil && !errors.Is(packageErr, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect package.json: %w", packageErr)
	}
	nodeProject := packageErr == nil
	if nodeProject && (!packageInfo.Mode().IsRegular() || packageInfo.Size() > maxPackageJSONBytes) {
		return nil, nil, errors.New("package.json must be a regular file under 1 MiB")
	}
	if goModule && nodeProject {
		return nil, nil, errors.New("repository has both go.mod and package.json; add paas.yaml to choose a runtime")
	}
	if goModule {
		if !rootMain && !cmdMain {
			return nil, nil, errors.New("Go project needs main.go or cmd/server/main.go")
		}
		packageName := "."
		if !rootMain && cmdMain {
			packageName = "./cmd/server"
		}
		return &config.Config{
			Name: appName, Runtime: "go",
			Build:     config.Build{Command: `go build -o "$PAAS_OUT/server" ` + packageName},
			Start:     config.Start{Command: "/app/server"},
			Resources: config.Resources{Memory: config.DefaultMemory, CPU: config.DefaultCPU, Tasks: config.DefaultTasks},
		}, nil, nil
	}
	if nodeProject {
		var manifest packageManifest
		file, err := os.Open(packagePath)
		if err != nil {
			return nil, nil, fmt.Errorf("read package.json: %w", err)
		}
		manifest, err = decodePackageManifest(io.LimitReader(file, maxPackageJSONBytes+1))
		_ = file.Close()
		if err != nil {
			return nil, nil, errors.New("package.json is invalid")
		}
		if strings.TrimSpace(manifest.Scripts["start"]) == "" {
			return nil, nil, errors.New("Node project needs a package.json start script")
		}
		manager, install, err := nodePackageManager(sourceDir, manifest.PackageManager)
		if err != nil {
			return nil, nil, err
		}
		build := install
		if strings.TrimSpace(manifest.Scripts["build"]) != "" {
			build += " && " + manager + " run build"
		}
		return &config.Config{
			Name: appName, Runtime: "node",
			Build:     config.Build{Command: build},
			Start:     config.Start{Command: manager + " run start"},
			Resources: config.Resources{Memory: config.DefaultMemory, CPU: config.DefaultCPU, Tasks: config.DefaultTasks},
		}, nil, nil
	}
	return nil, nil, errors.New("unsupported project: add paas.yaml or provide a Go or Node project")
}

func decodePackageManifest(reader io.Reader) (packageManifest, error) {
	var manifest packageManifest
	data, err := io.ReadAll(io.LimitReader(reader, maxPackageJSONBytes+1))
	if err != nil {
		return manifest, err
	}
	if len(data) > maxPackageJSONBytes {
		return manifest, errors.New("package.json exceeds size limit")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func nodePackageManager(sourceDir, declared string) (manager, install string, err error) {
	// packageManager is the repository's explicit toolchain selection. A lock
	// from another package manager must not silently override it: running that
	// tool can ignore the declared runtime and install a different dependency
	// graph (or fail only after deployment has already been queued).
	if declared != "" {
		manager, version, found := strings.Cut(declared, "@")
		manager = strings.ToLower(manager)
		if !found || version == "" {
			return "", "", errors.New("package.json has an unsupported packageManager value")
		}
		switch manager {
		case "npm", "pnpm", "yarn", "bun":
		default:
			return "", "", errors.New("package.json has an unsupported packageManager value")
		}

		locked := false
		switch manager {
		case "npm":
			locked, err = regularFileExists(filepath.Join(sourceDir, "package-lock.json"))
		case "pnpm":
			locked, err = regularFileExists(filepath.Join(sourceDir, "pnpm-lock.yaml"))
		case "yarn":
			locked, err = regularFileExists(filepath.Join(sourceDir, "yarn.lock"))
		case "bun":
			locked, err = regularFileExists(filepath.Join(sourceDir, "bun.lock"))
			if err == nil && !locked {
				locked, err = regularFileExists(filepath.Join(sourceDir, "bun.lockb"))
			}
		}
		if err != nil {
			return "", "", err
		}
		switch manager {
		case "npm":
			if locked {
				return manager, "npm ci", nil
			}
			return manager, "npm install", nil
		case "pnpm":
			if locked {
				return manager, "pnpm install --frozen-lockfile", nil
			}
			return manager, "pnpm install", nil
		case "yarn":
			if !locked {
				return manager, "yarn install", nil
			}
			if strings.HasPrefix(strings.ToLower(version), "1.") {
				return manager, "yarn install --frozen-lockfile", nil
			}
			classic, checkErr := classicYarnLock(sourceDir)
			if checkErr != nil {
				return "", "", checkErr
			}
			if classic {
				return manager, "yarn install --frozen-lockfile", nil
			}
			return manager, "yarn install --immutable", nil
		case "bun":
			if locked {
				return manager, "bun install --frozen-lockfile", nil
			}
			return manager, "bun install", nil
		}
	}

	locks := []struct{ file, manager, install string }{
		{"bun.lock", "bun", "bun install --frozen-lockfile"},
		{"bun.lockb", "bun", "bun install --frozen-lockfile"},
		{"pnpm-lock.yaml", "pnpm", "pnpm install --frozen-lockfile"},
		{"yarn.lock", "yarn", "yarn install --immutable"},
		{"package-lock.json", "npm", "npm ci"},
	}
	for _, lock := range locks {
		if exists, checkErr := regularFileExists(filepath.Join(sourceDir, lock.file)); checkErr != nil {
			return "", "", checkErr
		} else if exists {
			if lock.manager == "yarn" {
				classic, checkErr := classicYarnLock(sourceDir)
				if checkErr != nil {
					return "", "", checkErr
				}
				if classic || strings.HasPrefix(strings.ToLower(declared), "yarn@1.") {
					return "yarn", "yarn install --frozen-lockfile", nil
				}
			}
			return lock.manager, lock.install, nil
		}
	}
	return "npm", "npm install", nil
}

func classicYarnLock(sourceDir string) (bool, error) {
	file, err := os.Open(filepath.Join(sourceDir, "yarn.lock"))
	if err != nil {
		return false, fmt.Errorf("inspect yarn.lock: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return false, errors.New("could not inspect yarn.lock")
	}
	return strings.Contains(string(data), "# yarn lockfile v1"), nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect project files: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("project file %q must be a regular file", filepath.Base(path))
	}
	return true, nil
}

func applySelection(cfg *config.Config, selection ConfigSelection) (*config.Config, error) {
	if selection.Runtime != "" {
		cfg.Runtime = selection.Runtime
	}
	if selection.BuildCommand != "" {
		cfg.Build.Command = selection.BuildCommand
	}
	if selection.StartCommand != "" {
		cfg.Start.Command = selection.StartCommand
	}
	if selection.HealthPath != "" {
		cfg.Health.Path = selection.HealthPath
	}
	if selection.MemoryLimit != "" {
		cfg.Resources.Memory = selection.MemoryLimit
	}
	if selection.CPUQuota != nil {
		cfg.Resources.CPU = *selection.CPUQuota
	}
	if selection.TasksLimit != nil {
		cfg.Resources.Tasks = *selection.TasksLimit
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func requireAuthoritativeSelection(selection ConfigSelection, existing *config.Config) error {
	requested := ProjectConfig{
		Runtime: selection.Runtime, BuildCommand: selection.BuildCommand, StartCommand: selection.StartCommand,
		HealthPath: selection.HealthPath, MemoryLimit: selection.MemoryLimit,
	}
	if selection.CPUQuota != nil {
		requested.CPUQuota = *selection.CPUQuota
	}
	if selection.TasksLimit != nil {
		requested.TasksLimit = *selection.TasksLimit
	}
	actual := configView(existing)
	if requested.Runtime != "" && requested.Runtime != actual.Runtime ||
		requested.BuildCommand != "" && requested.BuildCommand != actual.BuildCommand ||
		requested.StartCommand != "" && requested.StartCommand != actual.StartCommand ||
		requested.HealthPath != "" && requested.HealthPath != actual.HealthPath ||
		requested.MemoryLimit != "" && requested.MemoryLimit != actual.MemoryLimit ||
		selection.CPUQuota != nil && requested.CPUQuota != actual.CPUQuota ||
		selection.TasksLimit != nil && requested.TasksLimit != actual.TasksLimit {
		return errors.New("repository paas.yaml is authoritative; remove it to use generated settings")
	}
	return nil
}

func configView(cfg *config.Config) ProjectConfig {
	return ProjectConfig{Runtime: cfg.Runtime, BuildCommand: cfg.Build.Command, StartCommand: cfg.Start.Command, HealthPath: cfg.Health.Path,
		MemoryLimit: cfg.Resources.Memory, CPUQuota: cfg.Resources.CPU, TasksLimit: cfg.Resources.Tasks}
}

func readEnvironmentKeys(sourceDir string) ([]string, []string, error) {
	keys := make(map[string]struct{})
	warnings := []string{}
	truncated := false
	for _, name := range []string{".env.example", ".env.sample"} {
		path := filepath.Join(sourceDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxEnvSampleBytes {
			warnings = append(warnings, name+" is too large or is not a regular file; environment names were skipped")
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", name, err)
		}
		limited := &io.LimitedReader{R: file, N: maxEnvSampleBytes + 1}
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 1024), maxEnvSampleBytes+1)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "export ") {
				line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, _, _ := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if !envKeyPattern.MatchString(key) {
				continue
			}
			if reservedEnvironmentKey(key) {
				if key == "DATABASE_URL" {
					warnings = appendUnique(warnings, "DATABASE_URL is supplied by a managed SQLite service. Choose a service under Database before deploying.")
					continue
				}
				warnings = appendUnique(warnings, "Reserved environment names were omitted.")
				continue
			}
			if _, exists := keys[key]; exists {
				continue
			}
			if len(keys) >= maxEnvSampleKeys {
				truncated = true
				break
			}
			keys[key] = struct{}{}
		}
		scanErr := scanner.Err()
		_ = file.Close()
		if scanErr != nil && !errors.Is(scanErr, bufio.ErrTooLong) {
			return nil, nil, fmt.Errorf("read %s: %w", name, scanErr)
		}
		if errors.Is(scanErr, bufio.ErrTooLong) || limited.N == 0 {
			warnings = appendUnique(warnings, name+" exceeded its read limit; remaining environment names were skipped")
		}
	}
	if truncated {
		warnings = appendUnique(warnings, "Only the first 100 environment names were included; add remaining values in app settings.")
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sortStrings(result)
	return result, warnings, nil
}

func reservedEnvironmentKey(key string) bool {
	return key == "PORT" || key == "PATH" || key == "HOME" || key == "DATABASE_URL" || strings.HasPrefix(key, "PAAS_")
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
