package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lazarus/internal/config"
)

func TestExceedsRTO(t *testing.T) {
	cases := []struct {
		name   string
		actual time.Duration
		limit  time.Duration
		want   bool
	}{
		{name: "under the limit", actual: 30 * time.Second, limit: time.Minute, want: false},
		{name: "exactly at the limit", actual: time.Minute, limit: time.Minute, want: false},
		{name: "over the limit", actual: 90 * time.Second, limit: time.Minute, want: true},
		{name: "no limit configured", actual: 6 * time.Hour, limit: 0, want: false},
		{name: "negative limit treated as no limit", actual: time.Hour, limit: -1, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceedsRTO(tc.actual, tc.limit); got != tc.want {
				t.Errorf("exceedsRTO(%s, %s) = %v, want %v", tc.actual, tc.limit, got, tc.want)
			}
		})
	}
}

func TestRTOErrorReportsSubSecondDurationsAccurately(t *testing.T) {
	// Regression test: rounding to whole seconds turned a real 116ms
	// restore into a misleading "restore took 0s".
	err := rtoError(116*time.Millisecond, 50*time.Millisecond)

	if strings.Contains(err.Error(), "took 0s") {
		t.Errorf("error = %q, a 116ms restore must not be reported as 0s", err)
	}
	if !strings.Contains(err.Error(), "116ms") {
		t.Errorf("error = %q, want it to state the actual duration", err)
	}
}

func TestExceedsSizeDrift(t *testing.T) {
	cases := []struct {
		name           string
		current        int64
		baseline       int64
		maxDecreasePct float64
		want           bool
	}{
		{name: "unchanged size", current: 1000, baseline: 1000, maxDecreasePct: 10, want: false},
		{name: "grew", current: 2000, baseline: 1000, maxDecreasePct: 10, want: false},
		{name: "shrank within tolerance", current: 950, baseline: 1000, maxDecreasePct: 10, want: false},
		{name: "shrank past tolerance", current: 400, baseline: 1000, maxDecreasePct: 50, want: true},
		{name: "shrank exactly at tolerance", current: 500, baseline: 1000, maxDecreasePct: 50, want: false},
		{name: "no baseline yet", current: 10, baseline: 0, maxDecreasePct: 50, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceedsSizeDrift(tc.current, tc.baseline, tc.maxDecreasePct); got != tc.want {
				t.Errorf("exceedsSizeDrift(%d, %d, %v) = %v, want %v", tc.current, tc.baseline, tc.maxDecreasePct, got, tc.want)
			}
		})
	}
}

func TestSizeDriftErrorStatesSizesAndPercentage(t *testing.T) {
	err := sizeDriftError(400, 1000, 50)

	for _, want := range []string{"60%", "50%"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// Unlike Postgres/MySQL, SQLite needs no Docker daemon, so Run() can be
// exercised end to end — real backup file, real sqlite3 CLI, real check —
// right here instead of only in a manual E2E pass.

func requireSQLite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not found on PATH")
	}
}

func sqliteBackup(t *testing.T, dir, name string, rows int) string {
	t.Helper()
	path := filepath.Join(dir, name)

	run := func(sql string) {
		cmd := exec.Command("sqlite3", path, sql)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sqlite3 %q: %v: %s", sql, err, out)
		}
	}
	run("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);")
	for i := 0; i < rows; i++ {
		run("INSERT INTO users (name) VALUES ('user');")
	}
	return path
}

func TestRunEndToEndSQLitePasses(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 3)

	target := config.Target{
		Name:   "sqlite-target",
		Engine: config.EngineSQLite,
		Path:   path,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false)

	if !result.Passed {
		t.Fatalf("Run() = %+v, want it to pass", result)
	}
	if result.Stage != StageDone {
		t.Errorf("Stage = %q, want %q", result.Stage, StageDone)
	}
	if result.RestoreDuration <= 0 {
		t.Error("RestoreDuration should be measured for a SQLite target too")
	}
}

func TestRunEndToEndSQLiteCatchesEmptyTable(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 0)

	target := config.Target{
		Name:   "sqlite-target",
		Engine: config.EngineSQLite,
		Path:   path,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false)

	if result.Passed {
		t.Fatal("Run() passed, want it to fail against an empty table")
	}
	if result.Stage != StageChecks {
		t.Errorf("Stage = %q, want %q", result.Stage, StageChecks)
	}
}

func TestRunEndToEndSQLiteCatchesCorruptFile(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := config.Target{
		Name:   "sqlite-target",
		Engine: config.EngineSQLite,
		Path:   path,
	}

	result := Run(context.Background(), target, 0, false)

	if result.Passed {
		t.Fatal("Run() passed, want it to fail on a file that isn't a real SQLite database")
	}
	if result.Stage != StageRestore {
		t.Errorf("Stage = %q, want %q", result.Stage, StageRestore)
	}
}

func int64ptr(v int64) *int64 { return &v }
