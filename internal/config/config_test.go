package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndTCPHealth(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `name: demo
runtime: go
build:
  command: go build -o "$PAAS_OUT/server" .
start:
  command: /app/server
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Memory != "128Mi" || cfg.Resources.CPU != 0.5 || cfg.Resources.Tasks != 64 {
		t.Fatalf("unexpected resource defaults: %+v", cfg.Resources)
	}
	if cfg.Health.Path != "" {
		t.Fatalf("empty health path should select TCP readiness, got %q", cfg.Health.Path)
	}
}

func TestLoadPreservesExplicitValues(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `name: demo-2
runtime: go
build:
  command: go build -o "$PAAS_OUT/server" .
start:
  command: /app/server
resources:
  memory: 256Mi
  cpu: 1.25
  tasks: 90
health:
  path: /ready
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "demo-2" || cfg.Runtime != "go" || cfg.Resources.Memory != "256Mi" || cfg.Resources.CPU != 1.25 || cfg.Resources.Tasks != 90 || cfg.Health.Path != "/ready" {
		t.Fatalf("configuration values were not preserved: %+v", cfg)
	}
}

func TestLoadRejectsUnknownFieldsAndInvalidValues(t *testing.T) {
	base := `name: demo
runtime: go
build:
  command: build
start:
  command: /app/server
`
	cases := map[string]string{
		"unknown top-level field": base + "port: 8080\n",
		"unknown nested field":    strings.Replace(base, "  command: build", "  command: build\n  extra: true", 1),
		"unsupported runtime":     strings.Replace(base, "runtime: go", "runtime: node", 1),
		"invalid name":            strings.Replace(base, "name: demo", "name: Demo", 1),
		"trailing name hyphen":    strings.Replace(base, "name: demo", "name: demo-", 1),
		"empty build":             strings.Replace(base, "  command: build", "  command: '  '", 1),
		"empty start":             strings.Replace(base, "  command: /app/server", "  command: ''", 1),
		"bad health path":         base + "health:\n  path: ready\n",
		"invalid memory":          base + "resources:\n  memory: huge\n",
		"zero CPU":                base + "resources:\n  cpu: 0\n",
		"zero tasks":              base + "resources:\n  tasks: 0\n",
		"multiple documents":      base + "---\nname: other\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, contents)
			if _, err := Load(dir); err == nil {
				t.Fatal("Load accepted invalid configuration")
			}
		})
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"a", "hello", "a-1", "a" + strings.Repeat("x", 62)}
	for _, name := range valid {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false", name)
		}
	}
	invalid := []string{"", "1app", "-app", "App", "a_b", "a.b", "é", "a" + strings.Repeat("x", 63)}
	for _, name := range invalid {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true", name)
		}
	}
}

func TestExampleConfigurationsLoad(t *testing.T) {
	for _, example := range []string{"hello-go", "broken-build", "memory-hog", "unhealthy"} {
		t.Run(example, func(t *testing.T) {
			cfg, err := Load(filepath.Join("..", "..", "examples", example))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Runtime != "go" || cfg.Build.Command == "" || cfg.Start.Command != "/app/server" {
				t.Fatalf("unexpected application config: %+v", cfg)
			}
		})
	}
}

func writeConfig(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "paas.yaml"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
