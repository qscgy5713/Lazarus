package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lazarus/internal/config"
)

func TestCreateCorruptedBackupCopy(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, "test.sql")
	originalData := []byte("CREATE TABLE users (id int); INSERT INTO users VALUES (1);")
	if err := os.WriteFile(origPath, originalData, 0o644); err != nil {
		t.Fatal(err)
	}

	corruptPath, err := createCorruptedBackupCopy(origPath, 16)
	if err != nil {
		t.Fatalf("createCorruptedBackupCopy failed: %v", err)
	}
	defer os.Remove(corruptPath)

	// Ensure original file is untouched
	readOrig, err := os.ReadFile(origPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(readOrig) != string(originalData) {
		t.Fatalf("original file was modified!")
	}

	// Verify corrupted file is different
	readCorrupt, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(readCorrupt) == string(originalData) {
		t.Fatalf("corrupted file should differ from original")
	}
	if len(readCorrupt) != len(originalData) {
		t.Fatalf("expected same length, got %d vs %d", len(readCorrupt), len(originalData))
	}
}

func TestChaosDrillSucceedsWithFallback(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()

	// Create older valid backup (1 hour ago)
	oldPath := sqliteBackup(t, dir, "backup_2026-01-01.db", 5)
	oldTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create newer valid backup (now)
	newPath := sqliteBackup(t, dir, "backup_2026-01-02.db", 10)
	newTime := time.Now()
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	origNewData, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}

	target := config.Target{
		Name:              "chaos-fallback-test",
		Engine:            config.EngineSQLite,
		Path:              filepath.Join(dir, "backup_*.db"),
		FallbackOnFailure: true,
		MaxFallbackDepth:  2,
		Chaos: config.ChaosConfig{
			Enabled:      true,
			CorruptBytes: 64,
		},
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	res := Run(context.Background(), target, 0, false, false, "")

	// 1. Overall verification should pass because fallback rescued it
	if !res.Passed {
		t.Fatalf("expected run to pass with chaos fallback, got err: %v", res.Err)
	}

	// 2. Chaos assertions
	if !res.ChaosInjected {
		t.Error("expected ChaosInjected to be true")
	}
	if !res.ChaosPassed {
		t.Errorf("expected ChaosPassed to be true, message: %s", res.ChaosMessage)
	}
	if !strings.Contains(res.ChaosMessage, "simulated corruption") {
		t.Errorf("expected ChaosMessage to mention simulated corruption, got %q", res.ChaosMessage)
	}

	// 3. Fallback assertions
	if !res.FallbackUsed {
		t.Error("expected FallbackUsed to be true")
	}
	if res.FallbackBackup == nil || filepath.Base(res.FallbackBackup.Path) != "backup_2026-01-01.db" {
		t.Errorf("expected fallback to backup_2026-01-01.db, got %v", res.FallbackBackup)
	}

	// 4. Original newest backup file must remain intact
	currentNewData, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(origNewData) != string(currentNewData) {
		t.Error("newPath file was altered on disk; chaos must use a temporary copy")
	}
}
