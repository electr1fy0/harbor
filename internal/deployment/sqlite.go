package deployment

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"paas/internal/model"
	managedservice "paas/internal/service"
)

// sqliteBinding validates the durable database and prepares just its binding
// directory for a root-managed Linux runtime. The directory is mounted at the
// same absolute path so file: URIs work unchanged inside every sandbox.
func (s *Service) sqliteBinding(binding model.Binding, svc *model.Service) (string, string, error) {
	databasePath, err := managedservice.DatabasePath(&binding, svc)
	if err != nil {
		return "", "", err
	}
	databaseURL, err := managedservice.DatabaseURL(&binding, svc)
	if err != nil {
		return "", "", err
	}
	if filepath.Base(databasePath) != "database.sqlite" || filepath.Dir(filepath.Dir(databasePath)) != filepath.Clean(svc.VolumeDir) {
		return "", "", fmt.Errorf("database path is outside the service binding layout")
	}

	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		uid, gid, err := sqliteRuntimeIDs(s.opts.RuntimeUser)
		if err != nil {
			return "", "", err
		}
		if err := makeSQLiteServicePathTraversable(s.opts.DataDir, svc.VolumeDir); err != nil {
			return "", "", err
		}
		if err := assignSQLiteBinding(databasePath, uid, gid); err != nil {
			return "", "", err
		}
	}
	return databaseURL, filepath.Dir(databasePath), nil
}

func sqliteRuntimeIDs(name string) (int, int, error) {
	if name == "" {
		name = "paas-runtime"
	}
	target, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("look up SQLite runtime user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil || uid <= 0 {
		return 0, 0, fmt.Errorf("SQLite runtime user %q must have a valid non-root UID", name)
	}
	gid, err := strconv.Atoi(target.Gid)
	if err != nil || gid < 0 {
		return 0, 0, fmt.Errorf("SQLite runtime user %q has an invalid GID", name)
	}
	return uid, gid, nil
}

// makeSQLiteServicePathTraversable grants execute-only access to directories
// between the private Harbor data directory and the service volume. Directory
// contents remain unlistable, and files such as state.sqlite keep their own
// daemon-only permissions.
func makeSQLiteServicePathTraversable(dataDir, volumeDir string) error {
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolve Harbor data directory: %w", err)
	}
	volume, err := filepath.Abs(volumeDir)
	if err != nil {
		return fmt.Errorf("resolve SQLite service volume: %w", err)
	}
	relative, err := filepath.Rel(filepath.Join(root, "volumes"), volume)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("SQLite service volume must be inside Harbor's volumes directory")
	}

	directories := []string{root, filepath.Join(root, "volumes")}
	current := filepath.Join(root, "volumes")
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid SQLite service volume path")
		}
		current = filepath.Join(current, part)
		directories = append(directories, current)
	}
	for _, path := range directories {
		directory, err := openSQLiteNoFollow(path)
		if err != nil {
			return fmt.Errorf("inspect SQLite parent directory %q: %w", path, err)
		}
		info, statErr := directory.Stat()
		if statErr == nil && !info.IsDir() {
			statErr = fmt.Errorf("path is not a directory")
		}
		if statErr == nil {
			statErr = directory.Chmod(0711)
		}
		_ = directory.Close()
		if statErr != nil {
			return fmt.Errorf("make SQLite parent directory traversable: %w", statErr)
		}
	}
	return nil
}

func assignSQLiteBinding(databasePath string, uid, gid int) error {
	databaseDir := filepath.Dir(databasePath)
	paths := []struct {
		path string
		dir  bool
		mode os.FileMode
	}{
		{path: databaseDir, dir: true, mode: 0700},
		{path: databasePath, mode: 0600},
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		paths = append(paths, struct {
			path string
			dir  bool
			mode os.FileMode
		}{path: databasePath + suffix, mode: 0600})
	}
	for _, item := range paths {
		file, err := openSQLiteNoFollow(item.path)
		if os.IsNotExist(err) && !item.dir {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect SQLite runtime path %q: %w", item.path, err)
		}
		info, statErr := file.Stat()
		if statErr == nil {
			if item.dir && !info.IsDir() {
				statErr = fmt.Errorf("path is not a directory")
			} else if !item.dir && !info.Mode().IsRegular() {
				statErr = fmt.Errorf("path is not a regular file")
			} else if !item.dir {
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok || stat.Nlink != 1 {
					statErr = fmt.Errorf("file must not have hard links")
				}
			}
		}
		if statErr == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				statErr = fmt.Errorf("cannot inspect file ownership")
			} else if stat.Uid != uint32(uid) || stat.Gid != uint32(gid) {
				statErr = file.Chown(uid, gid)
			}
		}
		if statErr == nil {
			statErr = file.Chmod(item.mode)
		}
		_ = file.Close()
		if statErr != nil {
			return fmt.Errorf("assign SQLite runtime access to %q: %w", item.path, statErr)
		}
	}
	return nil
}

// openSQLiteNoFollow opens a file or directory without following a symbolic
// link. File descriptor operations keep permission changes attached to the
// checked inode rather than re-resolving a potentially replaced path.
func openSQLiteNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
