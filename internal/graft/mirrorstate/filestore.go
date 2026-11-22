package mirrorstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	// mirrorDirRel is the directory of the mirror state in the git
	// directory of a repository.
	mirrorDirRel = "graft/mirror"
	// targetsDirName holds one file per mirror target.
	targetsDirName = "targets"
	// syncsDirName holds one file per mirror target, with the state of its
	// last publication.
	syncsDirName = "syncs"
	// lockFileName is the file lock that serialises the mutations of the
	// state across the processes of a node.
	lockFileName = ".lock"
	// fileSuffix is the suffix of the files of the state.
	fileSuffix = ".json"
)

// FileStore keeps the mirror state of one repository in the files of its git
// directory.
//
// Layout: <git dir>/graft/mirror/targets/<name>.json holds one target, and
// <git dir>/graft/mirror/syncs/<name>.json the state of its last
// publication. Every write goes through a temporary file, an fsync and a
// rename, so that a reader never sees half a state.
type FileStore struct {
	repoPath string
	// mu serialises the mutations of one process.
	mu sync.Mutex
}

var _ Store = (*FileStore)(nil)

// Open returns a FileStore for the repository at repoPath (its git
// directory). Open does not touch the disk.
func Open(repoPath string) *FileStore {
	return &FileStore{repoPath: repoPath}
}

// dir returns the directory of the mirror state.
func (s *FileStore) dir() string {
	return filepath.Join(s.repoPath, filepath.FromSlash(mirrorDirRel))
}

// targetsDir returns the directory of the targets.
func (s *FileStore) targetsDir() string {
	return filepath.Join(s.dir(), targetsDirName)
}

// syncsDir returns the directory of the synchronisation states.
func (s *FileStore) syncsDir() string {
	return filepath.Join(s.dir(), syncsDirName)
}

// RegisterTarget records a target of the repository.
func (s *FileStore) RegisterTarget(_ context.Context, _ Key, target Target) error {
	if err := target.Validate(); err != nil {
		return err
	}

	return s.write(target.Name, s.targetsDir(), target)
}

// UnregisterTarget takes a target out of the repository.
func (s *FileStore) UnregisterTarget(_ context.Context, _ Key, name string) error {
	if !validName(name) {
		return ErrTargetNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	path := filepath.Join(s.targetsDir(), name+fileSuffix)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrTargetNotFound
		}
		return fmt.Errorf("read the mirror target %q: %w", name, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove the mirror target %q: %w", name, err)
	}
	return syncDir(s.targetsDir())
}

// ListTargets returns the targets of the repository, by name.
func (s *FileStore) ListTargets(_ context.Context, _ Key) ([]Target, error) {
	dir := s.targetsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the mirror targets: %w", err)
	}

	targets := make([]Target, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != fileSuffix {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read the mirror target %q: %w", entry.Name(), err)
		}
		var target Target
		if err := json.Unmarshal(data, &target); err != nil {
			return nil, fmt.Errorf("decode the mirror target %q: %w", entry.Name(), err)
		}
		targets = append(targets, target)
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

// RecordSync records the state of the last publication of a target.
func (s *FileStore) RecordSync(_ context.Context, _ Key, sync Sync) error {
	if err := sync.Validate(); err != nil {
		return err
	}

	return s.write(sync.Target, s.syncsDir(), sync)
}

// ListSyncs returns the state of the last publication of every target of the
// repository, by target name.
func (s *FileStore) ListSyncs(_ context.Context, _ Key) ([]Sync, error) {
	dir := s.syncsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the mirror synchronisations: %w", err)
	}

	syncs := make([]Sync, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != fileSuffix {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read the mirror synchronisation %q: %w", entry.Name(), err)
		}
		var sync Sync
		if err := json.Unmarshal(data, &sync); err != nil {
			return nil, fmt.Errorf("decode the mirror synchronisation %q: %w", entry.Name(), err)
		}
		syncs = append(syncs, sync)
	}

	sort.Slice(syncs, func(i, j int) bool { return syncs[i].Target < syncs[j].Target })
	return syncs, nil
}

// write records a target or a synchronisation in the directory, under the
// lock of the state.
func (s *FileStore) write(name, dir string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create the mirror state directory: %w", err)
	}

	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the mirror state: %w", err)
	}
	data = append(data, '\n')

	return writeFileAtomic(dir, name+fileSuffix, data)
}

// lock takes the lock of the state of the repository, so that two processes
// of a node do not write it at the same time.
func (s *FileStore) lock() (func(), error) {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return nil, fmt.Errorf("create the mirror state directory: %w", err)
	}
	return lockFile(filepath.Join(s.dir(), lockFileName))
}

// writeFileAtomic writes the data in the file of the directory, through a
// temporary file, an fsync and a rename.
func writeFileAtomic(dir, name string, data []byte) (returnErr error) {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}

	defer func() {
		if returnErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}

	return syncDir(dir)
}
