// Package check runs SQL assertions against a freshly restored database.
//
// A restore that "succeeds" can still hand you an empty shell — the schema
// restores fine, every table is empty, and nobody notices until the day it
// matters. These checks are how a backup proves it carries actual data.
package check

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lazarus/internal/config"
	"lazarus/internal/sandbox"
)

// Result is the outcome of one check.
type Result struct {
	Name         string
	SQL          string
	Value        int64
	Passed       bool
	Reason       string        // why it failed, empty when passed
	Err          error         // the query itself failed to run
	RPOLag       time.Duration // computed data lag if MaxRPO check
	IsRPOCheck   bool
	RawTimestamp string
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

	if c.MaxRPO > 0 {
		result.IsRPOCheck = true
		result.RawTimestamp = strings.TrimSpace(raw)
		result.Passed, result.RPOLag, result.Reason = EvaluateRPO(raw, c)
		return result
	}

	if c.ExpectString != nil {
		result.Passed, result.Reason = evaluateString(raw, c)
		return result
	}

	if c.Pattern != "" || c.NotPattern != "" {
		result.Passed, result.Reason = evaluatePattern(raw, c)
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

func evaluateString(raw string, c config.Check) (bool, string) {
	if c.ExpectString == nil {
		return false, "no expect_string configured"
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == *c.ExpectString {
		return true, ""
	}
	return false, fmt.Sprintf("got %q, want exactly %q", trimmed, *c.ExpectString)
}

// Evaluate is the same pass/fail comparison run() uses, exported so a
// verification path that doesn't run checks through a sandbox (sqlitecheck,
// which queries a file directly) can still share the exact same logic.
func Evaluate(value int64, c config.Check) (bool, string) { return evaluate(value, c) }

// EvaluatePattern is exported for sqlitecheck to evaluate pattern expectations.
func EvaluatePattern(raw string, c config.Check) (bool, string) { return evaluatePattern(raw, c) }

// EvaluateString is exported for sqlitecheck to evaluate exact string expectations.
func EvaluateString(raw string, c config.Check) (bool, string) { return evaluateString(raw, c) }

// ParseScalar is exported for the same reason as Evaluate.
func ParseScalar(raw string) (int64, error) { return parseScalar(raw) }

func evaluatePattern(raw string, c config.Check) (bool, string) {
	trimmed := strings.TrimSpace(raw)
	if c.Pattern != "" {
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return false, fmt.Sprintf("invalid regex %q: %v", c.Pattern, err)
		}
		if !re.MatchString(trimmed) {
			return false, fmt.Sprintf("output %q did not match expected pattern %q", trimmed, c.Pattern)
		}
	}
	if c.NotPattern != "" {
		re, err := regexp.Compile(c.NotPattern)
		if err != nil {
			return false, fmt.Sprintf("invalid regex %q: %v", c.NotPattern, err)
		}
		if re.MatchString(trimmed) {
			return false, fmt.Sprintf("output %q matched prohibited pattern %q (sanitization failed)", trimmed, c.NotPattern)
		}
	}
	return true, ""
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
	case config.EngineRedis:
		parts := strings.Fields(sql)
		if len(parts) == 0 {
			return "", fmt.Errorf("empty redis command")
		}
		args = append([]string{"redis-cli"}, parts...)
	case config.EngineMongoDB:
		expr := strings.TrimSpace(sql)
		evalCode := fmt.Sprintf("const db = db.getSiblingDB('%s'); print(%s)", sandbox.DBName(), expr)
		args = []string{
			"mongosh",
			"--username", sandbox.User(),
			"--password=" + sandbox.Password(),
			"--authenticationDatabase", "admin",
			"--quiet",
			"--eval", evalCode,
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

// EvaluateRPO evaluates whether the retrieved timestamp falls within the max allowed RPO lag.
func EvaluateRPO(raw string, c config.Check) (bool, time.Duration, string) {
	tRecord, err := ParseTimestamp(raw)
	if err != nil {
		return false, 0, fmt.Sprintf("invalid timestamp for RPO check: %v", err)
	}
	now := time.Now().UTC()
	var lag time.Duration
	if now.After(tRecord) {
		lag = now.Sub(tRecord)
	} else {
		lag = 0
	}
	if lag > c.MaxRPO {
		return false, lag, fmt.Sprintf("RPO exceeded: latest record timestamp %s is %s ago, exceeds max_rpo %s",
			tRecord.Format("2006-01-02 15:04:05"), lag.Round(time.Second), c.MaxRPO)
	}
	return true, lag, ""
}

// ParseTimestamp parses a timestamp string from various standard formats or epoch values.
func ParseTimestamp(raw string) (time.Time, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.Trim(cleaned, `"'`)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return time.Time{}, fmt.Errorf("empty timestamp string")
	}

	// 1. Check if it's a numeric Unix epoch
	if val, err := strconv.ParseInt(cleaned, 10, 64); err == nil {
		if val > 100000000000 { // milliseconds
			return time.UnixMilli(val).UTC(), nil
		}
		return time.Unix(val, 0).UTC(), nil
	}

	// 2. Common SQL and ISO time formats
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
		time.RFC822,
		time.RFC822Z,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, cleaned); err == nil {
			return t.UTC(), nil
		}
		if t, err := time.ParseInLocation(layout, cleaned, time.Local); err == nil {
			return t.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("unrecognized timestamp %q (expected ISO8601, RFC3339, YYYY-MM-DD HH:MM:SS, or Unix epoch)", cleaned)
}
