// Package restore feeds a backup file into a sandbox database. This is the
// step that actually proves a backup is usable — everything else is bookkeeping.
package restore

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"lazarus/internal/backup"
	"lazarus/internal/config"
	"lazarus/internal/decrypt"
	"lazarus/internal/sandbox"
)

// Run streams file into the sandbox and returns the restore output. A
// non-nil error means the backup could not be restored — which is the whole
// finding this tool exists to produce. gpgPassphrase is only used when
// file.Encrypted; ignored otherwise.
func Run(ctx context.Context, sb *sandbox.Sandbox, engine config.Engine, file *backup.File, gpgPassphrase string) (string, error) {
	// Plain-SQL Postgres dumps reference the roles that owned the original
	// database; create them first so a missing role doesn't fail a backup
	// that's actually fine. pg_restore handles this itself via --no-owner.
	if engine == config.EnginePostgres && file.Format == backup.FormatPlainSQL {
		scanReader, scanCloser, err := open(ctx, file, gpgPassphrase)
		if err != nil {
			return "", err
		}
		roles := scanRoles(scanReader)
		scanCloser()

		if err := ensureRoles(ctx, sb, engine, roles); err != nil {
			return "", err
		}
	}

	reader, closer, err := open(ctx, file, gpgPassphrase)
	if err != nil {
		return "", err
	}
	defer closer()

	args, err := restoreCommand(engine, file)
	if err != nil {
		return "", err
	}

	full := append([]string{"exec", "--interactive", sb.Name}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin = reader

	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return output, fmt.Errorf("restore failed: %w: %s", err, output)
	}

	// psql without ON_ERROR_STOP and pg_restore both exit 0 while printing
	// errors, so an exit code alone would happily call a broken restore a
	// success. Treat error output as failure.
	if problem := findErrorLine(output); problem != "" {
		return output, fmt.Errorf("restore reported errors: %s", problem)
	}

	return output, nil
}

// open returns a reader over file's plaintext, uncompressed content —
// decrypting (if file.Encrypted) and decompressing (if file.Compressed), in
// that order, since the real-world convention is to compress a dump and
// only then encrypt it, never the other way around.
func open(ctx context.Context, file *backup.File, gpgPassphrase string) (io.Reader, func(), error) {
	path := file.Path
	var cleanups []func()
	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	if file.Encrypted {
		decryptedPath, cleanup, err := decrypt.Decrypt(ctx, path, gpgPassphrase)
		if err != nil {
			return nil, nil, err
		}
		path = decryptedPath
		cleanups = append(cleanups, cleanup)
	}

	f, err := os.Open(path)
	if err != nil {
		cleanupAll()
		return nil, nil, fmt.Errorf("open backup %q: %w", path, err)
	}
	cleanups = append(cleanups, func() { f.Close() })

	if !file.Compressed {
		return f, cleanupAll, nil
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		cleanupAll()
		return nil, nil, fmt.Errorf("backup %q is not readable as gzip: %w", path, err)
	}
	cleanups = append(cleanups, func() { gz.Close() })
	return gz, cleanupAll, nil
}

func restoreCommand(engine config.Engine, file *backup.File) ([]string, error) {
	switch engine {
	case config.EnginePostgres:
		if file.Format == backup.FormatPostgresCustom {
			return []string{
				"pg_restore",
				"--username", sandbox.User(),
				"--dbname", sandbox.DBName(),
				"--no-owner", "--no-privileges",
				"--exit-on-error",
			}, nil
		}
		return []string{
			"psql",
			"--username", sandbox.User(),
			"--dbname", sandbox.DBName(),
			// Without this psql shrugs off failing statements and still
			// exits 0, which would make a corrupt dump look restorable.
			"--set", "ON_ERROR_STOP=1",
			"--quiet",
		}, nil

	case config.EngineMySQL:
		return []string{
			"mysql",
			"--user=root",
			"--password=" + sandbox.Password(),
			sandbox.DBName(),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported engine %q", engine)
	}
}

// errorMarkers are the prefixes psql/pg_restore/mysql use for real problems.
// Matching whole lines (rather than searching anywhere) keeps a table or
// column innocently named "error_log" from tripping this.
var errorMarkers = []string{
	"ERROR:",
	"FATAL:",
	"PANIC:",
	"pg_restore: error:",
	"ERROR ",
}

func findErrorLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, marker := range errorMarkers {
			if strings.HasPrefix(trimmed, marker) {
				return trimmed
			}
		}
	}
	return ""
}
