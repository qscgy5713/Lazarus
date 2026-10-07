package server

import (
	"bytes"
	"fmt"
	"net/http"
)

// handleMetrics exposes Prometheus format metrics for targets and verification drills.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	summary := s.store.GetSummary()
	targets := s.store.GetTargets()

	var buf bytes.Buffer

	// 1. Total targets summary gauge
	buf.WriteString("# HELP lazarus_targets_total Total number of tracked backup targets by status.\n")
	buf.WriteString("# TYPE lazarus_targets_total gauge\n")
	fmt.Fprintf(&buf, "lazarus_targets_total{status=\"healthy\"} %d\n", summary.Healthy)
	fmt.Fprintf(&buf, "lazarus_targets_total{status=\"failed\"} %d\n", summary.Failed)
	fmt.Fprintf(&buf, "lazarus_targets_total{status=\"overdue\"} %d\n", summary.Overdue)
	fmt.Fprintf(&buf, "lazarus_targets_total{status=\"muted\"} %d\n", summary.Muted)

	// 2. Target status gauge (1 = healthy, 0 = failed, 2 = overdue, 3 = muted)
	buf.WriteString("\n# HELP lazarus_target_status Health status of each target (1=healthy, 0=failed, 2=overdue, 3=muted).\n")
	buf.WriteString("# TYPE lazarus_target_status gauge\n")
	for _, t := range targets {
		var statusVal int
		switch t.Status {
		case StatusHealthy:
			statusVal = 1
		case StatusFailed:
			statusVal = 0
		case StatusOverdue:
			statusVal = 2
		case StatusMuted:
			statusVal = 3
		}
		fmt.Fprintf(&buf, "lazarus_target_status{target=%q} %d\n", t.Name, statusVal)
	}

	// 3. Last drill timestamp
	buf.WriteString("\n# HELP lazarus_target_last_drill_timestamp_seconds Unix timestamp of the last drill.\n")
	buf.WriteString("# TYPE lazarus_target_last_drill_timestamp_seconds gauge\n")
	for _, t := range targets {
		if !t.LastDrilledAt.IsZero() {
			fmt.Fprintf(&buf, "lazarus_target_last_drill_timestamp_seconds{target=%q} %d\n", t.Name, t.LastDrilledAt.Unix())
		}
	}

	// 4. Restore duration seconds
	buf.WriteString("\n# HELP lazarus_target_restore_duration_seconds Restore duration of the last drill in seconds.\n")
	buf.WriteString("# TYPE lazarus_target_restore_duration_seconds gauge\n")
	for _, t := range targets {
		restoreSec := float64(t.LastRestoreMs) / 1000.0
		fmt.Fprintf(&buf, "lazarus_target_restore_duration_seconds{target=%q} %.3f\n", t.Name, restoreSec)
	}

	// 5. Total duration seconds
	buf.WriteString("\n# HELP lazarus_target_duration_seconds Total verification drill duration in seconds.\n")
	buf.WriteString("# TYPE lazarus_target_duration_seconds gauge\n")
	for _, t := range targets {
		totalSec := float64(t.LastDurationMs) / 1000.0
		fmt.Fprintf(&buf, "lazarus_target_duration_seconds{target=%q} %.3f\n", t.Name, totalSec)
	}

	// 6. Sandbox Peak RAM Bytes
	buf.WriteString("\n# HELP lazarus_sandbox_peak_memory_bytes Peak RAM consumed by the sandbox container during restoration.\n")
	buf.WriteString("# TYPE lazarus_sandbox_peak_memory_bytes gauge\n")
	for _, t := range targets {
		if t.PeakMemoryBytes > 0 {
			fmt.Fprintf(&buf, "lazarus_sandbox_peak_memory_bytes{target=%q} %d\n", t.Name, t.PeakMemoryBytes)
		}
	}

	// 7. Sandbox Disk Footprint Bytes
	buf.WriteString("\n# HELP lazarus_sandbox_disk_footprint_bytes Uncompressed database disk directory footprint in bytes in the sandbox.\n")
	buf.WriteString("# TYPE lazarus_sandbox_disk_footprint_bytes gauge\n")
	for _, t := range targets {
		if t.DiskFootprintBytes > 0 {
			fmt.Fprintf(&buf, "lazarus_sandbox_disk_footprint_bytes{target=%q} %d\n", t.Name, t.DiskFootprintBytes)
		}
	}

	// 8. Incremental Patches Count
	buf.WriteString("\n# HELP lazarus_incremental_patches_count Total number of incremental patches applied in the last drill.\n")
	buf.WriteString("# TYPE lazarus_incremental_patches_count gauge\n")
	for _, t := range targets {
		fmt.Fprintf(&buf, "lazarus_incremental_patches_count{target=%q} %d\n", t.Name, len(t.IncrementalPatchesApplied))
	}

	// 9. Remediation Playbook Status (1=success, 0=failed, -1=not triggered)
	buf.WriteString("\n# HELP lazarus_remediation_status Status of automated remediation playbook (1=success, 0=failed, -1=not triggered).\n")
	buf.WriteString("# TYPE lazarus_remediation_status gauge\n")
	for _, t := range targets {
		remVal := -1
		if t.Remediation != nil && t.Remediation.Triggered {
			if t.Remediation.Success {
				remVal = 1
			} else {
				remVal = 0
			}
		}
		fmt.Fprintf(&buf, "lazarus_remediation_status{target=%q} %d\n", t.Name, remVal)
	}

	// 10. SLA Compliance Rate & MTTR
	buf.WriteString("\n# HELP lazarus_sla_compliance_rate Overall SLA RTO compliance rate percentage (0-100).\n")
	buf.WriteString("# TYPE lazarus_sla_compliance_rate gauge\n")
	fmt.Fprintf(&buf, "lazarus_sla_compliance_rate %.2f\n", summary.SLAPercentage)

	buf.WriteString("\n# HELP lazarus_mttr_seconds Mean Time to Restore (MTTR) across all databases in seconds.\n")
	buf.WriteString("# TYPE lazarus_mttr_seconds gauge\n")
	fmt.Fprintf(&buf, "lazarus_mttr_seconds %.3f\n", float64(summary.AvgRestoreMs)/1000.0)

	// 11. RPO Data Lag Seconds & Compliance
	buf.WriteString("\n# HELP lazarus_target_rpo_lag_seconds Measured data lag (RPO) in seconds between latest record and drill execution.\n")
	buf.WriteString("# TYPE lazarus_target_rpo_lag_seconds gauge\n")
	for _, t := range targets {
		if t.HasRPOCheck {
			fmt.Fprintf(&buf, "lazarus_target_rpo_lag_seconds{target=%q} %.3f\n", t.Name, t.MaxRPOLagSec)
		}
	}

	buf.WriteString("\n# HELP lazarus_target_rpo_compliant Whether the target conforms to its configured RPO SLA (1=compliant, 0=violated).\n")
	buf.WriteString("# TYPE lazarus_target_rpo_compliant gauge\n")
	for _, t := range targets {
		if t.HasRPOCheck {
			compVal := 1
			if t.RPOViolated {
				compVal = 0
			}
			fmt.Fprintf(&buf, "lazarus_target_rpo_compliant{target=%q} %d\n", t.Name, compVal)
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
