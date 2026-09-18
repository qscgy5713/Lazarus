package state

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestSaveLeavesNoTempFileBehindOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := New()
	s.Set("prod-db", TargetState{LastSizeBytes: 1})
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".lazarus-state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind after a successful Save(): %v", matches)
	}
}

func TestSaveProducesAReadableFile(t *testing.T) {
	// The file previously came from os.WriteFile(path, raw, 0o644); the
	// write-temp-then-rename path goes through os.CreateTemp instead, which
	// defaults to a stricter 0600 — this pins that the final file still
	// ends up at the original, more permissive 0644.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := New()
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("state file mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestSaveLeavesExistingFileIntactWhenItFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Chmod on Windows only toggles a file's read-only attribute (the
		// 0200 bit) — it doesn't restrict directory writes the POSIX way,
		// so chmod-ing dir below wouldn't actually make Save() fail here.
		t.Skip("directory permission bits aren't meaningful on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root ignores directory permission bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	original := New()
	original.Set("prod-db", TargetState{LastSizeBytes: 999})
	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A read-only directory can't have a new temp file created in it,
	// standing in for whatever might interrupt a real save (a crash, a
	// permissions problem, a full disk) partway through.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700) // let t.TempDir() clean up afterward

	updated := New()
	updated.Set("prod-db", TargetState{LastSizeBytes: 1})
	if err := updated.Save(path); err == nil {
		t.Fatal("Save() error = nil, want an error when the directory can't be written to")
	}

	os.Chmod(dir, 0o700)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("existing state file was modified despite Save() failing — a failed save must never corrupt what was already there")
	}
}
