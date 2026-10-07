package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/schema"
	"lazarus/internal/verify"
)

func TestTextShowsRestoreDurationWhenPresent(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "prod-db",
			Passed:          true,
			Stage:           verify.StageDone,
			Backup:          &backup.File{Path: "/backups/prod.sql"},
			RestoreDuration: 90 * time.Second,
		},
	}

	var buf bytes.Buffer
	Text(&buf, results, false)

	if !strings.Contains(buf.String(), "restore took: 1m30s") {
		t.Errorf("output missing restore duration:\n%s", buf.String())
	}
}

func TestTextShowsSubSecondRestoreDurationAccurately(t *testing.T) {
	// Regression test: rounding to whole seconds turned a real 116ms
	// restore into a misleading "restore took: 0s".
	results := []verify.Result{
		{
			Target:          "fast-db",
			Passed:          true,
			Stage:           verify.StageDone,
			RestoreDuration: 116 * time.Millisecond,
		},
	}

	var buf bytes.Buffer
	Text(&buf, results, false)

	if strings.Contains(buf.String(), "restore took: 0s") {
		t.Errorf("a 116ms restore must not be reported as 0s:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "restore took: 116ms") {
		t.Errorf("output missing accurate sub-second duration:\n%s", buf.String())
	}
}

func TestTextOmitsRestoreDurationLineWhenNotMeasured(t *testing.T) {
	// A target that failed before reaching the restore step (e.g. the
	// backup file was missing) never has a restore duration to show.
	results := []verify.Result{
		{Target: "prod-db", Passed: false, Stage: verify.StageLocate, Err: errors.New("no backup file matches")},
	}

	var buf bytes.Buffer
	Text(&buf, results, false)

	if strings.Contains(buf.String(), "restore took") {
		t.Errorf("output should not mention restore duration when it was never measured:\n%s", buf.String())
	}
}

func TestTextReportsRTOFailure(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "slow-db",
			Passed:          false,
			Stage:           verify.StageRTO,
			Err:             errors.New("restore took 2h0m0s, longer than the 1h0m0s RTO limit"),
			RestoreDuration: 2 * time.Hour,
		},
	}

	var buf bytes.Buffer
	allPassed := Text(&buf, results, false)

	if allPassed {
		t.Error("Text() reported all passed, want false for an RTO failure")
	}
	if !strings.Contains(buf.String(), "[rto]") {
		t.Errorf("output should name the rto stage:\n%s", buf.String())
	}
}

func TestQuietSuppressesPassingTargetsEntirely(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "prod-db",
			Passed:          true,
			Stage:           verify.StageDone,
			Backup:          &backup.File{Path: "/backups/prod.sql"},
			RestoreDuration: 90 * time.Second,
			Checks:          []check.Result{{Name: "users exist", Passed: true, Value: 5}},
		},
	}

	var buf bytes.Buffer
	allPassed := Text(&buf, results, true)

	if !allPassed {
		t.Error("Text() reported not all passed, want true")
	}
	for _, unwanted := range []string{"PASS", "prod-db", "backup:", "restore took", "check ok"} {
		if strings.Contains(buf.String(), unwanted) {
			t.Errorf("quiet output should mention nothing about a passing target, but contains %q:\n%s", unwanted, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "1 passed, 0 failed") {
		t.Errorf("quiet output should still show the final tally:\n%s", buf.String())
	}
	// Regression test: the tally line was always preceded by a blank
	// separator line meant to set it apart from the per-target blocks above
	// it — with nothing printed above it (quiet, everything passing), that
	// separator became a stray leading blank line instead.
	if buf.String() != "1 passed, 0 failed\n" {
		t.Errorf("quiet output for an all-passing run = %q, want exactly %q with no leading blank line", buf.String(), "1 passed, 0 failed\n")
	}
}

func TestQuietStillShowsFailingTargetsInFull(t *testing.T) {
	results := []verify.Result{
		{Target: "prod-db-ok", Passed: true, Stage: verify.StageDone},
		{
			Target: "prod-db-broken",
			Passed: false,
			Stage:  verify.StageChecks,
			Err:    errors.New(`check "users exist" failed: got 0, want at least 1`),
			Checks: []check.Result{{Name: "users exist", Passed: false, Reason: "got 0, want at least 1"}},
		},
	}

	var buf bytes.Buffer
	allPassed := Text(&buf, results, true)

	if allPassed {
		t.Error("Text() reported all passed, want false")
	}
	if strings.Contains(buf.String(), "prod-db-ok") {
		t.Errorf("quiet output should not mention the passing target:\n%s", buf.String())
	}
	for _, want := range []string{"FAIL", "prod-db-broken", "check FAILED", "1 passed, 1 failed"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("quiet output missing %q for a failing target:\n%s", want, buf.String())
		}
	}
}

func TestJSONIncludesRestoreDurationMs(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "prod-db",
			Passed:          true,
			Stage:           verify.StageDone,
			RestoreDuration: 1500 * time.Millisecond,
		},
	}

	var buf bytes.Buffer
	JSON(&buf, results)

	var decoded []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := decoded[0]["restore_duration_ms"].(float64)
	if !ok || int64(got) != 1500 {
		t.Errorf("restore_duration_ms = %v, want 1500", decoded[0]["restore_duration_ms"])
	}
}

func TestJSONOmitsRestoreDurationMsWhenZero(t *testing.T) {
	results := []verify.Result{
		{Target: "prod-db", Passed: false, Stage: verify.StageLocate, Err: errors.New("no backup file matches")},
	}

	var buf bytes.Buffer
	JSON(&buf, results)

	var decoded []map[string]any
	json.Unmarshal(buf.Bytes(), &decoded)
	if _, present := decoded[0]["restore_duration_ms"]; present {
		t.Errorf("restore_duration_ms should be omitted when never measured, got %+v", decoded[0])
	}
}

func TestStepSummary(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "prod-postgres",
			Passed:          true,
			Stage:           verify.StageDone,
			Duration:        2500 * time.Millisecond,
			RestoreDuration: 1200 * time.Millisecond,
			Backup:          &backup.File{Path: "/backups/prod.sql.gz", Size: 1048576},
			Checks: []check.Result{
				{Name: "user_count", Passed: true, Value: 500},
			},
		},
		{
			Target:   "staging-mysql",
			Passed:   false,
			Stage:    verify.StageRestore,
			Duration: 800 * time.Millisecond,
			Err:      errors.New("syntax error in SQL dump"),
			LogsTail: "mysqld: Table 'broken' doesn't exist\nAborting",
		},
	}

	var buf bytes.Buffer
	if err := StepSummary(&buf, results); err != nil {
		t.Fatalf("StepSummary returned error: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"## 🛡️ Lazarus Disaster Recovery Drill Summary",
		"DRILLS FAILED",
		"| `prod-postgres` | ✅ PASS |",
		"| `staging-mysql` | ❌ FAIL |",
		"mysqld: Table 'broken' doesn't exist",
		"syntax error in SQL dump",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("StepSummary output missing %q, got:\n%s", expected, out)
		}
	}
}

func TestReportFallbackAndSchemaDrift(t *testing.T) {
	results := []verify.Result{
		{
			Target:          "fallback-target",
			Passed:          true,
			Stage:           verify.StageDone,
			Duration:        3 * time.Second,
			RestoreDuration: 1 * time.Second,
			Backup:          &backup.File{Path: "/backups/backup_old.sql", Size: 2048},
			FallbackUsed:    true,
			FallbackBackup:  &backup.File{Path: "/backups/backup_old.sql", Size: 2048},
			FallbackRPO:     2 * time.Hour,
			FallbackMessage: "Primary backup corrupted; fell back to backup_old.sql",
			SchemaDrift: &schema.DriftReport{
				TotalTables:      15,
				MissingTables:    []string{"audit_logs"},
				EmptyTables:      []string{"transactions"},
				HasCriticalDrift: true,
			},
		},
	}

	// 1. Text test
	var textBuf bytes.Buffer
	Text(&textBuf, results, false)
	textOut := textBuf.String()
	if !strings.Contains(textOut, "FALLBACK RECOVERY - RPO: 2h0m0s") {
		t.Errorf("Text output missing fallback RPO notice: %s", textOut)
	}
	if !strings.Contains(textOut, "schema WARNING: missing tables [audit_logs]") {
		t.Errorf("Text output missing schema warning: %s", textOut)
	}

	// 2. JSON test
	var jsonBuf bytes.Buffer
	JSON(&jsonBuf, results)
	var jr []jsonResult
	if err := json.Unmarshal(jsonBuf.Bytes(), &jr); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if !jr[0].FallbackUsed || jr[0].FallbackRPOSec != 7200 {
		t.Errorf("JSON output wrong fallback data: %+v", jr[0])
	}

	// 3. Step summary test
	var stepBuf bytes.Buffer
	if err := StepSummary(&stepBuf, results); err != nil {
		t.Fatalf("StepSummary err: %v", err)
	}
	stepOut := stepBuf.String()
	if !strings.Contains(stepOut, "FALLBACK PASS") || !strings.Contains(stepOut, "Schema Drift Warning") {
		t.Errorf("StepSummary missing fallback or schema warning: %s", stepOut)
	}
}
