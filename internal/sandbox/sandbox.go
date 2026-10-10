package sandbox

import (
	"runtime"

	"paas/internal/contracts"
)

// NewDefault returns the best available Sandbox implementation for the host.
// On Linux with systemd and Bubblewrap, it returns a SystemdSandbox.
// Otherwise, it falls back to a HostSandbox.
func NewDefault() contracts.Sandbox {
	if runtime.GOOS == "linux" {
		if sbx, err := NewSystemd(); err == nil {
			return sbx
		}
	}
	return NewHost()
}
