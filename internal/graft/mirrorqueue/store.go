package mirrorqueue

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Store keeps the mirror queue of a client.
type Store interface {
	// Load returns the queue that is stored. A queue that was never written
	// is an empty queue, not an error.
	Load() (*Queue, error)
	// Save writes the queue.
	Save(q *Queue) error
	// Update applies a change to the stored queue and writes the result. The
	// change and the write happen under a lock, so that two processes that
	// manage the queue at the same time do not lose a change.
	Update(change func(*Queue) error) error
}

// FileStore keeps the queue in one JSON file.
type FileStore struct {
	// Path is the file the queue is kept in.
	Path string
}

// NewFileStore returns a store that keeps the queue in that file.
func NewFileStore(path string) *FileStore {
	return &FileStore{Path: path}
}

// DefaultPath returns the path of the queue file of the current user.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("the queue of the mirror has no home to live in: %w", err)
	}
	return filepath.Join(dir, "graft", "mirror-queue.json"), nil
}

// Load returns the queue that is stored in the file.
func (s *FileStore) Load() (*Queue, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("the mirror queue %s cannot be read: %w", s.Path, err)
	}
	q := New()
	if err := json.Unmarshal(data, q); err != nil {
		return nil, fmt.Errorf("the mirror queue %s is not a queue: %w", s.Path, err)
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("the mirror queue %s cannot be used: %w", s.Path, err)
	}
	return q, nil
}

// Save writes the queue to the file. The file is written next to it and moved
// over it, so that a reader never sees half a queue, and a crash never leaves
// a broken one.
func (s *FileStore) Save(q *Queue) error {
	if q.Version == 0 {
		q.Version = queueVersion
	}
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the directory of the mirror queue cannot be created: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mirror-queue-*.tmp")
	if err != nil {
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	// The queue names the repositories and the hosts of the operator, so it is
	// as private as the operator's own files.
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	if err := os.Rename(name, s.Path); err != nil {
		return fmt.Errorf("the mirror queue cannot be written: %w", err)
	}
	return nil
}

// Update applies a change to the stored queue and writes the result.
func (s *FileStore) Update(change func(*Queue) error) error {
	unlock, err := lockFile(s.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	q, err := s.Load()
	if err != nil {
		return err
	}
	if err := change(q); err != nil {
		return err
	}
	return s.Save(q)
}

// lockTimeout is how long a process waits for the lock of the queue.
const lockTimeout = 10 * time.Second

// lockRetry is how long a process waits between two tries for the lock.
const lockRetry = 50 * time.Millisecond

// staleLockAge is how old a lock has to be before it is taken over from the
// process that held it. A process that dies leaves its lock behind, and the
// queue would be locked until the file is removed by hand.
const staleLockAge = 10 * time.Minute

// lockFile takes the lock of a file and returns the function that releases it.
// The lock is a file of its own, so that it works on every system the client
// runs on, without a dependency.
func lockFile(path string) (func(), error) {
	token, err := lockToken()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), token)
			_ = f.Close()
			return func() { releaseLock(path, token) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("the mirror queue cannot be locked: %w", err)
		}
		// A lock that is older than a process that died with it is taken
		// over, so that the queue does not stay locked forever.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > staleLockAge {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the mirror queue is locked by another graft process: %s", path)
		}
		time.Sleep(lockRetry)
	}
}

// releaseLock removes the lock of the file, unless another process took it
// over in the meantime.
func releaseLock(path, token string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if !containsToken(string(data), token) {
		return
	}
	_ = os.Remove(path)
}

// containsToken tells whether the lock file was written by the holder of that
// token.
func containsToken(data, token string) bool {
	for _, field := range fields(data) {
		if field == token {
			return true
		}
	}
	return false
}

// fields splits a line of the lock file into its fields.
func fields(line string) []string {
	var out []string
	current := ""
	for _, r := range line {
		if r == ' ' || r == '\n' || r == '\t' {
			if current != "" {
				out = append(out, current)
				current = ""
			}
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

// lockToken returns a token that tells the lock of one process from the lock
// of another.
func lockToken() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("the mirror queue cannot be locked: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
