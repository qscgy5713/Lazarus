// Package report turns verification results into something a human reads in
// a terminal, or a machine reads from cron/CI.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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

// StepSummary writes a GitHub Flavored Markdown summary suitable for GITHUB_STEP_SUMMARY.
func StepSummary(w io.Writer, results []verify.Result) error {
	passed, failed := tally(results)
	total := passed + failed

	fmt.Fprintf(w, "## 🛡️ Lazarus Disaster Recovery Drill Summary\n\n")

	if failed == 0 {
		fmt.Fprintf(w, "> **Result**: :white_check_mark: **ALL DRILLS PASSED** (%d/%d targets verified successfully)\n\n", passed, total)
	} else {
		fmt.Fprintf(w, "> **Result**: :x: **DRILLS FAILED** (%d passed, %d failed out of %d targets)\n\n", passed, failed, total)
	}

	fmt.Fprintf(w, "| Target | Status | Stage | Duration | Restore Time | Backup Size | Checks |\n")
	fmt.Fprintf(w, "| :--- | :---: | :---: | :---: | :---: | :---: | :---: |\n")

	for _, r := range results {
		status := "✅ PASS"
		if !r.Passed {
			status = "❌ FAIL"
		}
		duration := r.Duration.Round(time.Millisecond).String()
		restoreDuration := "-"
		if r.RestoreDuration > 0 {
			restoreDuration = r.RestoreDuration.Round(time.Millisecond).String()
		}
		backupSize := "-"
		if r.Backup != nil {
			backupSize = backup.HumanSize(r.Backup.Size)
		}
		checksPassed := 0
		for _, c := range r.Checks {
			if c.Passed {
				checksPassed++
			}
		}
		checksStr := fmt.Sprintf("%d/%d", checksPassed, len(r.Checks))
		if len(r.Checks) == 0 {
			checksStr = "none"
		}

		fmt.Fprintf(w, "| `%s` | %s | `%s` | %s | %s | %s | %s |\n",
			r.Target, status, r.Stage, duration, restoreDuration, backupSize, checksStr)
	}
	fmt.Fprintln(w)

	for _, r := range results {
		if !r.Passed {
			fmt.Fprintf(w, "### ⚠️ Failure Details: `%s`\n\n", r.Target)
			if r.Err != nil {
				fmt.Fprintf(w, "**Error**: `%s`\n\n", r.Err.Error())
			}
			if len(r.Checks) > 0 {
				fmt.Fprintf(w, "**Failed Assertions:**\n")
				for _, c := range r.Checks {
					if !c.Passed {
						fmt.Fprintf(w, "- Check `%s`: %s\n", c.Name, c.Reason)
					}
				}
				fmt.Fprintln(w)
			}
			if r.LogsTail != "" {
				fmt.Fprintf(w, "<details><summary><b>Container Stderr / Logs Tail (Last 50 lines)</b></summary>\n\n```text\n%s\n```\n</details>\n\n", strings.TrimSpace(r.LogsTail))
			}
			if r.DebugHint != "" {
				fmt.Fprintf(w, "> **Debug Command**: `%s`\n\n", r.DebugHint)
			}
		}
	}

	fmt.Fprintf(w, "---\n*Drill verified by [Lazarus](https://github.com/chen-yuju/Lazarus) at %s*\n", time.Now().UTC().Format(time.RFC3339))
	return nil
}

// WriteGitHubStepSummaryIfPresent checks if GITHUB_STEP_SUMMARY environment variable
// is set and writes the Markdown summary to that file path.
func WriteGitHubStepSummaryIfPresent(results []verify.Result) error {
	summaryPath := os.Getenv("GITHUB_STEP_SUMMARY")
	if summaryPath == "" {
		return nil
	}

	f, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open GITHUB_STEP_SUMMARY: %w", err)
	}
	defer f.Close()

	return StepSummary(f, results)
}
