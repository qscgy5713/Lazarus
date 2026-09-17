// Package check runs SQL assertions against a freshly restored database.
//
// A restore that "succeeds" can still hand you an empty shell — the schema
// restores fine, every table is empty, and nobody notices until the day it
// matters. These checks are how a backup proves it carries actual data.
package check

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"lazarus/internal/config"
	"lazarus/internal/sandbox"
)

// Result is the outcome of one check.
type Result struct {
	Name   string
	SQL    string
	Value  int64
	Passed bool
	Reason string // why it failed, empty when passed
	Err    error  // the query itself failed to run
}

// RunAll executes every check against the sandbox, in order.
func RunAll(ctx context.Context, sb *sandbox.Sandbox, engine config.Engine, checks []config.Check) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, run(ctx, sb, engine, c))
	}
	return results
}

func run(ctx context.Context, sb *sandbox.Sandbox, engine config.Engine, c config.Check) Result {
	result := Result{Name: c.Name, SQL: c.SQL}

	raw, err := query(ctx, sb, engine, c.SQL)
	if err != nil {
		result.Err = err
		result.Reason = err.Error()
		return result
	}

	value, err := parseScalar(raw)
	if err != nil {
		result.Err = err
		result.Reason = err.Error()
		return result
	}
	result.Value = value

	result.Passed, result.Reason = evaluate(value, c)
	return result
}

func evaluate(value int64, c config.Check) (bool, string) {
	switch {
	case c.Equal != nil && value != *c.Equal:
		return false, fmt.Sprintf("got %d, want exactly %d", value, *c.Equal)
	case c.Min != nil && value < *c.Min:
		return false, fmt.Sprintf("got %d, want at least %d", value, *c.Min)
	case c.Max != nil && value > *c.Max:
		return false, fmt.Sprintf("got %d, want at most %d", value, *c.Max)
	}
	return true, ""
}

func query(ctx context.Context, sb *sandbox.Sandbox, engine config.Engine, sql string) (string, error) {
	var args []string
	switch engine {
	case config.EnginePostgres:
		args = []string{
			"psql",
			"--username", sandbox.User(),
			"--dbname", sandbox.DBName(),
			"--tuples-only", "--no-align",
			"--set", "ON_ERROR_STOP=1",
			"--command", sql,
		}
	case config.EngineMySQL:
		args = []string{
			"mysql",
			"--user=root",
			"--password=" + sandbox.Password(),
			"--skip-column-names", "--batch",
			"--execute=" + sql,
			sandbox.DBName(),
		}
	default:
		return "", fmt.Errorf("unsupported engine %q", engine)
	}

	out, err := sb.Exec(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	return out, nil
}

// parseScalar pulls a single number out of a client's output. Checks are
// deliberately limited to one numeric value: "how many rows" answers almost
// every question worth asking about a restore, and keeps failures unambiguous.
func parseScalar(raw string) (int64, error) {
	cleaned := strings.TrimSpace(raw)
	// MySQL's client writes a password warning to stderr, which Exec folds
	// into the same stream; drop anything that isn't the value itself.
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "Using a password on the command line") {
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return 0, fmt.Errorf("query returned no rows, expected a single number")
	}
	if len(lines) > 1 {
		return 0, fmt.Errorf("query returned %d rows, expected a single number", len(lines))
	}

	value, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("query returned %q, expected a single number", lines[0])
	}
	return value, nil
}
