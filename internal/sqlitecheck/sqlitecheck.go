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
	"lazarus/internal/decrypt"
)

// Prepare copies file into a throwaway temp file — decrypting it first if
// it's GPG-encrypted, then decompressing if it's gzipped (that order,
// matching the real-world convention of compressing a dump and only then
// encrypting it) — and returns that copy's path. The caller must run the
// returned cleanup func once done with it. gpgPassphrase is only used when
// file.Encrypted; ignored otherwise.
func Prepare(ctx context.Context, file *backup.File, gpgPassphrase string) (string, func(), error) {
	srcPath := file.Path
	var cleanups []func()
	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	if file.Encrypted {
		decryptedPath, cleanup, err := decrypt.Decrypt(ctx, srcPath, gpgPassphrase)
		if err != nil {
			return "", nil, err
		}
		srcPath = decryptedPath
		cleanups = append(cleanups, cleanup)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		cleanupAll()
		return "", nil, fmt.Errorf("open backup %q: %w", srcPath, err)
	}
	defer src.Close()

	var reader io.Reader = src
	if file.Compressed {
		gz, err := gzip.NewReader(src)
		if err != nil {
			cleanupAll()
			return "", nil, fmt.Errorf("backup %q is not readable as gzip: %w", srcPath, err)
		}
		defer gz.Close()
		reader = gz
	}

	tmp, err := os.CreateTemp("", "lazarus-sqlite-*.db")
	if err != nil {
		cleanupAll()
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpCleanup := func() { os.Remove(tmp.Name()) }

	if _, err := io.Copy(tmp, reader); err != nil {
		tmp.Close()
		tmpCleanup()
		cleanupAll()
		return "", nil, fmt.Errorf("copy backup into place: %w", err)
	}
	if err := tmp.Close(); err != nil {
		tmpCleanup()
		cleanupAll()
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}

	return tmp.Name(), func() { tmpCleanup(); cleanupAll() }, nil
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
