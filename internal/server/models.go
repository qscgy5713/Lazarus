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
	Passed            bool           `json:"passed"`
	Stage             string         `json:"stage"`
	Error             string         `json:"error,omitempty"`
	BackupPath        string         `json:"backup_path,omitempty"`
	BackupSize        int64          `json:"backup_size,omitempty"`
	BackupSizeHuman   string         `json:"backup_size_human,omitempty"`
	BackupAge         string         `json:"backup_age,omitempty"`
	DurationMs        int64          `json:"duration_ms"`
	RestoreDurationMs int64          `json:"restore_duration_ms,omitempty"`
	Checks            []InboundCheck `json:"checks,omitempty"`
	DebugHint         string         `json:"debug_hint,omitempty"`
}

type InboundCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Value  int64  `json:"value"`
	Reason string `json:"reason,omitempty"`
}

// TargetStatus is the calculated health status of a database target.
type TargetStatus string

const (
	StatusHealthy TargetStatus = "healthy"
	StatusFailed  TargetStatus = "failed"
	StatusOverdue TargetStatus = "overdue"
)

// TargetRecord represents a tracked database target and its most recent drill result.
type TargetRecord struct {
	Name              string          `json:"name"`
	Status            TargetStatus    `json:"status"`
	LastPassed        bool            `json:"last_passed"`
	LastDrilledAt     time.Time       `json:"last_drilled_at"`
	LastHostname      string          `json:"last_hostname,omitempty"`
	LastStage         string          `json:"last_stage,omitempty"`
	LastError         string          `json:"last_error,omitempty"`
	LastBackupPath    string          `json:"last_backup_path,omitempty"`
	LastBackupSize    string          `json:"last_backup_size,omitempty"`
	LastBackupAge     string          `json:"last_backup_age,omitempty"`
	LastDurationMs    int64           `json:"last_duration_ms"`
	LastRestoreMs     int64           `json:"last_restore_ms"`
	LastChecks        []InboundCheck  `json:"last_checks,omitempty"`
	RecentHistory     []HistoryRecord `json:"recent_history,omitempty"`
}

type HistoryRecord struct {
	DrilledAt     time.Time `json:"drilled_at"`
	Passed        bool      `json:"passed"`
	RestoreMs     int64     `json:"restore_ms"`
	TotalDuration int64     `json:"total_duration_ms"`
	Error         string    `json:"error,omitempty"`
}

type Summary struct {
	TotalTargets int `json:"total_targets"`
	Healthy      int `json:"healthy"`
	Failed       int `json:"failed"`
	Overdue      int `json:"overdue"`
}
