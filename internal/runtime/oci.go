// Package runtime manages container runtime filesystems and OCI image extraction.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// DefaultPolicyJSON returns a permissive trust policy for local OCI image operations.
func DefaultPolicyJSON() string {
	return `{
  "default": [
    {
      "type": "insecureAcceptAnything"
    }
  ]
}
`
}

// EnsurePolicyJSON ensures that ~/.config/containers/policy.json exists for Skopeo.
func EnsurePolicyJSON() error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return fmt.Errorf("cannot determine user home directory")
	}

	policyDir := filepath.Join(home, ".config", "containers")
	policyPath := filepath.Join(policyDir, "policy.json")
	if _, err := os.Stat(policyPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(policyDir, 0755); err != nil {
		return fmt.Errorf("create containers policy dir: %w", err)
	}
	return os.WriteFile(policyPath, []byte(DefaultPolicyJSON()), 0644)
}

// FetchAndUnpack pulls an OCI image via Skopeo and unpacks it via umoci into targetDir.
// The resulting rootfs is located at filepath.Join(targetDir, "rootfs").
func FetchAndUnpack(ctx context.Context, imageRef string, targetDir string) error {
	rootfs := filepath.Join(targetDir, "rootfs")
	if info, err := os.Stat(rootfs); err == nil && info.IsDir() {
		// Already unpacked
		return nil
	}

	if err := EnsurePolicyJSON(); err != nil {
		return fmt.Errorf("ensure policy.json: %w", err)
	}

	skopeoPath, err := exec.LookPath("skopeo")
	if err != nil {
		return fmt.Errorf("skopeo is not installed: %w", err)
	}
	umociPath, err := exec.LookPath("umoci")
	if err != nil {
		return fmt.Errorf("umoci is not installed: %w", err)
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	layoutDir, err := os.MkdirTemp(targetDir, ".oci-layout-")
	if err != nil {
		return fmt.Errorf("create temp layout dir: %w", err)
	}
	defer os.RemoveAll(layoutDir)

	// 1. Skopeo copy from remote to local OCI layout
	skopeoCmd := exec.CommandContext(ctx, skopeoPath, "copy", imageRef, "oci:"+layoutDir+":latest")
	skopeoCmd.Stdout = os.Stdout
	skopeoCmd.Stderr = os.Stderr
	if err := skopeoCmd.Run(); err != nil {
		return fmt.Errorf("skopeo copy %s: %w", imageRef, err)
	}

	// 2. umoci unpack --rootless into targetDir
	umociCmd := exec.CommandContext(ctx, umociPath, "unpack", "--rootless", "--image", layoutDir+":latest", targetDir)
	umociCmd.Stdout = os.Stdout
	umociCmd.Stderr = os.Stderr
	if err := umociCmd.Run(); err != nil {
		return fmt.Errorf("umoci unpack: %w", err)
	}

	// 3. Ensure required mount points exist in rootfs
	for _, sub := range []string{"data", "app", "tmp"} {
		_ = os.MkdirAll(filepath.Join(rootfs, sub), 0777)
	}

	// Save metadata
	metaPath := filepath.Join(targetDir, "runtime.json")
	meta := map[string]string{
		"image_ref": imageRef,
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(metaPath, b, 0644)
	}

	return nil
}
