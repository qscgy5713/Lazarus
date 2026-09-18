package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name string, content []byte, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

func TestLocatePicksNewestGlobMatch(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	writeFile(t, dir, "db-2026-01-01.sql", []byte("SELECT 1;"), now.Add(-72*time.Hour))
	newest := writeFile(t, dir, "db-2026-01-03.sql", []byte("SELECT 1;"), now.Add(-1*time.Hour))
	writeFile(t, dir, "db-2026-01-02.sql", []byte("SELECT 1;"), now.Add(-48*time.Hour))

	got, err := Locate(filepath.Join(dir, "db-*.sql"))
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Path != newest {
		t.Errorf("Locate() = %q, want the newest match %q", got.Path, newest)
	}
}

func TestLocateAcceptsPlainPath(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "dump.sql", []byte("SELECT 1;"), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Path != path {
		t.Errorf("Locate() = %q, want %q", got.Path, path)
	}
	if got.Size != int64(len("SELECT 1;")) {
		t.Errorf("Size = %d, want %d", got.Size, len("SELECT 1;"))
	}
}

func TestLocateNoMatch(t *testing.T) {
	_, err := Locate(filepath.Join(t.TempDir(), "missing-*.sql"))
	if err == nil {
		t.Fatal("Locate() error = nil, want an error when nothing matches")
	}
	if !strings.Contains(err.Error(), "no backup file matches") {
		t.Errorf("error = %q, want it to say nothing matched", err)
	}
}

func TestLocateIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "backups-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := writeFile(t, dir, "backups-file", []byte("SELECT 1;"), time.Now().Add(-time.Hour))

	got, err := Locate(filepath.Join(dir, "backups-*"))
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Path != file {
		t.Errorf("Locate() = %q, want the file %q, not the directory", got.Path, file)
	}
}

func TestDetectsGzipFromExtension(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "dump.sql.gz", []byte{0x1f, 0x8b, 0x08}, time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if !got.Compressed {
		t.Error("Compressed = false, want true for a .gz file")
	}
	if got.Format != FormatPlainSQL {
		t.Errorf("Format = %q, want %q for a gzipped dump", got.Format, FormatPlainSQL)
	}
}

func TestDetectsPostgresCustomFormatByMagicBytes(t *testing.T) {
	dir := t.TempDir()
	// pg_dump -Fc archives start with "PGDMP" — psql can't replay those.
	path := writeFile(t, dir, "prod.dump", append([]byte("PGDMP"), 0x01, 0x0e), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Format != FormatPostgresCustom {
		t.Errorf("Format = %q, want %q", got.Format, FormatPostgresCustom)
	}
}

func TestDetectsEncryptedFromGPGSuffix(t *testing.T) {
	cases := []string{"dump.sql.gpg", "dump.sql.pgp", "dump.sql.asc", "dump.sql.GPG"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, name, []byte("ciphertext, not real SQL"), time.Now())

			got, err := Locate(path)
			if err != nil {
				t.Fatalf("Locate() error = %v", err)
			}
			if !got.Encrypted {
				t.Errorf("Encrypted = false, want true for %q", name)
			}
		})
	}
}

func TestPlainFileIsNotMistakenForEncrypted(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "dump.sql", []byte("SELECT 1;"), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Encrypted {
		t.Error("Encrypted = true, want false for a plain .sql file")
	}
}

func TestDetectsCompressionUnderneathEncryption(t *testing.T) {
	// The real-world convention is compress-then-encrypt, so the .gz suffix
	// ends up second-to-last, not last (dump.sql.gz.gpg) — Compressed has to
	// still be detected correctly with the encrypted suffix stripped off
	// first, not just by checking the very end of the filename.
	dir := t.TempDir()
	path := writeFile(t, dir, "dump.sql.gz.gpg", []byte("ciphertext"), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if !got.Encrypted {
		t.Error("Encrypted = false, want true for dump.sql.gz.gpg")
	}
	if !got.Compressed {
		t.Error("Compressed = false, want true for dump.sql.gz.gpg — .gz is still there, just not at the very end")
	}
}

func TestEncryptedUncompressedFileIsNotMistakenForCompressed(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "dump.sql.gpg", []byte("ciphertext"), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Compressed {
		t.Error("Compressed = true, want false for dump.sql.gpg — there's no .gz anywhere in this name")
	}
}

func TestEncryptedFileDefaultsToPlainSQLFormatWithoutPeekingCiphertext(t *testing.T) {
	// Peeking at the real magic bytes would mean decrypting during Locate,
	// which isn't its job — an encrypted file's "ciphertext" here would
	// never coincidentally start with PGDMP anyway, but the point is Locate
	// must not try to open/decrypt it at all.
	dir := t.TempDir()
	path := writeFile(t, dir, "dump.dump.gpg", []byte("PGDMP-shaped-looking ciphertext that isn't real"), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Format != FormatPlainSQL {
		t.Errorf("Format = %q, want %q for an encrypted file (format is only knowable after decrypting)", got.Format, FormatPlainSQL)
	}
}

func TestPlainSQLDumpIsNotMistakenForCustomFormat(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "prod.sql", []byte("--\n-- PostgreSQL database dump\n--\n"), time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got.Format != FormatPlainSQL {
		t.Errorf("Format = %q, want %q", got.Format, FormatPlainSQL)
	}
}

func TestEmptyFileIsTreatedAsPlainSQL(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "empty.sql", nil, time.Now())

	got, err := Locate(path)
	if err != nil {
		t.Fatalf("Locate() error = %v, want an empty file to locate fine (the restore reports the real problem)", err)
	}
	if got.Format != FormatPlainSQL {
		t.Errorf("Format = %q, want %q", got.Format, FormatPlainSQL)
	}
	if got.Size != 0 {
		t.Errorf("Size = %d, want 0", got.Size)
	}
}

func TestAge(t *testing.T) {
	now := time.Now()
	file := File{ModTime: now.Add(-90 * time.Minute)}

	if got := file.Age(now); got != 90*time.Minute {
		t.Errorf("Age() = %v, want 90m", got)
	}
}
