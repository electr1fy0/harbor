// Package config loads and validates application configuration.
package config

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config describes how to build and start one application.
type Config struct {
	Name      string    `yaml:"name"`
	Runtime   string    `yaml:"runtime"`
	Build     Build     `yaml:"build"`
	Start     Start     `yaml:"start"`
	Resources Resources `yaml:"resources"`
	Health    Health    `yaml:"health"`
}

// Build contains the shell command used to produce the application artifact.
type Build struct {
	Command string `yaml:"command"`
}

// Start contains the command used to launch the application.
type Start struct {
	Command string `yaml:"command"`
}

// Resources contains the requested runtime resources.
type Resources struct {
	Memory string  `yaml:"memory"`
	CPU    float64 `yaml:"cpu"`
	Tasks  int     `yaml:"tasks"`
}

// Health configures an optional HTTP health endpoint. An empty Path indicates
// that callers should use a TCP readiness check.
type Health struct {
	Path string `yaml:"path"`
}

type configDocument struct {
	Name      string         `yaml:"name"`
	Runtime   string         `yaml:"runtime"`
	Build     Build          `yaml:"build"`
	Start     Start          `yaml:"start"`
	Resources *yamlResources `yaml:"resources"`
	Health    Health         `yaml:"health"`
}

type yamlResources struct {
	Memory *string  `yaml:"memory"`
	CPU    *float64 `yaml:"cpu"`
	Tasks  *int     `yaml:"tasks"`
}

// Load reads paas.yaml from sourceDir, rejects unknown YAML fields, applies
// resource defaults, and validates the resulting configuration.
func Load(sourceDir string) (*Config, error) {
	path := filepath.Join(sourceDir, "paas.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var document configDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple YAML documents are not allowed", path)
		}
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}

	cfg := &Config{
		Name:    document.Name,
		Runtime: document.Runtime,
		Build:   document.Build,
		Start:   document.Start,
		Health:  document.Health,
	}
	if document.Resources == nil || document.Resources.Memory == nil {
		cfg.Resources.Memory = "128Mi"
	} else {
		cfg.Resources.Memory = *document.Resources.Memory
	}
	if document.Resources == nil || document.Resources.CPU == nil {
		cfg.Resources.CPU = 0.5
	} else {
		cfg.Resources.CPU = *document.Resources.CPU
	}
	if document.Resources == nil || document.Resources.Tasks == nil {
		cfg.Resources.Tasks = 64
	} else {
		cfg.Resources.Tasks = *document.Resources.Tasks
	}

	if !ValidName(cfg.Name) {
		return nil, fmt.Errorf("invalid application name %q", cfg.Name)
	}
	if cfg.Runtime != "go" {
		return nil, fmt.Errorf("unsupported runtime %q: only go is supported", cfg.Runtime)
	}
	if strings.TrimSpace(cfg.Build.Command) == "" {
		return nil, fmt.Errorf("build.command must not be empty")
	}
	if strings.TrimSpace(cfg.Start.Command) == "" {
		return nil, fmt.Errorf("start.command must not be empty")
	}
	if !validMemory(cfg.Resources.Memory) {
		return nil, fmt.Errorf("resources.memory must be a positive byte quantity with a supported unit")
	}
	if cfg.Resources.CPU <= 0 || math.IsNaN(cfg.Resources.CPU) || math.IsInf(cfg.Resources.CPU, 0) {
		return nil, fmt.Errorf("resources.cpu must be a finite positive number")
	}
	if cfg.Resources.Tasks < 1 {
		return nil, fmt.Errorf("resources.tasks must be a positive integer")
	}
	if cfg.Health.Path != "" && !strings.HasPrefix(cfg.Health.Path, "/") {
		return nil, fmt.Errorf("health.path must be empty or start with /")
	}

	return cfg, nil
}

func validMemory(value string) bool {
	units := []string{"Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "K", "M", "G", "T", "P", "E", "B", ""}
	number := value
	for _, unit := range units {
		if strings.HasSuffix(value, unit) {
			number = strings.TrimSuffix(value, unit)
			break
		}
	}
	if number == "" {
		return false
	}
	for _, digit := range number {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	amount, err := strconv.ParseUint(number, 10, 64)
	return err == nil && amount > 0
}

// ValidName reports whether name is a lowercase application slug of at most
// 63 characters, beginning with a letter and containing only letters,
// numbers, and hyphens.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' || name[len(name)-1] == '-' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
