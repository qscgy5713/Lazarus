package restore

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"lazarus/internal/backup"
	"lazarus/internal/config"
)

func TestRestoreCommandPostgresPlainSQLStopsOnError(t *testing.T) {
	args, err := restoreCommand(config.EnginePostgres, &backup.File{Format: backup.FormatPlainSQL})
	if err != nil {
		t.Fatalf("restoreCommand() error = %v", err)
	}
	if args[0] != "psql" {
		t.Errorf("command = %q, want psql for a plain SQL dump", args[0])
	}

	// Without ON_ERROR_STOP, psql exits 0 even when statements fail — a
	// corrupt dump would silently pass verification.
	var found bool
	for _, a := range args {
		if a == "ON_ERROR_STOP=1" {
			found = true
		}
	}
	if !found {
		t.Error("psql args missing ON_ERROR_STOP=1; a failing dump would look successful")
	}
}

func TestRestoreCommandPostgresCustomFormatUsesPgRestore(t *testing.T) {
	args, err := restoreCommand(config.EnginePostgres, &backup.File{Format: backup.FormatPostgresCustom})
	if err != nil {
		t.Fatalf("restoreCommand() error = %v", err)
	}
	if args[0] != "pg_restore" {
		t.Errorf("command = %q, want pg_restore for a custom-format archive", args[0])
	}

	var found bool
	for _, a := range args {
		if a == "--exit-on-error" {
			found = true
		}
	}
	if !found {
		t.Error("pg_restore args missing --exit-on-error")
	}
}

func TestRestoreCommandMySQL(t *testing.T) {
	args, err := restoreCommand(config.EngineMySQL, &backup.File{Format: backup.FormatPlainSQL})
	if err != nil {
		t.Fatalf("restoreCommand() error = %v", err)
	}
	if args[0] != "mysql" {
		t.Errorf("command = %q, want mysql", args[0])
	}
}

func TestRestoreCommandUnsupportedEngine(t *testing.T) {
	if _, err := restoreCommand(config.Engine("mongodb"), &backup.File{}); err == nil {
		t.Fatal("restoreCommand() error = nil, want an error for an unsupported engine")
	}
}

func TestFindErrorLine(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "clean output",
			output: "SET\nCREATE TABLE\nCOPY 42\n",
			want:   false,
		},
		{
			name:   "psql error",
			output: "SET\nERROR:  relation \"users\" does not exist\n",
			want:   true,
		},
		{
			name:   "pg_restore error",
			output: "pg_restore: error: could not read from input file: end of file\n",
			want:   true,
		},
		{
			name:   "mysql error",
			output: "ERROR 1064 (42000) at line 12: You have an error in your SQL syntax\n",
			want:   true,
		},
		{
			name:   "fatal",
			output: "FATAL:  database \"nope\" does not exist\n",
			want:   true,
		},
		{
			// A table named error_log shouldn't look like a failure.
			name:   "innocent mention of error",
			output: "CREATE TABLE\nCOPY 3\n-- inserting into error_log\n",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findErrorLine(tc.output) != ""
			if got != tc.want {
				t.Errorf("findErrorLine(%q) found=%v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func TestOpenPlainFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(path, []byte("SELECT 1;"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader, closer, err := open(context.Background(), &backup.File{Path: path}, "")
	if err != nil {
		t.Fatalf("open() error = %v", err)
	}
	defer closer()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "SELECT 1;" {
		t.Errorf("content = %q, want %q", content, "SELECT 1;")
	}
}

func TestOpenDecompressesGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql.gz")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("CREATE TABLE users (id int);")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader, closer, err := open(context.Background(), &backup.File{Path: path, Compressed: true}, "")
	if err != nil {
		t.Fatalf("open() error = %v", err)
	}
	defer closer()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "CREATE TABLE users (id int);" {
		t.Errorf("content = %q, want the decompressed SQL", content)
	}
}

func TestOpenRejectsCorruptGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql.gz")
	// Not actually gzip — exactly the kind of silently broken backup this
	// tool exists to catch.
	if err := os.WriteFile(path, []byte("this is not gzip data"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, _, err := open(context.Background(), &backup.File{Path: path, Compressed: true}, ""); err == nil {
		t.Fatal("open() error = nil, want an error for a file that isn't valid gzip")
	}
}
