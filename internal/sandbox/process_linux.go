//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"paas/internal/contracts"
)

const (
	prSetChildSubreaper = 36
	prGetChildSubreaper = 37
)

var subreaper struct {
	sync.Mutex
	users       int
	wasEnabled  bool
	enabledHere bool
}

func configureCommand(cmd *exec.Cmd, spec contracts.SandboxSpec) (func(), error) {
	// Pdeathsig terminates the managed leader if the daemon dies. Linux does
	// not propagate it to descendants; full crash-time tree supervision is deferred.
	attributes := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		name := spec.User
		if name == "" {
			name = "paas-runtime"
		}
		target, err := user.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("look up runtime user %q: %w", name, err)
		}
		uid, err := strconv.ParseUint(target.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse runtime user %q UID: %w", name, err)
		}
		if uid == 0 {
			return nil, fmt.Errorf("runtime user %q resolves to UID 0", name)
		}
		gid, err := strconv.ParseUint(target.Gid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse runtime user %q GID: %w", name, err)
		}
		groupNames, err := target.GroupIds()
		if err != nil {
			return nil, fmt.Errorf("look up groups for runtime user %q: %w", name, err)
		}
		groups := make([]uint32, 0, len(groupNames))
		for _, groupName := range groupNames {
			groupID, parseErr := strconv.ParseUint(groupName, 10, 32)
			if parseErr != nil {
				return nil, fmt.Errorf("parse group ID %q for runtime user %q: %w", groupName, name, parseErr)
			}
			groups = append(groups, uint32(groupID))
		}
		attributes.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}
	}
	cmd.SysProcAttr = attributes
	return acquireProcessTracking()
}

func acquireProcessTracking() (func(), error) {
	subreaper.Lock()
	if subreaper.users == 0 {
		wasEnabled, err := getChildSubreaper()
		if err != nil {
			subreaper.Unlock()
			return nil, fmt.Errorf("read child reaper setting: %w", err)
		}
		subreaper.wasEnabled = wasEnabled
		subreaper.enabledHere = !wasEnabled
		if !wasEnabled {
			if err := setChildSubreaper(true); err != nil {
				subreaper.Unlock()
				return nil, fmt.Errorf("enable descendant reaping: %w", err)
			}
		}
	}
	subreaper.users++
	subreaper.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			subreaper.Lock()
			subreaper.users--
			if subreaper.users == 0 && subreaper.enabledHere {
				_ = setChildSubreaper(false)
				subreaper.enabledHere = false
				subreaper.wasEnabled = false
			}
			subreaper.Unlock()
		})
	}, nil
}

func getChildSubreaper() (bool, error) {
	var value int32
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prGetChildSubreaper, uintptr(unsafe.Pointer(&value)), 0, 0, 0, 0)
	if errno != 0 {
		return false, errno
	}
	return value != 0, nil
}

func setChildSubreaper(enabled bool) error {
	value := uintptr(0)
	if enabled {
		value = 1
	}
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetChildSubreaper, value, 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	err := syscall.Kill(-pgid, signal)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

func finishProcessTree(pgid int) {
	if processGroupAlive(pgid) {
		_ = signalProcessGroup(pgid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		reapProcessGroupChildren(pgid)
		if !processGroupAlive(pgid) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = signalProcessGroup(pgid, syscall.SIGKILL)
	for processGroupAlive(pgid) {
		reapProcessGroupChildren(pgid)
		time.Sleep(20 * time.Millisecond)
	}
}

func reapProcessGroupChildren(pgid int) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
		if pid > 0 || err == syscall.EINTR {
			continue
		}
		return
	}
}

func exitStatus(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok {
		if status.Exited() {
			return status.ExitStatus()
		}
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
	}
	return state.ExitCode()
}
