package verify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lazarus/internal/config"
	"lazarus/internal/sandbox"
	"lazarus/internal/state"
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

	result := Run(context.Background(), target, 0, false, false, "")

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

	result := Run(context.Background(), target, 0, false, false, "")

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

	result := Run(context.Background(), target, 0, false, false, "")

	if result.Passed {
		t.Fatal("Run() passed, want it to fail on a file that isn't a real SQLite database")
	}
	if result.Stage != StageRestore {
		t.Errorf("Stage = %q, want %q", result.Stage, StageRestore)
	}
	if result.DebugHint != "" {
		t.Errorf("DebugHint = %q, want empty when keepOnFailure wasn't requested", result.DebugHint)
	}
}

func TestRunKeepsSQLiteTempFileWhenRestoreItselfFails(t *testing.T) {
	// keepOnFailure isn't just for a check that fails against a
	// successfully-restored database — a failure in the restore step itself
	// (the copy was made, but the file isn't valid) still has a temp file
	// worth keeping, and the docs promise this too.
	requireSQLite(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := config.Target{Name: "sqlite-target", Engine: config.EngineSQLite, Path: path}

	result := Run(context.Background(), target, 0, false, true, "")

	if result.Passed {
		t.Fatal("Run() passed, want it to fail on a file that isn't a real SQLite database")
	}
	if result.Stage != StageRestore {
		t.Errorf("Stage = %q, want %q", result.Stage, StageRestore)
	}
	if result.DebugHint == "" {
		t.Fatal("DebugHint is empty, want a hint pointing at the kept temp file even for a restore-stage failure")
	}

	tempPath := strings.TrimSpace(strings.TrimPrefix(result.DebugHint, "sqlite3"))
	if _, err := os.Stat(tempPath); err != nil {
		t.Errorf("kept temp file %q should still exist after Run() returns: %v", tempPath, err)
	}
	os.Remove(tempPath)
}

func int64ptr(v int64) *int64 { return &v }

// GPG decryption needs no Docker either, so a SQLite target proves the whole
// decrypt -> restore -> check pipeline end to end with a real gpg binary and
// a real encrypted backup.

func requireGPG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found on PATH")
	}
}

func encryptedSQLiteBackup(t *testing.T, dir string, rows int, passphrase string) string {
	t.Helper()
	plain := sqliteBackup(t, dir, "backup.db", rows)

	encPath := filepath.Join(dir, "backup.db.gpg")
	cmd := exec.Command("gpg", "--batch", "--yes",
		"--passphrase", passphrase,
		"--symmetric", "--cipher-algo", "AES256",
		"-o", encPath, plain)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encrypt fixture: %v: %s", err, out)
	}
	return encPath
}

func TestRunDecryptsGPGEncryptedSQLiteBackup(t *testing.T) {
	requireSQLite(t)
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptedSQLiteBackup(t, dir, 3, "correct-passphrase")

	target := config.Target{
		Name:   "encrypted-sqlite",
		Engine: config.EngineSQLite,
		Path:   encPath,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false, false, "correct-passphrase")

	if !result.Passed {
		t.Fatalf("Run() = %+v, want it to pass once decrypted", result)
	}
}

func TestRunFailsAtRestoreStageWithWrongGPGPassphrase(t *testing.T) {
	requireSQLite(t)
	requireGPG(t)
	dir := t.TempDir()
	encPath := encryptedSQLiteBackup(t, dir, 3, "the-real-passphrase")

	target := config.Target{
		Name:   "encrypted-sqlite",
		Engine: config.EngineSQLite,
		Path:   encPath,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false, false, "wrong-passphrase")

	if result.Passed {
		t.Fatal("Run() passed, want it to fail with the wrong passphrase")
	}
	if result.Stage != StageRestore {
		t.Errorf("Stage = %q, want %q", result.Stage, StageRestore)
	}
}

func TestDebugHintForPostgres(t *testing.T) {
	sb := &sandbox.Sandbox{Name: "lazarus-verify-123", Engine: config.EnginePostgres}

	hint := debugHint(sb)

	if !strings.Contains(hint, "docker exec -it lazarus-verify-123") || !strings.Contains(hint, "psql") {
		t.Errorf("debugHint() = %q, want a docker exec ... psql command naming the container", hint)
	}
}

func TestDebugHintForMySQL(t *testing.T) {
	sb := &sandbox.Sandbox{Name: "lazarus-verify-456", Engine: config.EngineMySQL}

	hint := debugHint(sb)

	if !strings.Contains(hint, "docker exec -it lazarus-verify-456") || !strings.Contains(hint, "mysql") {
		t.Errorf("debugHint() = %q, want a docker exec ... mysql command naming the container", hint)
	}
}

// keepOnFailure is exercised against SQLite since, like the rest of that
// path, it needs no Docker daemon to prove the real behavior: the temp file
// really does (or doesn't) survive Run() returning.

func TestRunKeepsSQLiteTempFileOnFailureWhenAsked(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 0) // empty table -> check fails

	target := config.Target{
		Name:   "sqlite-target",
		Engine: config.EngineSQLite,
		Path:   path,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false, true, "")

	if result.Passed {
		t.Fatal("Run() passed, want it to fail against an empty table")
	}
	if result.DebugHint == "" {
		t.Fatal("DebugHint is empty, want a hint pointing at the kept temp file")
	}
	if !strings.Contains(result.DebugHint, "sqlite3") {
		t.Errorf("DebugHint = %q, want it to mention sqlite3", result.DebugHint)
	}

	tempPath := strings.TrimSpace(strings.TrimPrefix(result.DebugHint, "sqlite3"))
	if _, err := os.Stat(tempPath); err != nil {
		t.Errorf("kept temp file %q should still exist after Run() returns: %v", tempPath, err)
	}
	os.Remove(tempPath) // test cleanup — Run() intentionally left this behind
}

func TestRunCleansUpSQLiteTempFileOnFailureWhenNotAsked(t *testing.T) {
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

	result := Run(context.Background(), target, 0, false, false, "")

	if result.Passed {
		t.Fatal("Run() passed, want it to fail against an empty table")
	}
	if result.DebugHint != "" {
		t.Errorf("DebugHint = %q, want empty when keepOnFailure wasn't requested", result.DebugHint)
	}
}

func TestRunCleansUpSQLiteTempFileOnSuccessEvenWithKeepOnFailureSet(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 1)

	target := config.Target{
		Name:   "sqlite-target",
		Engine: config.EngineSQLite,
		Path:   path,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false, true, "")

	if !result.Passed {
		t.Fatalf("Run() = %+v, want it to pass", result)
	}
	if result.DebugHint != "" {
		t.Errorf("DebugHint = %q, want empty on success — keepOnFailure only applies to failures", result.DebugHint)
	}
}

// FetchCommand runs through a real shell (no Docker needed for that part
// either), so these exercise the actual fetch -> locate handoff end to end:
// a target whose backup doesn't exist at Path until FetchCommand puts it
// there.

func TestRunFetchesBackupBeforeLocating(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	src := sqliteBackup(t, dir, "source.db", 4)
	dst := filepath.Join(dir, "fetched.db")

	if _, err := os.Stat(dst); err == nil {
		t.Fatal("destination should not exist before the fetch command runs")
	}

	target := config.Target{
		Name:         "remote-sqlite",
		Engine:       config.EngineSQLite,
		Path:         dst,
		FetchCommand: fmt.Sprintf("cp %s %s", src, dst),
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	result := Run(context.Background(), target, 0, false, false, "")

	if !result.Passed {
		t.Fatalf("Run() = %+v, want it to pass once the fetch command places the backup", result)
	}
}

func TestRunFailsAtFetchStageWhenFetchCommandFails(t *testing.T) {
	dir := t.TempDir()
	target := config.Target{
		Name:         "remote-sqlite",
		Engine:       config.EngineSQLite,
		Path:         filepath.Join(dir, "never-arrives.db"),
		FetchCommand: "echo access denied >&2; exit 1",
	}

	result := Run(context.Background(), target, 0, false, false, "")

	if result.Passed {
		t.Fatal("Run() passed, want it to fail when the fetch command exits non-zero")
	}
	if result.Stage != StageFetch {
		t.Errorf("Stage = %q, want %q", result.Stage, StageFetch)
	}
	if !strings.Contains(result.Err.Error(), "access denied") {
		t.Errorf("error = %v, want it to include the fetch command's own diagnostic output", result.Err)
	}
}

func TestRunFailsFastWhenFetchCommandOutlivesItsTimeout(t *testing.T) {
	dir := t.TempDir()
	target := config.Target{
		Name:         "remote-sqlite",
		Engine:       config.EngineSQLite,
		Path:         filepath.Join(dir, "never-arrives.db"),
		FetchCommand: "sleep 5",
		FetchTimeout: 100 * time.Millisecond,
	}

	start := time.Now()
	result := Run(context.Background(), target, 0, false, false, "")
	elapsed := time.Since(start)

	if result.Passed {
		t.Fatal("Run() passed, want it to fail when the fetch command exceeds its timeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run() took %s, want the fetch to be killed near its 100ms timeout", elapsed)
	}
}

func TestRunSkipsFetchWhenNoFetchCommandConfigured(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 1)

	target := config.Target{Name: "local-sqlite", Engine: config.EngineSQLite, Path: path}

	result := Run(context.Background(), target, 0, false, false, "")

	if !result.Passed {
		t.Fatalf("Run() = %+v, want a plain local target with no fetch_command to still pass", result)
	}
}

// RunAll's whole point is running targets concurrently, so these run real
// SQLite targets (no Docker needed) through it under -race to prove the
// concurrency itself is safe, not just that each target's own logic works.

func TestRunAllPreservesTargetOrderRegardlessOfCompletionOrder(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()

	const n = 8
	targets := make([]config.Target, n)
	for i := 0; i < n; i++ {
		// Varying row counts vary each target's own work slightly, so
		// completion order in practice won't match target order — which is
		// exactly what this test needs to be a meaningful check.
		path := sqliteBackup(t, dir, fmt.Sprintf("backup-%d.db", i), i)
		targets[i] = config.Target{
			Name:   fmt.Sprintf("target-%d", i),
			Engine: config.EngineSQLite,
			Path:   path,
		}
	}

	results := RunAll(context.Background(), targets, state.New(), 4, false, "")

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for i, r := range results {
		want := fmt.Sprintf("target-%d", i)
		if r.Target != want {
			t.Errorf("results[%d].Target = %q, want %q — order must match the input targets", i, r.Target, want)
		}
	}
}

func TestRunAllRecordsBaselineForEveryPassingTargetConcurrently(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()

	const n = 20
	targets := make([]config.Target, n)
	wantSize := make(map[string]int64, n)
	for i := 0; i < n; i++ {
		path := sqliteBackup(t, dir, fmt.Sprintf("backup-%d.db", i), 1)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("target-%d", i)
		targets[i] = config.Target{Name: name, Engine: config.EngineSQLite, Path: path}
		wantSize[name] = info.Size()
	}

	st := state.New()
	results := RunAll(context.Background(), targets, st, 8, false, "")

	for _, r := range results {
		if !r.Passed {
			t.Fatalf("target %q failed: %v", r.Target, r.Err)
		}
	}

	// This is the part concurrent Set calls could lose: every one of the 20
	// targets that passed must have its own baseline recorded, not just
	// some subset that happened to avoid a race on the shared map.
	for name, want := range wantSize {
		ts, ok := st.Get(name)
		if !ok {
			t.Errorf("target %q: no baseline recorded after a passing run", name)
			continue
		}
		if ts.LastSizeBytes != want {
			t.Errorf("target %q: baseline size = %d, want %d", name, ts.LastSizeBytes, want)
		}
	}
}

func TestRunAllTreatsNonPositiveParallelismAsOne(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 1)
	target := config.Target{Name: "t", Engine: config.EngineSQLite, Path: path}

	for _, p := range []int{0, -1} {
		results := RunAll(context.Background(), []config.Target{target}, state.New(), p, false, "")
		if len(results) != 1 || !results[0].Passed {
			t.Errorf("RunAll with parallelism=%d = %+v, want a single passing result", p, results)
		}
	}
}

func TestRun_S3Download(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	sourceDB := sqliteBackup(t, dir, "source.db", 5)
	dbData, err := os.ReadFile(sourceDB)
	if err != nil {
		t.Fatal(err)
	}

	destPath := filepath.Join(dir, "pulled.db")

	// Mock S3 server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(dbData)
	}))
	defer server.Close()

	target := config.Target{
		Name:   "s3-sqlite",
		Engine: config.EngineSQLite,
		Path:   destPath,
		S3: &config.S3Config{
			Bucket:   "my-bucket",
			Key:      "backups/db.sqlite",
			Endpoint: server.URL,
		},
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if !res.Passed {
		t.Fatalf("expected target to pass, got err: %v", res.Err)
	}
}

func TestCleanupBackup(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	sourceDB := sqliteBackup(t, dir, "to_clean.db", 3)

	target := config.Target{
		Name:          "cleanup-test",
		Engine:        config.EngineSQLite,
		Path:          sourceDB,
		CleanupBackup: true,
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if !res.Passed {
		t.Fatalf("run failed: %v", res.Err)
	}

	// Verify file was removed
	if _, err := os.Stat(sourceDB); !os.IsNotExist(err) {
		t.Fatalf("expected backup file %s to be deleted, stat err: %v", sourceDB, err)
	}
}

func TestPreAndPostDrillCommands(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	sourceDB := sqliteBackup(t, dir, "backup.db", 2)
	preMarker := filepath.Join(dir, "pre_marker.txt")
	postMarker := filepath.Join(dir, "post_marker.txt")

	target := config.Target{
		Name:             "hooks-test",
		Engine:           config.EngineSQLite,
		Path:             sourceDB,
		PreDrillCommand:  fmt.Sprintf("echo $LAZARUS_TARGET > %s", preMarker),
		PostDrillCommand: fmt.Sprintf("echo $LAZARUS_STATUS:$LAZARUS_TARGET > %s", postMarker),
		HooksTimeout:     5 * time.Second,
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if !res.Passed {
		t.Fatalf("run failed: %v", res.Err)
	}

	preContent, err := os.ReadFile(preMarker)
	if err != nil {
		t.Fatalf("failed reading pre marker: %v", err)
	}
	if strings.TrimSpace(string(preContent)) != "hooks-test" {
		t.Errorf("pre content = %q, want 'hooks-test'", string(preContent))
	}

	postContent, err := os.ReadFile(postMarker)
	if err != nil {
		t.Fatalf("failed reading post marker: %v", err)
	}
	if strings.TrimSpace(string(postContent)) != "PASSED:hooks-test" {
		t.Errorf("post content = %q, want 'PASSED:hooks-test'", string(postContent))
	}
}

func TestPreDrillCommandFailureAborts(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	sourceDB := sqliteBackup(t, dir, "backup.db", 2)

	target := config.Target{
		Name:            "pre-fail-test",
		Engine:          config.EngineSQLite,
		Path:            sourceDB,
		PreDrillCommand: "exit 1",
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if res.Passed {
		t.Fatal("expected run to fail on pre hook error")
	}
	if res.Stage != StagePreHook {
		t.Errorf("Stage = %q, want %q", res.Stage, StagePreHook)
	}
}

func TestFallbackOnFailureRecovers(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()

	// Create older valid backup
	oldPath := sqliteBackup(t, dir, "backup_2026-01-01.db", 3)
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create newer corrupt backup
	newPath := filepath.Join(dir, "backup_2026-01-02.db")
	if err := os.WriteFile(newPath, []byte("corrupt data"), 0o644); err != nil {
		t.Fatal(err)
	}
	newTime := time.Now()
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	target := config.Target{
		Name:              "fallback-test",
		Engine:            config.EngineSQLite,
		Path:              filepath.Join(dir, "backup_*.db"),
		FallbackOnFailure: true,
		MaxFallbackDepth:  2,
		Checks: []config.Check{
			{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
		},
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if !res.Passed {
		t.Fatalf("expected run to pass with fallback, got err: %v", res.Err)
	}
	if !res.FallbackUsed {
		t.Error("expected FallbackUsed to be true")
	}
	if res.FallbackBackup == nil || filepath.Base(res.FallbackBackup.Path) != "backup_2026-01-01.db" {
		t.Errorf("expected fallback backup to be backup_2026-01-01.db, got %v", res.FallbackBackup)
	}
	if res.FallbackRPO < 1*time.Hour {
		t.Errorf("expected FallbackRPO ~2 hours, got %v", res.FallbackRPO)
	}
	if !strings.Contains(res.FallbackMessage, "successfully fell back") {
		t.Errorf("FallbackMessage = %q, expected recovery mention", res.FallbackMessage)
	}
}

func TestAutoSchemaCheckSQLite(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := sqliteBackup(t, dir, "backup.db", 3)

	target := config.Target{
		Name:            "schema-check-test",
		Engine:          config.EngineSQLite,
		Path:            path,
		AutoSchemaCheck: true,
		SchemaBaseline:  []string{"users", "non_existent_table"},
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if res.Passed {
		t.Fatal("expected failure due to missing table non_existent_table")
	}
	if res.Stage != StageSchema {
		t.Errorf("Stage = %q, want %q", res.Stage, StageSchema)
	}
	if res.SchemaDrift == nil || len(res.SchemaDrift.MissingTables) == 0 {
		t.Fatal("expected SchemaDrift to report missing table")
	}
	if res.SchemaDrift.MissingTables[0] != "non_existent_table" {
		t.Errorf("missing table = %q, want non_existent_table", res.SchemaDrift.MissingTables[0])
	}
}

func TestRunIncrementalPatchesSQLite(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	basePath := sqliteBackup(t, dir, "base.db", 1)

	// Create an incremental SQL patch
	patchPath := filepath.Join(dir, "patch_01.sql")
	patchContent := "INSERT INTO users (name) VALUES ('charlie');"
	if err := os.WriteFile(patchPath, []byte(patchContent), 0o600); err != nil {
		t.Fatal(err)
	}

	target := config.Target{
		Name:               "patch-test",
		Engine:             config.EngineSQLite,
		Path:               basePath,
		IncrementalPatches: []string{filepath.Join(dir, "patch_*.sql")},
		Checks: []config.Check{
			{Name: "count-users", SQL: "SELECT COUNT(*) FROM users", Pattern: "^2$"},
		},
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if !res.Passed {
		t.Fatalf("expected run with incremental patch to pass, got err: %v", res.Err)
	}
	if len(res.IncrementalPatchesApplied) != 1 || res.IncrementalPatchesApplied[0] != "patch_01.sql" {
		t.Errorf("IncrementalPatchesApplied = %v, want ['patch_01.sql']", res.IncrementalPatchesApplied)
	}
	if res.DiskFootprintBytes <= 0 {
		t.Errorf("DiskFootprintBytes = %d, expected > 0", res.DiskFootprintBytes)
	}
}

func TestRunRemediationOnFailure(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	basePath := sqliteBackup(t, dir, "base.db", 1)
	flagFile := filepath.Join(dir, "remediated.txt")

	target := config.Target{
		Name:   "remediation-test",
		Engine: config.EngineSQLite,
		Path:   basePath,
		Checks: []config.Check{
			{Name: "impossible-check", SQL: "SELECT COUNT(*) FROM users", Pattern: "^999$"},
		},
		Remediation: &config.RemediationConfig{
			Command: fmt.Sprintf("echo 'disaster-recovered' > %s", flagFile),
		},
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if res.Passed {
		t.Fatal("expected drill to fail")
	}
	if res.Remediation == nil {
		t.Fatal("expected Remediation result to be populated")
	}
	if !res.Remediation.Triggered || !res.Remediation.Success {
		t.Errorf("Remediation = %+v, expected triggered and success", res.Remediation)
	}
	data, err := os.ReadFile(flagFile)
	if err != nil || !strings.Contains(string(data), "disaster-recovered") {
		t.Errorf("expected remediation script to execute and write to flagFile, got: %s (err: %v)", string(data), err)
	}
}

// The primary attempt can record an RPO violation before failing; once the
// fallback backup passes, the reported RPO verdict must be the fallback's.
func TestFallbackSuccessReplacesPrimaryRPOVerdict(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()

	mk := func(name string, recordAt time.Time, mtime time.Time) {
		path := filepath.Join(dir, name)
		sql := fmt.Sprintf("CREATE TABLE events (ts INTEGER); INSERT INTO events VALUES (%d);", recordAt.Unix())
		if out, err := exec.Command("sqlite3", path, sql).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v: %s", err, out)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	mk("b_old.db", now.Add(-5*time.Minute), now.Add(-time.Hour)) // fresh data
	mk("b_new.db", now.Add(-48*time.Hour), now)                  // stale data: RPO violated

	target := config.Target{
		Name:              "rpo-fallback",
		Engine:            config.EngineSQLite,
		Path:              filepath.Join(dir, "b_*.db"),
		FallbackOnFailure: true,
		MaxFallbackDepth:  1,
		Checks: []config.Check{
			{Name: "fresh", SQL: "SELECT max(ts) FROM events", MaxRPO: time.Hour},
		},
	}

	res := Run(context.Background(), target, 0, false, false, "")
	if !res.Passed || !res.FallbackUsed {
		t.Fatalf("expected pass via fallback, got passed=%v fallback=%v err=%v", res.Passed, res.FallbackUsed, res.Err)
	}
	if res.RPOViolated {
		t.Error("RPOViolated carried over from the failed primary attempt")
	}
	if !res.HasRPOCheck || res.MaxRPOLag > time.Hour {
		t.Errorf("RPO verdict = has=%v lag=%v, want the fallback's (~5m)", res.HasRPOCheck, res.MaxRPOLag)
	}
}

func TestFallbackPassDoesNotLowerSizeDriftBaseline(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	oldPath := sqliteBackup(t, dir, "s_old.db", 3)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(oldPath, old, old)
	if err := os.WriteFile(filepath.Join(dir, "s_new.db"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.New()
	st.Set("drift", state.TargetState{LastSizeBytes: 999999})
	targets := []config.Target{{
		Name: "drift", Engine: config.EngineSQLite, Path: filepath.Join(dir, "s_*.db"),
		FallbackOnFailure: true, MaxFallbackDepth: 1,
		Checks: []config.Check{{Name: "users", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)}},
	}}
	res := RunAll(context.Background(), targets, st, 1, false, "")
	if !res[0].Passed || !res[0].FallbackUsed {
		t.Fatalf("expected fallback pass, got %+v", res[0])
	}
	if ts, _ := st.Get("drift"); ts.LastSizeBytes != 999999 {
		t.Errorf("baseline overwritten with fallback size %d", ts.LastSizeBytes)
	}
}
