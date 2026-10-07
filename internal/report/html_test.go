package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/schema"
	"lazarus/internal/verify"
)

func TestGenerateHTMLReport(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "prod-postgres",
			Passed:          true,
			Stage:           verify.StageDone,
			Duration:        2500 * time.Millisecond,
			RestoreDuration: 1800 * time.Millisecond,
			Backup: &backup.File{
				Path:    "/backups/prod-db.sql.gz",
				Size:    1024 * 1024 * 50,
				ModTime: time.Now().Add(-2 * time.Hour),
			},
			Checks: []check.Result{
				{Name: "users count", Passed: true, Value: 15420},
			},
			SchemaDrift: &schema.DriftReport{
				TotalTables: 42,
			},
		},
		{
			Target:          "analytics-mysql",
			Passed:          false,
			Stage:           verify.StageChecks,
			Duration:        3500 * time.Millisecond,
			RestoreDuration: 2200 * time.Millisecond,
			Backup: &backup.File{
				Path:    "/backups/analytics.sql.gz",
				Size:    1024 * 1024 * 120,
				ModTime: time.Now().Add(-5 * time.Hour),
			},
			Checks: []check.Result{
				{Name: "events count", Passed: false, Reason: "got 0, want at least 1"},
			},
		},
	}

	var buf bytes.Buffer
	err := HTML(&buf, results, "ACME Corp DR Audit")
	if err != nil {
		t.Fatalf("HTML() error: %v", err)
	}

	out := buf.String()

	// Check title and targets
	if !strings.Contains(out, "ACME Corp DR Audit") {
		t.Errorf("HTML does not contain custom title")
	}
	if !strings.Contains(out, "prod-postgres") || !strings.Contains(out, "analytics-mysql") {
		t.Errorf("HTML does not contain all targets")
	}

	// Check metrics
	if !strings.Contains(out, "50.0%") {
		t.Errorf("HTML does not contain SLA percentage (50.0%%)")
	}

	// Check checks assertions
	if !strings.Contains(out, "users count") || !strings.Contains(out, "15420") {
		t.Errorf("HTML does not contain check result details")
	}
	if !strings.Contains(out, "got 0, want at least 1") {
		t.Errorf("HTML does not contain failure reason")
	}

	// Check CSS print stylesheet
	if !strings.Contains(out, "@media print") {
		t.Errorf("HTML missing print stylesheet")
	}
}
