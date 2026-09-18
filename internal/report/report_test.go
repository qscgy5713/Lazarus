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
