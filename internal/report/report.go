// Package report turns verification results into something a human reads in
// a terminal, or a machine reads from cron/CI.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"lazarus/internal/verify"
)

// Text writes a human-readable summary and reports whether everything passed.
func Text(w io.Writer, results []verify.Result) bool {
	allPassed := true

	for _, r := range results {
		if r.Passed {
			fmt.Fprintf(w, "PASS  %s (%s)\n", r.Target, r.Duration.Round(time.Millisecond))
		} else {
			allPassed = false
			fmt.Fprintf(w, "FAIL  %s [%s] %v\n", r.Target, r.Stage, r.Err)
		}

		if r.Backup != nil {
			fmt.Fprintf(w, "      backup: %s (%s, %s old)\n",
				r.Backup.Path,
				humanSize(r.Backup.Size),
				r.Backup.Age(time.Now()).Round(time.Minute),
			)
		}
		for _, c := range r.Checks {
			status := "ok"
			detail := fmt.Sprintf("= %d", c.Value)
			if !c.Passed {
				status, detail = "FAILED", c.Reason
			}
			fmt.Fprintf(w, "      check %-6s %s (%s)\n", status, c.Name, detail)
		}
	}

	passed, failed := tally(results)
	fmt.Fprintf(w, "\n%d passed, %d failed\n", passed, failed)

	return allPassed
}

type jsonResult struct {
	Target     string      `json:"target"`
	Passed     bool        `json:"passed"`
	Stage      string      `json:"stage"`
	Error      string      `json:"error,omitempty"`
	BackupPath string      `json:"backup_path,omitempty"`
	BackupAge  string      `json:"backup_age,omitempty"`
	DurationMs int64       `json:"duration_ms"`
	Checks     []jsonCheck `json:"checks,omitempty"`
}

type jsonCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Value  int64  `json:"value"`
	Reason string `json:"reason,omitempty"`
}

// JSON writes machine-readable output and reports whether everything passed.
func JSON(w io.Writer, results []verify.Result) bool {
	out := make([]jsonResult, 0, len(results))
	allPassed := true

	for _, r := range results {
		if !r.Passed {
			allPassed = false
		}

		jr := jsonResult{
			Target:     r.Target,
			Passed:     r.Passed,
			Stage:      string(r.Stage),
			DurationMs: r.Duration.Milliseconds(),
		}
		if r.Err != nil {
			jr.Error = r.Err.Error()
		}
		if r.Backup != nil {
			jr.BackupPath = r.Backup.Path
			jr.BackupAge = r.Backup.Age(time.Now()).Round(time.Second).String()
		}
		for _, c := range r.Checks {
			jr.Checks = append(jr.Checks, jsonCheck{
				Name:   c.Name,
				Passed: c.Passed,
				Value:  c.Value,
				Reason: c.Reason,
			})
		}
		out = append(out, jr)
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(out)

	return allPassed
}

func tally(results []verify.Result) (passed, failed int) {
	for _, r := range results {
		if r.Passed {
			passed++
		} else {
			failed++
		}
	}
	return passed, failed
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
