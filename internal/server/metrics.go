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

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
