package onboarding

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"paas/internal/config"
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

func TestDeclaredNodePackageManagerWinsOverOtherLockfiles(t *testing.T) {
	for _, tc := range []struct {
		name, declared, matchingLock, otherLock, wantManager, wantInstall string
	}{
		{"npm ignores pnpm lock", "npm@10.5.0", "", "pnpm-lock.yaml", "npm", "npm install"},
		{"npm uses its lock", "npm@10.5.0", "package-lock.json", "pnpm-lock.yaml", "npm", "npm ci"},
		{"pnpm ignores npm lock", "pnpm@9.1.0", "", "package-lock.json", "pnpm", "pnpm install"},
		{"pnpm uses its lock", "pnpm@9.1.0", "pnpm-lock.yaml", "package-lock.json", "pnpm", "pnpm install --frozen-lockfile"},
		{"classic yarn without lock", "yarn@1.22.22", "", "package-lock.json", "yarn", "yarn install"},
		{"modern yarn without lock", "yarn@4.3.1", "", "package-lock.json", "yarn", "yarn install"},
		{"modern yarn with lock", "yarn@4.3.1", "yarn.lock", "package-lock.json", "yarn", "yarn install --immutable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.matchingLock != "" {
				writeProjectFile(t, dir, tc.matchingLock, "lock\n")
			}
			if tc.otherLock != "" {
				writeProjectFile(t, dir, tc.otherLock, "lock\n")
			}
			manager, install, err := nodePackageManager(dir, tc.declared)
			if err != nil {
				t.Fatal(err)
			}
			if manager != tc.wantManager || install != tc.wantInstall {
				t.Fatalf("manager=%q install=%q; want %q, %q", manager, install, tc.wantManager, tc.wantInstall)
			}
		})
	}
}

func TestUnsupportedDeclaredNodePackageManagerIsNotHiddenByLockfile(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "package-lock.json", "{}")
	if _, _, err := nodePackageManager(dir, "unsupported@1.0.0"); err == nil {
		t.Fatal("unsupported packageManager was ignored because package-lock.json exists")
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

func TestPrepareConfiguredAppliesPersistedSelectionOverRepositoryConfig(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "paas.yaml", `name: repo-app
runtime: go
build:
  command: go build -o "$PAAS_OUT/server" .
start:
  command: /app/server
health:
  path: /
resources:
  memory: 128Mi
  cpu: 0.5
  tasks: 64
`)
	selection := ConfigSelection{BuildCommand: `go build -o "$PAAS_OUT/server" ./cmd/server`, HealthPath: "/ready"}
	cfg, err := PrepareConfigured(dir, "repo-app", selection)
	if err != nil {
		t.Fatalf("PrepareConfigured(): %v", err)
	}
	if cfg.Build.Command != selection.BuildCommand || cfg.Health.Path != selection.HealthPath || cfg.Start.Command != "/app/server" || cfg.Resources.Memory != "128Mi" || cfg.Resources.CPU != 0.5 || cfg.Resources.Tasks != 64 {
		t.Fatalf("persisted selection did not override repository config: %+v", cfg)
	}
	deployedCfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load after PrepareConfigured(): %v", err)
	}
	if deployedCfg.Build.Command != selection.BuildCommand || deployedCfg.Health.Path != selection.HealthPath {
		t.Fatalf("deployment reload lost persisted selection: %+v", deployedCfg)
	}
	if _, err := Inspect(dir, "repo-app", ConfigSelection{}); err != nil {
		t.Fatalf("prepared checkout config became invalid: %v", err)
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

func TestInspectRejectsTrailingPackageJSONData(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "package.json", `{"scripts":{"start":"node server.js"}} {"scripts":{"start":"node attacker.js"}}`)
	if _, err := Inspect(dir, "node-app", ConfigSelection{}); err == nil || !strings.Contains(err.Error(), "package.json is invalid") {
		t.Fatalf("package.json with trailing JSON was accepted: %v", err)
	}
}

func TestPackageManifestDecoderRejectsDataPastSizeLimit(t *testing.T) {
	input := `{"scripts":{"start":"node server.js"}}` + strings.Repeat(" ", maxPackageJSONBytes) + "junk"
	if _, err := decodePackageManifest(strings.NewReader(input)); err == nil {
		t.Fatal("package.json content beyond the size limit was silently ignored")
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

func TestEnvironmentSampleAcceptsSingleLineAtSizeLimit(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "go.mod", "module example\n")
	writeProjectFile(t, dir, "main.go", "package main\n")
	sample := "API_TOKEN=" + strings.Repeat("x", maxEnvSampleBytes-len("API_TOKEN="))
	writeProjectFile(t, dir, ".env.example", sample)

	got, err := Inspect(dir, "sample-app", ConfigSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.EnvironmentKeys, "API_TOKEN") {
		t.Fatalf("environment sample dropped its valid key: %+v", got)
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
