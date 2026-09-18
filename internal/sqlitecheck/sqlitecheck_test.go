package sqlitecheck

import (
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"lazarus/internal/backup"
	"lazarus/internal/config"
)

// requireSQLite skips the test if the sqlite3 CLI isn't on PATH, the same
// way the Docker-backed engines' tests would skip without a daemon.
func requireSQLite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not found on PATH")
	}
}

// newRealDB creates a real SQLite database file with a users table, so
// tests exercise the actual sqlite3 CLI rather than a hand-built fixture.
func newRealDB(t *testing.T, dir string, rows int) string {
	t.Helper()
	path := filepath.Join(dir, "source.db")

	cmd := exec.Command("sqlite3", path, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create table: %v: %s", err, out)
	}

	for i := 0; i < rows; i++ {
		cmd := exec.Command("sqlite3", path, "INSERT INTO users (name) VALUES ('user');")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("insert row: %v: %s", err, out)
		}
	}
	return path
}

func TestPrepareCopiesPlainFile(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	src := newRealDB(t, dir, 3)

	path, cleanup, err := Prepare(context.Background(), &backup.File{Path: src}, "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	if path == src {
		t.Error("Prepare() returned the original path, want a disposable copy")
	}
	if err := IntegrityCheck(context.Background(), path); err != nil {
		t.Errorf("IntegrityCheck() on the copy = %v, want nil", err)
	}
}

func TestPrepareDecompressesGzip(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	src := newRealDB(t, dir, 2)

	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	gzPath := filepath.Join(dir, "source.db.gz")
	f, err := os.Create(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	f.Close()

	path, cleanup, err := Prepare(context.Background(), &backup.File{Path: gzPath, Compressed: true}, "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	if err := IntegrityCheck(context.Background(), path); err != nil {
		t.Errorf("IntegrityCheck() on the decompressed copy = %v, want nil", err)
	}
}

func TestPrepareRejectsMissingFile(t *testing.T) {
	_, _, err := Prepare(context.Background(), &backup.File{Path: filepath.Join(t.TempDir(), "nope.db")}, "")
	if err == nil {
		t.Fatal("Prepare() error = nil, want an error for a missing backup file")
	}
}

func TestIntegrityCheckFailsOnCorruptFile(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.db")
	// Not a real SQLite file at all — the "empty shell" equivalent for this
	// engine: exists, has a name, isn't a real database.
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := IntegrityCheck(context.Background(), path); err == nil {
		t.Fatal("IntegrityCheck() error = nil, want an error for a corrupt file")
	}
}

func TestRunChecksAgainstRealDatabase(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := newRealDB(t, dir, 5)

	checks := []config.Check{
		{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
	}

	results := RunChecks(context.Background(), path, checks)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].Passed {
		t.Errorf("check failed: %s", results[0].Reason)
	}
	if results[0].Value != 5 {
		t.Errorf("Value = %d, want 5", results[0].Value)
	}
}

func TestRunChecksCatchesEmptyTable(t *testing.T) {
	requireSQLite(t)
	dir := t.TempDir()
	path := newRealDB(t, dir, 0)

	checks := []config.Check{
		{Name: "users exist", SQL: "SELECT count(*) FROM users", Min: int64ptr(1)},
	}

	results := RunChecks(context.Background(), path, checks)
	if len(results) != 1 || results[0].Passed {
		t.Fatalf("got %+v, want the check to fail against an empty table", results)
	}
}

func int64ptr(v int64) *int64 { return &v }

func TestPrepareOutputFileIsNotWorldOrGroupReadable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	if err := os.WriteFile(src, []byte("fake sqlite content"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, cleanup, err := Prepare(context.Background(), &backup.File{Path: src}, "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("prepared temp file mode = %v, want no group/other permission bits", info.Mode().Perm())
	}
}

func TestPrepareEncryptedOutputFileIsNotWorldOrGroupReadable(t *testing.T) {
	requireSQLite(t)
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found on PATH")
	}
	dir := t.TempDir()
	src := newRealDB(t, dir, 2)

	encPath := filepath.Join(dir, "source.db.gpg")
	cmd := exec.Command("gpg", "--batch", "--yes", "--passphrase", "pw", "--symmetric", "--cipher-algo", "AES256", "-o", encPath, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encrypt fixture: %v: %s", err, out)
	}

	path, cleanup, err := Prepare(context.Background(), &backup.File{Path: encPath, Encrypted: true}, "pw")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("prepared temp file mode = %v, want no group/other permission bits", info.Mode().Perm())
	}
}
