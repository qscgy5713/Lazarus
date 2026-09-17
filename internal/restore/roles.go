package restore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"lazarus/internal/config"
	"lazarus/internal/sandbox"
)

// A plain-SQL pg_dump carries the ownership of the original database with it
// ("ALTER TABLE x OWNER TO app_user;"). Those roles don't exist in a fresh
// sandbox, so the restore dies on a missing role — reporting a perfectly
// good backup as broken. Since pg_dump's own output is the only description
// of which roles it needs, scan for them and create them before restoring.
//
// pg_restore avoids this with --no-owner; psql has no equivalent, and
// stripping the statements from the stream would mean rewriting the dump
// rather than replaying it faithfully.
var roleReferencePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bOWNER\s+TO\s+"?([A-Za-z_][A-Za-z0-9_$-]*)"?`),
	regexp.MustCompile(`(?i)\bGRANT\b.*\bTO\s+"?([A-Za-z_][A-Za-z0-9_$-]*)"?`),
	regexp.MustCompile(`(?i)\bSESSION\s+AUTHORIZATION\s+"?([A-Za-z_][A-Za-z0-9_$-]*)"?`),
}

// Roles that always exist or aren't real grantees.
var notRealRoles = map[string]bool{
	"public":       true,
	"current_user": true,
	"session_user": true,
	"none":         true,
}

// maxRoleScanLine caps how much of any single line is kept in memory to test
// against the role patterns. A line this long is a COPY data row, never one
// of the short DDL statements being searched for, so the excess is simply
// not inspected — the alternative, using bufio.Scanner and giving up the
// instant one line is too long, silently stops scanning everything after
// it, including the GRANT statements pg_dump writes at the very end of the
// file. That would mean a single wide row makes ensureRoles miss a role a
// perfectly good backup actually needs.
const maxRoleScanLine = 1024 * 1024

// scanRoles streams r looking for roles the dump expects to exist.
func scanRoles(r io.Reader) []string {
	found := map[string]bool{}

	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := readLine(br)
		if line != "" && (strings.Contains(line, "OWNER") || strings.Contains(line, "GRANT") || strings.Contains(line, "AUTHORIZATION")) {
			for _, pattern := range roleReferencePatterns {
				for _, match := range pattern.FindAllStringSubmatch(line, -1) {
					role := match[1]
					if !notRealRoles[strings.ToLower(role)] {
						found[role] = true
					}
				}
			}
		}
		if err != nil {
			break // EOF or a real read error either way ends the scan
		}
	}

	roles := make([]string, 0, len(found))
	for role := range found {
		roles = append(roles, role)
	}
	sort.Strings(roles) // deterministic order keeps failures reproducible
	return roles
}

// readLine returns the next line from br, trimmed of its terminator, capped
// at maxRoleScanLine bytes. Unlike bufio.Scanner, it never gives up when a
// line runs past that cap: it keeps reading (and discarding the overflow)
// until it actually reaches the line's end, so the reader stays in sync and
// every later line is still scanned. The returned error is nil for a
// complete line, or the error that ended the read (io.EOF included) — in
// which case line may still hold a final, unterminated fragment worth
// checking before the caller stops.
func readLine(br *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if room := maxRoleScanLine - len(line); room > 0 {
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			line = append(line, chunk...)
		}

		switch err {
		case nil:
			return strings.TrimRight(string(line), "\r\n"), nil
		case bufio.ErrBufferFull:
			continue // more of this same line still coming
		default:
			return strings.TrimRight(string(line), "\r\n"), err
		}
	}
}

// ensureRoles creates any of roles that don't already exist in the sandbox.
func ensureRoles(ctx context.Context, sb *sandbox.Sandbox, engine config.Engine, roles []string) error {
	if engine != config.EnginePostgres || len(roles) == 0 {
		return nil
	}

	var sql strings.Builder
	for _, role := range roles {
		// Roles are matched from a strict identifier pattern, so they can't
		// carry quotes or statement terminators into here.
		fmt.Fprintf(&sql, `DO $lazarus$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
    CREATE ROLE "%s";
  END IF;
END $lazarus$;
`, role, role)
	}

	_, err := sb.Exec(ctx, sql.String(),
		"psql",
		"--username", sandbox.User(),
		"--dbname", sandbox.DBName(),
		"--set", "ON_ERROR_STOP=1",
		"--quiet",
	)
	if err != nil {
		return fmt.Errorf("pre-create roles %v: %w", roles, err)
	}
	return nil
}
