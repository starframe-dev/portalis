package portalis

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultClipboardTempFiles = 16
	defaultClipboardTempBytes = 256 << 20
	defaultClipboardTempTTL   = time.Hour
)

// ClipboardTempPolicy bounds clipboard image files created for one Emulator.
type ClipboardTempPolicy struct {
	MaxFiles int
	MaxBytes int64
	TTL      time.Duration
}

// DefaultClipboardTempPolicy returns the default per-emulator clipboard limits.
func DefaultClipboardTempPolicy() ClipboardTempPolicy {
	return ClipboardTempPolicy{
		MaxFiles: defaultClipboardTempFiles,
		MaxBytes: defaultClipboardTempBytes,
		TTL:      defaultClipboardTempTTL,
	}
}

func (p ClipboardTempPolicy) validate() error {
	if p.MaxFiles <= 0 || p.MaxBytes <= 0 || p.TTL <= 0 {
		return fmt.Errorf("clipboard temp limits must be positive")
	}
	return nil
}

type clipboardTempEntry struct {
	size      int64
	expiresAt time.Time
	timer     *time.Timer
}

type clipboardTempStore struct {
	mu     sync.Mutex
	dir    string
	policy ClipboardTempPolicy
	files  map[string]clipboardTempEntry
	total  int64
	closed bool
}

func newClipboardTempStore(policy ClipboardTempPolicy) (*clipboardTempStore, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "portalis-clipboard-")
	if err != nil {
		return nil, fmt.Errorf("create private clipboard directory: %w", err)
	}
	return &clipboardTempStore{
		dir:    dir,
		policy: policy,
		files:  make(map[string]clipboardTempEntry),
	}, nil
}

func (s *clipboardTempStore) directory() string {
	return s.dir
}

func (s *clipboardTempStore) register(path string) error {
	cleanPath := filepath.Clean(path)
	if filepath.Dir(cleanPath) != s.dir {
		return fmt.Errorf("clipboard image is outside the private session directory")
	}
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return fmt.Errorf("stat clipboard image: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("clipboard image must be a private regular file")
	}
	if info.Size() <= 0 {
		return fmt.Errorf("clipboard image is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("clipboard temp directory is closed")
	}
	if _, exists := s.files[cleanPath]; exists {
		return nil
	}
	if len(s.files) >= s.policy.MaxFiles || info.Size() > s.policy.MaxBytes-s.total {
		return fmt.Errorf("clipboard temp files exceed per-session limits")
	}
	entry := clipboardTempEntry{size: info.Size(), expiresAt: time.Now().Add(s.policy.TTL)}
	entry.timer = time.AfterFunc(s.policy.TTL, func() { _ = s.remove(cleanPath) })
	s.files[cleanPath] = entry
	s.total += entry.size
	return nil
}

func (s *clipboardTempStore) remove(path string) error {
	cleanPath := filepath.Clean(path)
	s.mu.Lock()
	entry, exists := s.files[cleanPath]
	if exists {
		delete(s.files, cleanPath)
		s.total -= entry.size
		if s.total < 0 {
			s.total = 0
		}
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}
	s.mu.Unlock()
	if !exists {
		return nil
	}
	if err := os.Remove(cleanPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *clipboardTempStore) expire(path string) error {
	return s.remove(path)
}

func (s *clipboardTempStore) discard(path string) error {
	cleanPath := filepath.Clean(path)
	if filepath.Dir(cleanPath) != s.dir {
		return nil
	}
	s.mu.Lock()
	_, registered := s.files[cleanPath]
	s.mu.Unlock()
	if registered {
		return s.remove(cleanPath)
	}
	if err := os.Remove(cleanPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *clipboardTempStore) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, entry := range s.files {
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}
	dir := s.dir
	s.files = nil
	s.total = 0
	s.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove private clipboard directory: %w", err)
	}
	return nil
}
