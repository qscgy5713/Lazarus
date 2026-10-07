package ui

import (
	"bytes"
	"strings"
	"testing"

	"lazarus/internal/config"
	"lazarus/internal/verify"
)

func TestTrackerRendering(t *testing.T) {
	targets := []config.Target{
		{Name: "postgres-main", Engine: config.EnginePostgres},
		{Name: "mysql-billing", Engine: config.EngineMySQL},
	}

	var buf bytes.Buffer
	tracker := NewTracker(&buf, targets, true)

	// Initial render
	tracker.render(true)
	out := buf.String()

	if !strings.Contains(out, "postgres-main") || !strings.Contains(out, "mysql-billing") {
		t.Errorf("output missing target names: %s", out)
	}
	if !strings.Contains(out, "0/2 completed") {
		t.Errorf("output should show 0 completed initially: %s", out)
	}

	// Update first target to done (pass)
	buf.Reset()
	tracker.Update("postgres-main", verify.StageDone, "", true, true)
	tracker.render(false)
	out = buf.String()

	if !strings.Contains(out, "✓ PASS") {
		t.Errorf("expected pass label for postgres-main: %s", out)
	}
	if !strings.Contains(out, "1/2 completed (1 passed)") {
		t.Errorf("expected 1 completed in footer: %s", out)
	}
}

func TestDisabledTrackerIsNoOp(t *testing.T) {
	targets := []config.Target{
		{Name: "sqlite-db", Engine: config.EngineSQLite},
	}

	var buf bytes.Buffer
	tracker := NewTracker(&buf, targets, false)
	tracker.Start()
	tracker.Update("sqlite-db", verify.StageRestore, "", false, false)
	tracker.Stop()

	if buf.Len() != 0 {
		t.Errorf("disabled tracker should produce zero output, got: %q", buf.String())
	}
}

func TestProgressBarCalculation(t *testing.T) {
	cases := []struct {
		pct      int
		width    int
		contains string
	}{
		{0, 10, "[>         ]"},
		{50, 10, "[=====>    ]"},
		{100, 10, "[==========]"},
	}

	for _, tc := range cases {
		got := progressBar(tc.pct, tc.width)
		if got != tc.contains {
			t.Errorf("progressBar(%d, %d) = %q, want %q", tc.pct, tc.width, got, tc.contains)
		}
	}
}
