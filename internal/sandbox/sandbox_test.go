package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lazarus/internal/config"
)

func TestConstantsAndHelpers(t *testing.T) {
	if got := DBName(); got != "lazarus_verify" {
		t.Errorf("DBName() = %q, want %q", got, "lazarus_verify")
	}
	if got := User(); got != "lazarus" {
		t.Errorf("User() = %q, want %q", got, "lazarus")
	}
	if got := Password(); got != "lazarus" {
		t.Errorf("Password() = %q, want %q", got, "lazarus")
	}
}

func TestStopSafety(t *testing.T) {
	// Calling Stop on a nil Sandbox pointer should not panic.
	var nilBox *Sandbox
	nilBox.Stop()

	// Calling Stop on an empty Sandbox struct should not panic or execute docker rm.
	emptyBox := &Sandbox{}
	emptyBox.Stop()

	// Calling Stop multiple times on a sandbox should be idempotent.
	namedBox := &Sandbox{Name: "non-existent-dummy-sandbox"}
	namedBox.Stop()
	if namedBox.Name != "" {
		t.Errorf("expected namedBox.Name to be cleared, got %q", namedBox.Name)
	}
	// Second call with cleared Name should be a no-op.
	namedBox.Stop()
}

func TestStartUnsupportedEngine(t *testing.T) {
	ctx := context.Background()

	// SQLite doesn't use docker containers, so StartWithMount must reject it immediately.
	_, err := StartWithMount(ctx, config.EngineSQLite, "sqlite:latest", "")
	if err == nil {
		t.Fatal("expected error for unsupported engine sqlite, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported engine \"sqlite\"") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Completely unknown engine
	_, err = StartWithMount(ctx, config.Engine("unknown-engine"), "unknown:latest", "")
	if err == nil {
		t.Fatal("expected error for unknown engine, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported engine \"unknown-engine\"") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestReapOrphans(t *testing.T) {
	// 1. Create a dummy expired sqlite temp file in TempDir
	oldTemp := filepath.Join(os.TempDir(), "lazarus-sqlite-test-expired.tmp")
	if err := os.WriteFile(oldTemp, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-5 * time.Hour)
	_ = os.Chtimes(oldTemp, oldTime, oldTime)

	// 2. Call ReapOrphans with 2h cutoff
	_, err := ReapOrphans(context.Background(), 2*time.Hour)
	if err != nil {
		t.Fatalf("ReapOrphans error: %v", err)
	}

	// 3. Verify expired file was swept
	if _, err := os.Stat(oldTemp); !os.IsNotExist(err) {
		_ = os.Remove(oldTemp)
		t.Errorf("expected %s to be reaped", oldTemp)
	}
}

func TestParseSizeToBytes(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"1024B", 1024, false},
		{"1KiB", 1024, false},
		{"1KB", 1000, false},
		{"10MiB", 10 * 1024 * 1024, false},
		{"2.5GB", 2500000000, false},
		{"1GiB", 1024 * 1024 * 1024, false},
		{"500", 500, false},
		{"", 0, true},
		{"invalid-size", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseSizeToBytes(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSizeToBytes(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseSizeToBytes(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFootprintNotRunning(t *testing.T) {
	var s *Sandbox
	if _, err := s.Footprint(context.Background()); err == nil {
		t.Error("expected error for nil sandbox, got nil")
	}
	empty := &Sandbox{}
	if _, err := empty.Footprint(context.Background()); err == nil {
		t.Error("expected error for empty sandbox, got nil")
	}
}
