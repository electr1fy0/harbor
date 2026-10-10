package sandbox

import (
	"fmt"
	"paas/internal/contracts"
	"runtime"
)

// NewDefault requires isolation and resource limits on Linux. Host execution
// remains available there only through an explicit -sandbox=host selection.
func NewDefault() (contracts.Sandbox, error) {
	if runtime.GOOS == "linux" {
		sb, err := NewSystemd()
		if err != nil {
			return nil, fmt.Errorf("initialize default Linux sandbox: %w", err)
		}
		return sb, nil
	}
	return NewHost(), nil
}
