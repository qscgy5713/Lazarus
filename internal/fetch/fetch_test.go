package fetch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunExecutesTheCommand(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "fetched.txt")

	err := Run(context.Background(), "echo hello > "+target, 0)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	content, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("expected the command to have created %q: %v", target, readErr)
	}
	if strings.TrimSpace(string(content)) != "hello" {
		t.Errorf("file content = %q, want %q", content, "hello")
	}
}

func TestRunGoesThroughAShellSoPipesWork(t *testing.T) {
	// Proves the command isn't naively split into argv[0]/args — a fetch
	// command with a pipe, redirect, or "&&" chain is exactly the kind of
	// thing real backup-fetch scripts use.
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")

	err := Run(context.Background(), "echo hello | tr a-z A-Z > "+target, 0)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "HELLO" {
		t.Errorf("file content = %q, want %q", content, "HELLO")
	}
}

func TestRunFailureIncludesCommandOutput(t *testing.T) {
	err := Run(context.Background(), "echo access denied >&2; exit 1", 0)
	if err == nil {
		t.Fatal("Run() error = nil, want an error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error = %q, want it to include the command's own diagnostic output", err)
	}
}

func TestRunRespectsTimeout(t *testing.T) {
	start := time.Now()
	err := Run(context.Background(), "sleep 5", 100*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run() error = nil, want a timeout error for a command that outlives its budget")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %s, want it to be killed near the 100ms timeout, not run to completion", elapsed)
	}
}

func TestRunWithoutTimeoutStillRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Run(ctx, "sleep 5", 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run() error = nil, want an error when the parent context is cancelled")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %s, want it to stop once ctx is done", elapsed)
	}
}
