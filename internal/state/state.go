// Package state persists small facts about past runs — currently just the
// backup size from each target's last fully-passed verification — so a
// single run can tell "smaller than usual" from "smaller than everything
// else in this file", which is all any one run can see on its own.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// TargetState is what's remembered about one target's last known-good run.
type TargetState struct {
	LastSizeBytes int64     `json:"last_size_bytes"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// State is the full on-disk record, keyed by target name.
type State struct {
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
func (s *State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write state file %q: %w", path, err)
	}
	return nil
}

// Get returns the remembered state for target, if any.
func (s *State) Get(target string) (TargetState, bool) {
	ts, ok := s.Targets[target]
	return ts, ok
}

// Set records target's latest known-good state.
func (s *State) Set(target string, ts TargetState) {
	if s.Targets == nil {
		s.Targets = map[string]TargetState{}
	}
	s.Targets[target] = ts
}
