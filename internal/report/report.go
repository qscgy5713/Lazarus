// Package report turns verification results into something a human reads in
// a terminal, or a machine reads from cron/CI.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/verify"
)

// Text writes a human-readable summary and reports whether everything passed.
// quiet suppresses every line for a passing target — no PASS line, no
// backup/restore/check detail — so a cron run where everything is fine
// produces just the final tally instead of one block per target. A failing
// target is always printed in full regardless of quiet: a run worth
// investigating is exactly the noise quiet mode isn't meant to cut.
func Text(w io.Writer, results []verify.Result, quiet bool) bool {
	allPassed := true
	printedAny := false

	for _, r := range results {
		if r.Passed {
			if quiet {
				continue
			}
			fmt.Fprintf(w, "PASS  %s (%s)\n", r.Target, r.Duration.Round(time.Millisecond))
		} else {
			allPassed = false
			fmt.Fprintf(w, "FAIL  %s [%s] %v\n", r.Target, r.Stage, r.Err)
		}
		printedAny = true

		if r.Backup != nil {
			fmt.Fprintf(w, "      backup: %s (%s, %s old)\n",
				r.Backup.Path,
				backup.HumanSize(r.Backup.Size),
				r.Backup.Age(time.Now()).Round(time.Minute),
			)
		}
		if r.RestoreDuration > 0 {
			// Surfaced even without a configured limit: restore time is
			// otherwise only discovered for the first time during a real
			// incident, so it's worth seeing on every run. Millisecond
			// precision (matching the overall duration line above) because
			// rounding to whole seconds turns a real 116ms restore into a
			// misleading "0s".
			fmt.Fprintf(w, "      restore took: %s\n", r.RestoreDuration.Round(time.Millisecond))
		}
		for _, c := range r.Checks {
			status := "ok"
			detail := fmt.Sprintf("= %d", c.Value)
			if !c.Passed {
				status, detail = "FAILED", c.Reason
			}
			fmt.Fprintf(w, "      check %-6s %s (%s)\n", status, c.Name, detail)
		}
		if r.DebugHint != "" {
			fmt.Fprintf(w, "      kept for inspection: %s\n", r.DebugHint)
			fmt.Fprintf(w, "      (remember to clean it up yourself when done: docker rm -f, or delete the temp file)\n")
		}
	}

	passed, failed := tally(results)
	// The blank separator line only makes sense after at least one
	// target's own block — with --quiet and nothing to report, skipping it
	// keeps a fully-passing run's output to the single tally line it
	// promises, instead of a stray leading blank line.
	if printedAny {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d passed, %d failed\n", passed, failed)

	return allPassed
}

type jsonResult struct {
	Target            string      `json:"target"`
	Passed            bool        `json:"passed"`
	Stage             string      `json:"stage"`
	Error             string      `json:"error,omitempty"`
	BackupPath        string      `json:"backup_path,omitempty"`
	BackupAge         string      `json:"backup_age,omitempty"`
	DurationMs        int64       `json:"duration_ms"`
	RestoreDurationMs int64       `json:"restore_duration_ms,omitempty"`
	Checks            []jsonCheck `json:"checks,omitempty"`
	DebugHint         string      `json:"debug_hint,omitempty"`
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
			Target:            r.Target,
			Passed:            r.Passed,
			Stage:             string(r.Stage),
			DurationMs:        r.Duration.Milliseconds(),
			RestoreDurationMs: r.RestoreDuration.Milliseconds(),
			DebugHint:         r.DebugHint,
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
