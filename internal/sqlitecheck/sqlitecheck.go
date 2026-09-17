// Package sqlitecheck verifies SQLite backups without Docker. Unlike
// Postgres or MySQL, a SQLite backup is already a complete, self-contained
// database file — there's no server to import it into and nothing to
// replay. "Restoring" it just means making a disposable copy and querying
// that copy directly, so this package never touches the original file.
package sqlitecheck

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/config"
)

// Prepare copies file into a throwaway temp file (decompressing it first if
// it's gzipped) and returns that copy's path. The caller must run the
// returned cleanup func once done with it.
func Prepare(file *backup.File) (string, func(), error) {
	src, err := os.Open(file.Path)
	if err != nil {
		return "", nil, fmt.Errorf("open backup %q: %w", file.Path, err)
	}
	defer src.Close()

	var reader io.Reader = src
	if file.Compressed {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return "", nil, fmt.Errorf("backup %q is not readable as gzip: %w", file.Path, err)
		}
		defer gz.Close()
		reader = gz
	}

	tmp, err := os.CreateTemp("", "lazarus-sqlite-*.db")
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	cleanup := func() { os.Remove(tmp.Name()) }

	if _, err := io.Copy(tmp, reader); err != nil {
		tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("copy backup into place: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}

	return tmp.Name(), cleanup, nil
}

// IntegrityCheck runs SQLite's own structural check. A file that isn't
// really a SQLite database, or one that's truncated or corrupted, is caught
// here before any configured check even runs.
func IntegrityCheck(ctx context.Context, path string) error {
	out, err := query(ctx, path, "PRAGMA integrity_check;")
	if err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if out != "ok" {
		return fmt.Errorf("integrity check failed: %s", out)
	}
	return nil
}

// RunChecks runs the target's configured SQL assertions against path,
// sharing check's own pass/fail evaluation so a SQLite target's checks
// behave identically to a Postgres or MySQL one.
func RunChecks(ctx context.Context, path string, checks []config.Check) []check.Result {
	results := make([]check.Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, runCheck(ctx, path, c))
	}
	return results
}

func runCheck(ctx context.Context, path string, c config.Check) check.Result {
	result := check.Result{Name: c.Name, SQL: c.SQL}

	raw, err := query(ctx, path, c.SQL)
	if err != nil {
		result.Err = err
		result.Reason = err.Error()
		return result
	}

	value, err := check.ParseScalar(raw)
	if err != nil {
		result.Err = err
		result.Reason = err.Error()
		return result
	}
	result.Value = value

	result.Passed, result.Reason = check.Evaluate(value, c)
	return result
}

func query(ctx context.Context, path, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-batch", "-noheader", path, sql)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, output)
	}
	return output, nil
}
