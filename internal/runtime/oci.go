// Package runtime manages container runtime filesystems and OCI image extraction.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
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
	if info, err := os.Lstat(policyPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("containers policy must be a regular file")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect containers policy: %w", err)
	}

	if err := os.MkdirAll(policyDir, 0755); err != nil {
		return fmt.Errorf("create containers policy dir: %w", err)
	}
	return os.WriteFile(policyPath, []byte(DefaultPolicyJSON()), 0644)
}

// FetchAndUnpack pulls an OCI image via Skopeo and unpacks it via umoci into targetDir.
// The resulting rootfs is located at filepath.Join(targetDir, "rootfs").
func FetchAndUnpack(ctx context.Context, imageRef string, targetDir string) error {
	if ctx == nil {
		return errors.New("OCI fetch context must not be nil")
	}
	if imageRef == "" {
		return errors.New("OCI image reference must not be empty")
	}
	if targetDir == "" {
		return errors.New("OCI target directory must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rootfs := filepath.Join(targetDir, "rootfs")
	if cached, err := cachedRootfs(targetDir, imageRef); err != nil {
		return err
	} else if cached {
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

	// 3. Ensure required mount points exist as directories. A rootfs can contain
	// symlinks at these paths; accepting one as a completed mount point makes
	// the host-side setup depend on where that symlink resolves on the host.
	if err := ensureRootfsDirectory(rootfs); err != nil {
		return fmt.Errorf("validate unpacked rootfs: %w", err)
	}
	for _, sub := range []string{"data", "app", "tmp"} {
		if err := ensureRootfsChildDirectory(rootfs, sub); err != nil {
			return fmt.Errorf("prepare rootfs mount point %q: %w", sub, err)
		}
	}

	// Save metadata only after unpacking and all required mount points have
	// been validated, so a successful cache hit always has complete state.
	metaPath := filepath.Join(targetDir, "runtime.json")
	meta, err := json.MarshalIndent(map[string]string{"image_ref": imageRef}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime metadata: %w", err)
	}
	meta = append(meta, '\n')
	metaTemp, err := os.CreateTemp(targetDir, ".runtime-*.tmp")
	if err != nil {
		return fmt.Errorf("create runtime metadata: %w", err)
	}
	metaTempPath := metaTemp.Name()
	defer os.Remove(metaTempPath)
	if err := metaTemp.Chmod(0600); err != nil {
		_ = metaTemp.Close()
		return fmt.Errorf("secure runtime metadata: %w", err)
	}
	if _, err := metaTemp.Write(meta); err != nil {
		_ = metaTemp.Close()
		return fmt.Errorf("write runtime metadata: %w", err)
	}
	if err := metaTemp.Sync(); err != nil {
		_ = metaTemp.Close()
		return fmt.Errorf("persist runtime metadata: %w", err)
	}
	if err := metaTemp.Close(); err != nil {
		return fmt.Errorf("close runtime metadata: %w", err)
	}
	if err := os.Rename(metaTempPath, metaPath); err != nil {
		return fmt.Errorf("publish runtime metadata: %w", err)
	}

	return nil
}

func cachedRootfs(targetDir, imageRef string) (bool, error) {
	rootfs := filepath.Join(targetDir, "rootfs")
	info, err := os.Lstat(rootfs)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect cached rootfs: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("cached rootfs must be a real directory")
	}
	metaPath := filepath.Join(targetDir, "runtime.json")
	metaInfo, err := os.Lstat(metaPath)
	if err != nil || metaInfo.Mode()&os.ModeSymlink != 0 || !metaInfo.Mode().IsRegular() {
		return false, errors.New("cached rootfs has no safe runtime metadata")
	}
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return false, fmt.Errorf("read cached runtime metadata: %w", err)
	}
	var metadata struct {
		ImageRef string `json:"image_ref"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.ImageRef == "" {
		return false, errors.New("cached runtime metadata is invalid")
	}
	if metadata.ImageRef != imageRef {
		return false, fmt.Errorf("target directory already contains image %q, not requested image %q", metadata.ImageRef, imageRef)
	}
	for _, sub := range []string{"data", "app", "tmp"} {
		if err := ensureRootfsChildDirectory(rootfs, sub); err != nil {
			return false, fmt.Errorf("validate cached rootfs mount point %q: %w", sub, err)
		}
	}
	return true, nil
}

func ensureRootfsDirectory(rootfs string) error {
	info, err := os.Lstat(rootfs)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("rootfs is not a real directory")
	}
	return nil
}

func ensureRootfsChildDirectory(rootfs, name string) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("invalid rootfs directory name")
	}
	path := filepath.Join(rootfs, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0777); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("mount point is not a real directory")
	}
	return nil
}
