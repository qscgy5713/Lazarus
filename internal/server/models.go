package server

import (
	"time"
)

// InboundReport matches the JSON payload delivered by Lazarus CLI's generic/lazarus notifier.
type InboundReport struct {
	Passed    bool            `json:"passed"`
	Total     int             `json:"total"`
	Failed    int             `json:"failed"`
	Hostname  string          `json:"hostname,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	Results   []InboundResult `json:"results"`
}

type InboundResult struct {
	Target            string         `json:"target"`
	Tags              []string       `json:"tags,omitempty"`
	Passed            bool           `json:"passed"`
	Stage             string         `json:"stage"`
	Error             string         `json:"error,omitempty"`
	BackupPath        string         `json:"backup_path,omitempty"`
	BackupSize        int64          `json:"backup_size,omitempty"`
	BackupSizeHuman   string         `json:"backup_size_human,omitempty"`
	BackupAge         string         `json:"backup_age,omitempty"`
	DurationMs        int64          `json:"duration_ms"`
	RestoreDurationMs int64          `json:"restore_duration_ms,omitempty"`
	SLARTOMs          int64          `json:"sla_rto_ms,omitempty"`
	Checks            []InboundCheck `json:"checks,omitempty"`
	DebugHint                 string             `json:"debug_hint,omitempty"`
	LogsTail                  string             `json:"logs_tail,omitempty"`
	PeakMemoryBytes           int64              `json:"peak_memory_bytes,omitempty"`
	DiskFootprintBytes        int64              `json:"disk_footprint_bytes,omitempty"`
	IncrementalPatchesApplied []string           `json:"incremental_patches_applied,omitempty"`
	Remediation               *RemediationRecord `json:"remediation,omitempty"`
	HasRPOCheck               bool               `json:"has_rpo_check,omitempty"`
	MaxRPOLagSec              float64            `json:"max_rpo_lag_seconds,omitempty"`
	RPOViolated               bool               `json:"rpo_violated,omitempty"`
}

// RemediationRecord describes the outcome of an automated disaster recovery remediation playbook.
type RemediationRecord struct {
	Triggered  bool   `json:"triggered"`
	Command    string `json:"command"`
	Success    bool   `json:"success"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type InboundCheck struct {
	Name       string  `json:"name"`
	Passed     bool    `json:"passed"`
	Value      int64   `json:"value"`
	Reason     string  `json:"reason,omitempty"`
	IsRPOCheck bool    `json:"is_rpo_check,omitempty"`
	RPOLagSec  float64 `json:"rpo_lag_seconds,omitempty"`
}

// TargetStatus is the calculated health status of a database target.
type TargetStatus string

const (
	StatusHealthy TargetStatus = "healthy"
	StatusFailed  TargetStatus = "failed"
	StatusOverdue TargetStatus = "overdue"
	StatusMuted   TargetStatus = "muted"
)

// TargetRecord represents a tracked database target and its most recent drill result.
type TargetRecord struct {
	Name           string          `json:"name"`
	Tags           []string        `json:"tags,omitempty"`
	Status         TargetStatus    `json:"status"`
	Muted          bool            `json:"muted"`
	TriggerPending bool            `json:"trigger_pending,omitempty"`
	LastPassed     bool            `json:"last_passed"`
	LastDrilledAt  time.Time       `json:"last_drilled_at"`
	LastHostname   string          `json:"last_hostname,omitempty"`
	LastStage      string          `json:"last_stage,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	LastBackupPath string          `json:"last_backup_path,omitempty"`
	LastBackupSize string          `json:"last_backup_size,omitempty"`
	LastBackupAge  string          `json:"last_backup_age,omitempty"`
	LastDurationMs int64           `json:"last_duration_ms"`
	LastRestoreMs  int64           `json:"last_restore_ms"`
	LastLogsTail   string          `json:"last_logs_tail,omitempty"`
	SLARTO         string          `json:"sla_rto,omitempty"`
	SLARTOMs       int64           `json:"sla_rto_ms,omitempty"`
	// SLAPassed is derived when targets are listed (never persisted as a
	// stale verdict): healthy now AND last restore within the RTO, if set.
	SLAPassed bool `json:"sla_passed"`
	LastChecks                []InboundCheck     `json:"last_checks,omitempty"`
	PeakMemoryBytes           int64              `json:"peak_memory_bytes,omitempty"`
	DiskFootprintBytes        int64              `json:"disk_footprint_bytes,omitempty"`
	IncrementalPatchesApplied []string           `json:"incremental_patches_applied,omitempty"`
	Remediation               *RemediationRecord `json:"remediation,omitempty"`
	HasRPOCheck               bool               `json:"has_rpo_check,omitempty"`
	MaxRPOLagSec              float64            `json:"max_rpo_lag_seconds,omitempty"`
	RPOViolated               bool               `json:"rpo_violated,omitempty"`
	RecentHistory             []HistoryRecord    `json:"recent_history,omitempty"`
}

type HistoryRecord struct {
	Target                    string             `json:"target,omitempty"`
	Tags                      []string           `json:"tags,omitempty"`
	DrilledAt                 time.Time          `json:"drilled_at"`
	Passed                    bool               `json:"passed"`
	Stage                     string             `json:"stage,omitempty"`
	BackupPath                string             `json:"backup_path,omitempty"`
	BackupSize                string             `json:"backup_size,omitempty"`
	RestoreMs                 int64              `json:"restore_ms"`
	TotalDuration             int64              `json:"total_duration_ms"`
	ChecksTotal               int                `json:"checks_total,omitempty"`
	ChecksPassed              int                `json:"checks_passed,omitempty"`
	ChecksFailed              int                `json:"checks_failed,omitempty"`
	Error                     string             `json:"error,omitempty"`
	LogsTail                  string             `json:"logs_tail,omitempty"`
	PeakMemoryBytes           int64              `json:"peak_memory_bytes,omitempty"`
	DiskFootprintBytes        int64              `json:"disk_footprint_bytes,omitempty"`
	IncrementalPatchesApplied []string           `json:"incremental_patches_applied,omitempty"`
	Remediation               *RemediationRecord `json:"remediation,omitempty"`
	HasRPOCheck               bool               `json:"has_rpo_check,omitempty"`
	MaxRPOLagSec              float64            `json:"max_rpo_lag_seconds,omitempty"`
	RPOViolated               bool               `json:"rpo_violated,omitempty"`
}

type Summary struct {
	TotalTargets    int     `json:"total_targets"`
	Healthy         int     `json:"healthy"`
	Failed          int     `json:"failed"`
	Overdue         int     `json:"overdue"`
	Muted           int     `json:"muted"`
	SLAPercentage   float64 `json:"sla_percentage"`
	AvgRestoreMs    int64   `json:"avg_restore_ms"`    // Mean Time to Restore (MTTR)
	TotalDrills30d  int     `json:"total_drills_30d"`  // Total drills in the last 30 days
	PassedDrills30d int     `json:"passed_drills_30d"` // Successful drills in the last 30 days
}

// DailyMetric aggregates verification drill volume, pass rate, and MTTR for a single calendar day.
type DailyMetric struct {
	Date         string  `json:"date"` // YYYY-MM-DD
	TotalDrills  int     `json:"total_drills"`
	PassedDrills int     `json:"passed_drills"`
	FailedDrills int     `json:"failed_drills"`
	AvgRestoreMs int64   `json:"avg_restore_ms"`
	SuccessRate  float64 `json:"success_rate"`
}
