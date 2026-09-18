// Package state persists small facts about past runs — currently just the
// backup size from each target's last fully-passed verification — so a
// single run can tell "smaller than usual" from "smaller than everything
// else in this file", which is all any one run can see on its own.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TargetState is what's remembered about one target's last known-good run.
type TargetState struct {
	LastSizeBytes int64     `json:"last_size_bytes"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// State is the full on-disk record, keyed by target name. Verifying targets
// in parallel means Get and Set can be called concurrently from different
// goroutines, so access to Targets is guarded by mu.
type State struct {
	mu      sync.Mutex
	Targets map[string]TargetState `json:"targets"`
}

// New returns an empty state, as if no target had ever run before.
func New() *State {
	return &State{Targets: map[string]TargetState{}}
}

// Load reads the state file at path. A missing file is not an error — it
// just means every target is starting fresh — but a present, unreadable one
// is, since silently discarding it would erase real history.
func Load(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return nil, fmt.Errorf("read state file %q: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse state file %q: %w", path, err)
	}
	if s.Targets == nil {
		s.Targets = map[string]TargetState{}
	}
	return &s, nil
}

// Save writes the state file at path, overwriting whatever was there.
// Writes go to a temp file in the same directory first, then an atomic
// rename replaces path — so a process killed mid-write (SIGKILL, a crash,
// a machine losing power) leaves either the old file or the new one intact,
// never a half-written one. A truncated state file wouldn't corrupt any
// backup, but Load treats a present-and-unparseable one as an error worth
// surfacing rather than silently resetting, so it's worth not creating one.
func (s *State) Save(path string) error {
	s.mu.Lock()
	raw, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	// The temp file has to live in the same directory as path: os.Rename is
	// only atomic within a single filesystem, and a directory elsewhere
	// (e.g. the default /tmp) could easily be a different one.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".lazarus-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write state file %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file %q: %w", tmpPath, err)
	}
	// os.CreateTemp defaults to 0600; match the file's previous 0644 so
	// this change doesn't also quietly restrict who can read it.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("set permissions on temp state file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state file %q: %w", path, err)
	}
	return nil
}

// Get returns the remembered state for target, if any.
func (s *State) Get(target string) (TargetState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.Targets[target]
	return ts, ok
}

// Set records target's latest known-good state.
func (s *State) Set(target string, ts TargetState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Targets == nil {
		s.Targets = map[string]TargetState{}
	}
	s.Targets[target] = ts
}
