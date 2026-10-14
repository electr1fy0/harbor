package onboarding

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	protectedStateFile = "onboarding-automation.enc"
	protectedKeyFile   = "onboarding-automation.key"
	protectedStateMax  = 32 << 20
)

type HookMetadata struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Branch    string    `json:"branch"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Active    bool      `json:"active"`
}

type storedHook struct {
	Metadata HookMetadata `json:"metadata"`
	Secret   []byte       `json:"secret"`
}

type storedJob struct {
	Job                Job           `json:"job"`
	Request            DeployRequest `json:"request"`
	PreserveSettings   bool          `json:"preserveSettings"`
	RequireCommitMatch bool          `json:"requireCommitMatch"`
	Terminal           bool          `json:"terminal"`
}

type dedupRecord struct {
	JobID     string    `json:"jobID"`
	CreatedAt time.Time `json:"createdAt"`
}

type protectedState struct {
	BuildEnvironment map[string]map[string]string `json:"buildEnvironment,omitempty"`
	Hooks            map[string]storedHook        `json:"hooks,omitempty"`
	Jobs             map[string]storedJob         `json:"jobs,omitempty"`
	Order            []string                     `json:"order,omitempty"`
	Dedup            map[string]dedupRecord       `json:"dedup,omitempty"`
}

type protectedEnvelope struct {
	Version    int    `json:"version"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type protectedStore struct {
	path    string
	keyPath string
	key     []byte
	mu      sync.Mutex
	memory  protectedState
	initErr error
}

func newProtectedStore(dataDir string) *protectedStore {
	s := &protectedStore{memory: emptyProtectedState()}
	if dataDir == "" {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			s.initErr = errors.New("automation encryption is unavailable")
		} else {
			s.key = append([]byte(nil), key[:]...)
		}
		return s
	}
	s.path = filepath.Join(dataDir, protectedStateFile)
	s.keyPath = filepath.Join(dataDir, protectedKeyFile)
	if err := s.loadOrCreateKey(); err != nil {
		s.initErr = err
	}
	return s
}

func emptyProtectedState() protectedState {
	return protectedState{BuildEnvironment: make(map[string]map[string]string), Hooks: make(map[string]storedHook), Jobs: make(map[string]storedJob), Dedup: make(map[string]dedupRecord)}
}

func (s *protectedStore) loadOrCreateKey() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return errors.New("create protected automation storage")
	}
	read := func() error {
		info, err := os.Lstat(s.keyPath)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 || info.Size() != 32 {
			return errors.New("automation encryption key permissions or size are unsafe")
		}
		key, err := os.ReadFile(s.keyPath)
		if err != nil || len(key) != 32 {
			return errors.New("read automation encryption key")
		}
		s.key = key
		return nil
	}
	if err := read(); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(s.path); err == nil {
		return errors.New("automation encryption key is missing")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect protected automation storage")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return errors.New("generate automation encryption key")
	}
	file, err := os.OpenFile(s.keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return read()
	}
	if err != nil {
		return errors.New("create automation encryption key")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(s.keyPath)
		return errors.New("secure automation encryption key")
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		_ = os.Remove(s.keyPath)
		return errors.New("write automation encryption key")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(s.keyPath)
		return errors.New("sync automation encryption key")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(s.keyPath)
		return errors.New("close automation encryption key")
	}
	s.key = key
	return nil
}

func (s *protectedStore) read() (protectedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return protectedState{}, s.initErr
	}
	return s.readLocked()
}

func (s *protectedStore) update(change func(*protectedState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return s.initErr
	}
	state, err := s.readLocked()
	if err != nil {
		return err
	}
	if err := change(&state); err != nil {
		return err
	}
	return s.writeLocked(state)
}

func (s *protectedStore) readLocked() (protectedState, error) {
	if s.path == "" {
		return cloneProtectedState(s.memory), nil
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyProtectedState(), nil
	}
	if err != nil {
		return protectedState{}, errors.New("inspect protected automation state")
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 || info.Size() > protectedStateMax {
		return protectedState{}, errors.New("protected automation state permissions or size are unsafe")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return protectedState{}, errors.New("read protected automation state")
	}
	var envelope protectedEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Version != 1 {
		return protectedState{}, errors.New("protected automation state is invalid")
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return protectedState{}, errors.New("initialize automation encryption")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return protectedState{}, errors.New("initialize automation encryption")
	}
	plain, err := gcm.Open(nil, envelope.Nonce, envelope.Ciphertext, []byte("paas-automation-v1"))
	if err != nil {
		return protectedState{}, errors.New("decrypt protected automation state")
	}
	if len(plain) > protectedStateMax {
		return protectedState{}, errors.New("protected automation state exceeds size limit")
	}
	var state protectedState
	if err := json.Unmarshal(plain, &state); err != nil {
		return protectedState{}, errors.New("protected automation state is invalid")
	}
	ensureProtectedMaps(&state)
	return state, nil
}

func (s *protectedStore) writeLocked(state protectedState) error {
	ensureProtectedMaps(&state)
	plain, err := json.Marshal(state)
	if err != nil || len(plain) > protectedStateMax {
		return errors.New("protected automation state exceeds size limit")
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return errors.New("initialize automation encryption")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return errors.New("initialize automation encryption")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return errors.New("create automation encryption nonce")
	}
	envelope := protectedEnvelope{Version: 1, Nonce: nonce, Ciphertext: gcm.Seal(nil, nonce, plain, []byte("paas-automation-v1"))}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > protectedStateMax {
		return errors.New("protected automation state exceeds size limit")
	}
	if s.path == "" {
		s.memory = cloneProtectedState(state)
		return nil
	}
	if err := writeSourceFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("save protected automation state: %w", err)
	}
	return nil
}

func ensureProtectedMaps(state *protectedState) {
	if state.BuildEnvironment == nil {
		state.BuildEnvironment = make(map[string]map[string]string)
	}
	if state.Hooks == nil {
		state.Hooks = make(map[string]storedHook)
	}
	if state.Jobs == nil {
		state.Jobs = make(map[string]storedJob)
	}
	if state.Dedup == nil {
		state.Dedup = make(map[string]dedupRecord)
	}
}

func cloneProtectedState(state protectedState) protectedState {
	data, err := json.Marshal(state)
	if err != nil {
		return emptyProtectedState()
	}
	var clone protectedState
	if err := json.Unmarshal(data, &clone); err != nil {
		return emptyProtectedState()
	}
	ensureProtectedMaps(&clone)
	return clone
}
