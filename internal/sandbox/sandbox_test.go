package sandbox

import (
	"context"
	"strings"
	"testing"

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
