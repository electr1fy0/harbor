package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePolicyJSON(t *testing.T) {
	if err := EnsurePolicyJSON(); err != nil {
		t.Fatalf("EnsurePolicyJSON failed: %v", err)
	}

	home, _ := os.UserHomeDir()
	policyPath := filepath.Join(home, ".config", "containers", "policy.json")
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("policy.json does not exist: %v", err)
	}
}
