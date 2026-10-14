package onboarding

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const sourceFileName = "onboarding-sources.json"

type SourceSettings struct {
	Name                string          `json:"name"`
	RepoURL             string          `json:"repoURL"`
	Ref                 string          `json:"ref"`
	RootDir             string          `json:"rootDir"`
	Selection           ConfigSelection `json:"selection"`
	IncludePaths        []string        `json:"includePaths,omitempty"`
	IgnorePaths         []string        `json:"ignorePaths,omitempty"`
	BuildTimeoutSeconds int             `json:"buildTimeoutSeconds,omitempty"`
	UpdatedAt           time.Time       `json:"updatedAt"`
}

type sourceFile struct {
	Sources map[string]SourceSettings `json:"sources"`
}

type sourceStore struct {
	path string
	mu   sync.Mutex
}

func newSourceStore(dataDir string) *sourceStore {
	return &sourceStore{path: filepath.Join(dataDir, sourceFileName)}
}

func (s *sourceStore) get(name string) (SourceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return SourceSettings{}, err
	}
	settings, ok := all.Sources[name]
	if !ok {
		return SourceSettings{}, os.ErrNotExist
	}
	return settings, nil
}

func (s *sourceStore) save(settings SourceSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if all.Sources == nil {
		all.Sources = make(map[string]SourceSettings)
	}
	settings.UpdatedAt = time.Now().UTC()
	all.Sources[settings.Name] = settings
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return errors.New("could not encode persisted repository settings")
	}
	if len(data) > 1<<20 {
		return errors.New("persisted repository settings exceed the size limit")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return errors.New("could not create repository settings storage")
	}
	return writeSourceFileAtomic(s.path, data)
}

func (s *sourceStore) read() (sourceFile, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		return sourceFile{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return sourceFile{}, errors.New("persisted repository settings permissions or size are unsafe")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return sourceFile{}, errors.New("could not read persisted repository settings")
	}
	var all sourceFile
	if err := json.Unmarshal(data, &all); err != nil || all.Sources == nil {
		return sourceFile{}, errors.New("persisted repository settings are invalid")
	}
	return all, nil
}

func (s *sourceStore) canRedeploy(name string) bool {
	settings, err := s.get(name)
	return err == nil && settings.Name == name && settings.RepoURL != ""
}

func (s *sourceStore) delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	delete(all.Sources, name)
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil || len(data) > 1<<20 {
		return errors.New("could not encode persisted repository settings")
	}
	return writeSourceFileAtomic(s.path, data)
}

func writeSourceFileAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".source-settings-*")
	if err != nil {
		return errors.New("could not save repository settings")
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("could not secure repository settings")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return errors.New("could not save repository settings")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("could not sync repository settings")
	}
	if err := file.Close(); err != nil {
		return errors.New("could not close repository settings")
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.New("could not replace repository settings")
	}
	return nil
}
