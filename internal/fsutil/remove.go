// Package fsutil contains filesystem helpers for already-validated managed paths.
package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveReadonlyTree makes directories owner-writable before removing a managed
// tree. WalkDir does not follow symlinks; linked targets are never chmodded.
// Callers must validate ownership and containment before calling this helper.
func RemoveReadonlyTree(root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0700 != 0700 {
				return os.Chmod(path, info.Mode().Perm()|0700)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}
