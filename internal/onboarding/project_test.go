package onboarding

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInspectGoAndExtractsOnlyAllowedEnvironmentNames(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "go.mod", "module example\n")
	writeProjectFile(t, dir, "main.go", "package main\nfunc main() {}\n")
	writeProjectFile(t, dir, ".env.example", "# sample\nAPI_TOKEN=never-return-this\nexport DATABASE_URL=ignored\nPORT=8080\nEMPTY=\n")
	inspection, err := Inspect(dir, "sample-app", ConfigSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.ExistingConfig || inspection.Config.Runtime != "go" || inspection.Config.StartCommand != "/app/server" {
		t.Fatalf("inspection=%+v", inspection)
	}
	if len(inspection.EnvironmentKeys) != 2 || inspection.EnvironmentKeys[0] != "API_TOKEN" || inspection.EnvironmentKeys[1] != "EMPTY" {
		t.Fatalf("keys=%v", inspection.EnvironmentKeys)
	}
	if _, err := os.Stat(filepath.Join(dir, "paas.yaml")); !os.IsNotExist(err) {
		t.Fatalf("inspection wrote config: %v", err)
	}
	if _, err := Prepare(dir, "sample-app", selectionFromView(inspection.Config)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "paas.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "never-return-this") || strings.Contains(string(data), "API_TOKEN") {
		t.Fatal("environment sample content leaked into generated config")
	}
}

func TestInspectNodeUsesLockfileAndEditableSelection(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "package.json", `{"scripts":{"build":"vite build","start":"node server.js"}}`)
	writeProjectFile(t, dir, "package-lock.json", "{}")
	cpu, tasks := 1.25, 48
	got, err := Inspect(dir, "node-app", ConfigSelection{MemoryLimit: "256Mi", CPUQuota: &cpu, TasksLimit: &tasks, HealthPath: "/health"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Runtime != "node" || got.Config.BuildCommand != "npm ci && npm run build" || got.Config.StartCommand != "npm run start" || got.Config.MemoryLimit != "256Mi" || got.Config.CPUQuota != cpu || got.Config.TasksLimit != tasks || got.Config.HealthPath != "/health" {
		t.Fatalf("config=%+v", got.Config)
	}
}

func TestYarnLockfileUsesClassicOrBerryInstallMode(t *testing.T) {
	for _, tc := range []struct{ name, header, declared, want string }{
		{"classic", "# yarn lockfile v1\n", "", "yarn install --frozen-lockfile"},
		{"berry", "__metadata:\n  version: 6\n", "", "yarn install --immutable"},
		{"declared classic", "", "yarn@1.22.22", "yarn install --frozen-lockfile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProjectFile(t, dir, "yarn.lock", tc.header)
			gotManager, gotInstall, err := nodePackageManager(dir, tc.declared)
			if err != nil {
				t.Fatal(err)
			}
			if gotManager != "yarn" || gotInstall != tc.want {
				t.Fatalf("manager=%q install=%q want=%q", gotManager, gotInstall, tc.want)
			}
		})
	}
}

func TestExistingPaasYAMLIsAuthoritativeAndInvalidSelectionRejected(t *testing.T) {
	dir := t.TempDir()
	configText := strings.Join([]string{
		"name: config-name", "runtime: go", "build:", "  command: go build -o \"$PAAS_OUT/server\" .", "start:", "  command: /app/server",
		"resources:", "  memory: 192Mi", "  cpu: 0.75", "  tasks: 40", "",
	}, "\n")
	writeProjectFile(t, dir, "paas.yaml", configText)
	got, err := Inspect(dir, "user-name", ConfigSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExistingConfig || got.Config.MemoryLimit != "192Mi" {
		t.Fatalf("inspection=%+v", got)
	}
	cpu := 2.0
	if _, err := Inspect(dir, "user-name", ConfigSelection{CPUQuota: &cpu}); err == nil || !strings.Contains(err.Error(), "authoritative") {
		t.Fatalf("mismatched selection err=%v", err)
	}
	if _, err := Inspect(t.TempDir(), "bad-app", ConfigSelection{Runtime: "python"}); err == nil {
		t.Fatal("unsupported runtime was accepted")
	}
}

func TestAuthoritativeConfigRetryReadsCurrentRepositoryFile(t *testing.T) {
	dir := t.TempDir()
	first := "name: repo-app\nruntime: go\nbuild:\n  command: go build -o \\\"$PAAS_OUT/server\\\" .\nstart:\n  command: /app/server\nresources:\n  memory: 128Mi\n  cpu: 0.5\n  tasks: 64\n"
	writeProjectFile(t, dir, "paas.yaml", first)
	initial, err := Inspect(dir, "repo-app", ConfigSelection{})
	if err != nil || !initial.ExistingConfig {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	// The persisted selection for a repository-authored file is intentionally
	// empty. A later commit's own values are authoritative on retry.
	retried := strings.Replace(first, "memory: 128Mi", "memory: 256Mi", 1)
	retried = strings.Replace(retried, "command: /app/server", "command: /app/new-server", 1)
	writeProjectFile(t, dir, "paas.yaml", retried)
	inspection, err := Inspect(dir, "repo-app", ConfigSelection{})
	if err != nil || inspection.Config.MemoryLimit != "256Mi" || inspection.Config.StartCommand != "/app/new-server" {
		t.Fatalf("retry did not read current config: inspection=%+v err=%v", inspection, err)
	}
	cfg, err := Prepare(dir, "repo-app", ConfigSelection{})
	if err != nil || cfg.Resources.Memory != "256Mi" || cfg.Start.Command != "/app/new-server" {
		t.Fatalf("prepared config=%+v err=%v", cfg, err)
	}
}

func TestUnsupportedNodeAndAmbiguousRuntimeFailClearly(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "package.json", `{"scripts":{"build":"vite build"}}`)
	if _, err := Inspect(dir, "node-app", ConfigSelection{}); err == nil || !strings.Contains(err.Error(), "start script") {
		t.Fatalf("missing start script err=%v", err)
	}
	writeProjectFile(t, dir, "go.mod", "module example\n")
	if _, err := Inspect(dir, "node-app", ConfigSelection{}); err == nil || !strings.Contains(err.Error(), "both go.mod and package.json") {
		t.Fatalf("ambiguous runtime err=%v", err)
	}
}

func TestEnvironmentSampleIsBounded(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "go.mod", "module example\n")
	writeProjectFile(t, dir, "main.go", "package main\n")
	if err := os.WriteFile(filepath.Join(dir, ".env.sample"), []byte(strings.Repeat("X", maxEnvSampleBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(dir, "sample-app", ConfigSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.EnvironmentKeys) != 0 || len(got.Warnings) == 0 {
		t.Fatalf("inspection=%+v", got)
	}
}

func TestEnvironmentSampleWarnsWhenKeyListIsTruncated(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "go.mod", "module example\n")
	writeProjectFile(t, dir, "main.go", "package main\n")
	var sample strings.Builder
	for i := 0; i < maxEnvSampleKeys+1; i++ {
		fmt.Fprintf(&sample, "KEY_%03d=value\n", i)
	}
	writeProjectFile(t, dir, ".env.example", sample.String())

	got, err := Inspect(dir, "sample-app", ConfigSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.EnvironmentKeys) != maxEnvSampleKeys {
		t.Fatalf("environment keys = %d, want %d", len(got.EnvironmentKeys), maxEnvSampleKeys)
	}
	if !slices.Contains(got.Warnings, "Only the first 100 environment names were included; add remaining values in app settings.") {
		t.Fatalf("missing truncation warning: %+v", got.Warnings)
	}
}

func TestOversizedAuthoritativeConfigIsRejectedBeforeRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "paas.yaml"), []byte(strings.Repeat("x", maxPaasConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(dir, "sample-app", ConfigSelection{}); err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Fatalf("oversized paas.yaml error = %v", err)
	}
}

func writeProjectFile(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
