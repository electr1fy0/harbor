package onboarding

import (
	"encoding/json"
	"os"
	"testing"
)

func TestProtectedStoreRejectsMalformedNonceWithoutPanicking(t *testing.T) {
	store := newProtectedStore(t.TempDir())
	if store.initErr != nil {
		t.Fatal(store.initErr)
	}

	for _, nonce := range [][]byte{nil, []byte("short"), make([]byte, 13)} {
		envelope := protectedEnvelope{Version: 1, Nonce: nonce, Ciphertext: []byte("ciphertext")}
		data, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.read(); err == nil {
			t.Fatalf("malformed nonce length %d was accepted", len(nonce))
		}
	}
}
