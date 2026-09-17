package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileReturnsEmptyState(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := s.Get("prod-db"); ok {
		t.Error("expected no baseline for an unseen target")
	}
}

func TestLoadRejectsUnparseableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Error("expected an error for a corrupt state file, got nil")
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s := New()
	s.Set("prod-db", TargetState{LastSizeBytes: 12345, UpdatedAt: time.Now().Truncate(time.Second)})
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ts, ok := loaded.Get("prod-db")
	if !ok {
		t.Fatal("expected a baseline for prod-db after round trip")
	}
	if ts.LastSizeBytes != 12345 {
		t.Errorf("LastSizeBytes = %d, want 12345", ts.LastSizeBytes)
	}
}

func TestGetOnUnknownTargetReportsAbsent(t *testing.T) {
	s := New()
	s.Set("prod-db", TargetState{LastSizeBytes: 1})

	if _, ok := s.Get("other-db"); ok {
		t.Error("expected no baseline for a target that was never set")
	}
}
