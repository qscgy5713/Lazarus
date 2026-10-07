// Package report turns verification results into something a human reads in
// a terminal, or a machine reads from cron/CI.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lazarus/internal/backup"
	"lazarus/internal/verify"
)

// Text writes a human-readable summary and reports whether everything passed.
func Text(w io.Writer, results []verify.Result, quiet bool) bool {
	allPassed := true
	printedAny := false

	for _, r := range results {
		if r.Passed {
			if quiet {
				continue
			}
			statusSuffix := ""
			if r.FallbackUsed {
				statusSuffix = fmt.Sprintf(" (FALLBACK RECOVERY - RPO: %s)", r.FallbackRPO.Round(time.Second))
			}
			fmt.Fprintf(w, "PASS  %s (%s)%s\n", r.Target, r.Duration.Round(time.Millisecond), statusSuffix)
		} else {
			allPassed = false
			fmt.Fprintf(w, "FAIL  %s [%s] %v\n", r.Target, r.Stage, r.Err)
		}
		printedAny = true

		if r.FallbackUsed {
			fmt.Fprintf(w, "      fallback: recovered using %s (RPO: %s)\n",
				filepath.Base(r.Backup.Path), r.FallbackRPO.Round(time.Second))
		}

		if r.Backup != nil {
			fmt.Fprintf(w, "      backup: %s (%s, %s old)\n",
				r.Backup.Path,
				backup.HumanSize(r.Backup.Size),
				r.Backup.Age(time.Now()).Round(time.Minute),
			)
		}
		if r.RestoreDuration > 0 {
			fmt.Fprintf(w, "      restore took: %s\n", r.RestoreDuration.Round(time.Millisecond))
		}
		if len(r.IncrementalPatchesApplied) > 0 {
			fmt.Fprintf(w, "      incremental patches: applied %d patch(es) %v\n", len(r.IncrementalPatchesApplied), r.IncrementalPatchesApplied)
		}
		if r.PeakMemoryBytes > 0 || r.DiskFootprintBytes > 0 {
			ramStr := "-"
			if r.PeakMemoryBytes > 0 {
				ramStr = backup.HumanSize(r.PeakMemoryBytes)
			}
			diskStr := "-"
			if r.DiskFootprintBytes > 0 {
				diskStr = backup.HumanSize(r.DiskFootprintBytes)
			}
			fmt.Fprintf(w, "      sandbox resources: RAM Peak: %s | Disk Footprint: %s\n", ramStr, diskStr)
		}
		if r.Remediation != nil && r.Remediation.Triggered {
			status := "SUCCESS"
			if !r.Remediation.Success {
				status = "FAILED"
			}
			fmt.Fprintf(w, "      remediation playbook: %s (status: %s, duration: %s)\n",
				r.Remediation.Command, status, r.Remediation.Duration.Round(time.Millisecond))
		}
		if r.FallbackUsed {
			fmt.Fprintf(w, "      fallback: %s\n", r.FallbackMessage)
		}
		if r.ChaosInjected {
			if r.ChaosPassed {
				fmt.Fprintf(w, "      chaos drill: PASS (%s)\n", r.ChaosMessage)
			} else {
				fmt.Fprintf(w, "      chaos drill: FAILED (%s)\n", r.ChaosMessage)
			}
		}
		if r.SchemaDrift != nil {
			fmt.Fprintf(w, "      schema: %d tables verified\n", r.SchemaDrift.TotalTables)
			if len(r.SchemaDrift.MissingTables) > 0 {
				fmt.Fprintf(w, "      schema WARNING: missing tables %v\n", r.SchemaDrift.MissingTables)
			}
			if len(r.SchemaDrift.EmptyTables) > 0 {
				fmt.Fprintf(w, "      schema WARNING: empty tables %v\n", r.SchemaDrift.EmptyTables)
			}
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
	if printedAny {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d passed, %d failed\n", passed, failed)

	return allPassed
}

type jsonResult struct {
	Target              string      `json:"target"`
	Passed              bool        `json:"passed"`
	Stage               string      `json:"stage"`
	Error               string      `json:"error,omitempty"`
	BackupPath          string      `json:"backup_path,omitempty"`
	BackupAge           string      `json:"backup_age,omitempty"`
	DurationMs          int64       `json:"duration_ms"`
	RestoreDurationMs   int64       `json:"restore_duration_ms,omitempty"`
	Checks              []jsonCheck `json:"checks,omitempty"`
	DebugHint           string      `json:"debug_hint,omitempty"`
	FallbackUsed        bool        `json:"fallback_used,omitempty"`
	FallbackBackup      string      `json:"fallback_backup,omitempty"`
	FallbackRPOSec      float64     `json:"fallback_rpo_seconds,omitempty"`
	FallbackMessage     string      `json:"fallback_message,omitempty"`
	ChaosInjected       bool        `json:"chaos_injected,omitempty"`
	ChaosPassed         bool        `json:"chaos_passed,omitempty"`
	ChaosMessage        string      `json:"chaos_message,omitempty"`
	SchemaTotalTables         int                      `json:"schema_total_tables,omitempty"`
	SchemaMissingTables       []string                 `json:"schema_missing_tables,omitempty"`
	SchemaEmptyTables         []string                 `json:"schema_empty_tables,omitempty"`
	PeakMemoryBytes           int64                    `json:"peak_memory_bytes,omitempty"`
	DiskFootprintBytes        int64                    `json:"disk_footprint_bytes,omitempty"`
	IncrementalPatchesApplied []string                 `json:"incremental_patches_applied,omitempty"`
	Remediation               *verify.RemediationResult `json:"remediation,omitempty"`
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
			Target:                    r.Target,
			Passed:                    r.Passed,
			Stage:                     string(r.Stage),
			DurationMs:                r.Duration.Milliseconds(),
			RestoreDurationMs:         r.RestoreDuration.Milliseconds(),
			DebugHint:                 r.DebugHint,
			PeakMemoryBytes:           r.PeakMemoryBytes,
			DiskFootprintBytes:        r.DiskFootprintBytes,
			IncrementalPatchesApplied: r.IncrementalPatchesApplied,
			Remediation:               r.Remediation,
		}
		if r.Err != nil {
			jr.Error = r.Err.Error()
		}
		if r.Backup != nil {
			jr.BackupPath = r.Backup.Path
			jr.BackupAge = r.Backup.Age(time.Now()).Round(time.Second).String()
		}
		if r.FallbackUsed {
			jr.FallbackUsed = true
			if r.FallbackBackup != nil {
				jr.FallbackBackup = r.FallbackBackup.Path
			}
			jr.FallbackRPOSec = r.FallbackRPO.Seconds()
			jr.FallbackMessage = r.FallbackMessage
		}
		if r.ChaosInjected {
			jr.ChaosInjected = true
			jr.ChaosPassed = r.ChaosPassed
			jr.ChaosMessage = r.ChaosMessage
		}
		if r.SchemaDrift != nil {
			jr.SchemaTotalTables = r.SchemaDrift.TotalTables
			jr.SchemaMissingTables = r.SchemaDrift.MissingTables
			jr.SchemaEmptyTables = r.SchemaDrift.EmptyTables
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

	fmt.Fprintf(w, "| Target | Status | Stage | Duration | Restore Time | Backup Size | RAM / Disk | Checks |\n")
	fmt.Fprintf(w, "| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: |\n")

	for _, r := range results {
		status := "✅ PASS"
		if !r.Passed {
			status = "❌ FAIL"
		} else if r.FallbackUsed {
			status = "🔄 FALLBACK PASS"
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
		resourceStr := "-"
		if r.PeakMemoryBytes > 0 || r.DiskFootprintBytes > 0 {
			ram := "-"
			if r.PeakMemoryBytes > 0 {
				ram = backup.HumanSize(r.PeakMemoryBytes)
			}
			disk := "-"
			if r.DiskFootprintBytes > 0 {
				disk = backup.HumanSize(r.DiskFootprintBytes)
			}
			resourceStr = fmt.Sprintf("%s / %s", ram, disk)
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

		fmt.Fprintf(w, "| `%s` | %s | `%s` | %s | %s | %s | %s | %s |\n",
			r.Target, status, r.Stage, duration, restoreDuration, backupSize, resourceStr, checksStr)
	}
	fmt.Fprintln(w)

	for _, r := range results {
		if r.ChaosInjected {
			if r.ChaosPassed {
				fmt.Fprintf(w, "### 🧪 Chaos Resilience Verified: `%s`\n\n", r.Target)
				fmt.Fprintf(w, "> [!TIP]\n")
				fmt.Fprintf(w, "> %s\n\n", r.ChaosMessage)
			} else {
				fmt.Fprintf(w, "### 🧪 Chaos Drill Failure: `%s`\n\n", r.Target)
				fmt.Fprintf(w, "> [!WARNING]\n")
				fmt.Fprintf(w, "> %s\n\n", r.ChaosMessage)
			}
		}

		if r.FallbackUsed {
			fmt.Fprintf(w, "### 🔄 Fallback Recovery: `%s`\n\n", r.Target)
			fmt.Fprintf(w, "> [!IMPORTANT]\n")
			fmt.Fprintf(w, "> %s\n\n", r.FallbackMessage)
		}

		if len(r.IncrementalPatchesApplied) > 0 {
			fmt.Fprintf(w, "### 🧩 Incremental Patches Chain Replayed: `%s`\n\n", r.Target)
			fmt.Fprintf(w, "> Replayed **%d** patch(es) in chronological order: `%s`\n\n",
				len(r.IncrementalPatchesApplied), strings.Join(r.IncrementalPatchesApplied, ", "))
		}

		if r.Remediation != nil && r.Remediation.Triggered {
			remStatus := "✅ SUCCESS"
			if !r.Remediation.Success {
				remStatus = "❌ FAILED"
			}
			fmt.Fprintf(w, "### 🚨 Automated Incident Remediation Playbook: `%s`\n\n", r.Target)
			fmt.Fprintf(w, "- **Command**: `%s`\n", r.Remediation.Command)
			fmt.Fprintf(w, "- **Status**: %s (Duration: %s)\n", remStatus, r.Remediation.Duration.Round(time.Millisecond))
			if r.Remediation.Output != "" {
				fmt.Fprintf(w, "- **Output**:\n```text\n%s\n```\n", r.Remediation.Output)
			}
			if r.Remediation.Error != "" {
				fmt.Fprintf(w, "- **Error**: `%s`\n", r.Remediation.Error)
			}
			fmt.Fprintln(w)
		}

		if r.SchemaDrift != nil && r.SchemaDrift.HasCriticalDrift {
			fmt.Fprintf(w, "### ⚠️ Schema Drift Warning: `%s`\n\n", r.Target)
			if len(r.SchemaDrift.MissingTables) > 0 {
				fmt.Fprintf(w, "- **Missing Expected Tables**: `%v`\n", r.SchemaDrift.MissingTables)
			}
			if len(r.SchemaDrift.EmptyTables) > 0 {
				fmt.Fprintf(w, "- **Empty Tables (Zero Rows)**: `%v`\n", r.SchemaDrift.EmptyTables)
			}
			fmt.Fprintln(w)
		}

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
