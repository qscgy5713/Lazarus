package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Store struct {
	mu               sync.RWMutex
	filePath         string
	overdueThreshold time.Duration
	targets          map[string]*TargetRecord
	reports          []InboundReport
}

type persistedState struct {
	Targets []*TargetRecord `json:"targets"`
}

func NewStore(filePath string, overdueThreshold time.Duration) *Store {
	if overdueThreshold <= 0 {
		overdueThreshold = 26 * time.Hour
	}
	s := &Store{
		filePath:         filePath,
		overdueThreshold: overdueThreshold,
		targets:          make(map[string]*TargetRecord),
		reports:          make([]InboundReport, 0),
	}
	s.load()
	return s
}

func (s *Store) RecordReport(report InboundReport) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if report.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, report.Timestamp); err == nil {
			now = t
		}
	}

	s.reports = append(s.reports, report)
	if len(s.reports) > 50 {
		s.reports = s.reports[len(s.reports)-50:]
	}

	for _, res := range report.Results {
		rec, exists := s.targets[res.Target]
		if !exists {
			rec = &TargetRecord{
				Name:          res.Target,
				RecentHistory: make([]HistoryRecord, 0),
			}
			s.targets[res.Target] = rec
		}

		rec.LastPassed = res.Passed
		rec.LastDrilledAt = now
		rec.LastHostname = report.Hostname
		rec.LastStage = res.Stage
		rec.LastError = res.Error
		rec.LastBackupPath = res.BackupPath
		rec.LastBackupSize = res.BackupSizeHuman
		rec.LastBackupAge = res.BackupAge
		rec.LastDurationMs = res.DurationMs
		rec.LastRestoreMs = res.RestoreDurationMs
		rec.LastChecks = res.Checks

		if res.Passed {
			rec.Status = StatusHealthy
		} else {
			rec.Status = StatusFailed
		}

		// Append to history, keeping last 20
		rec.RecentHistory = append(rec.RecentHistory, HistoryRecord{
			DrilledAt:     now,
			Passed:        res.Passed,
			RestoreMs:     res.RestoreDurationMs,
			TotalDuration: res.DurationMs,
			Error:         res.Error,
		})
		if len(rec.RecentHistory) > 20 {
			rec.RecentHistory = rec.RecentHistory[len(rec.RecentHistory)-20:]
		}
	}

	s.save()
}

func (s *Store) GetTargets() []*TargetRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UTC()
	out := make([]*TargetRecord, 0, len(s.targets))

	for _, rec := range s.targets {
		// Clone record to prevent mutating internal pointer
		cloned := *rec
		// Dead Man's Snitch check: if older than threshold, mark as Overdue
		if s.overdueThreshold > 0 && now.Sub(rec.LastDrilledAt) > s.overdueThreshold {
			cloned.Status = StatusOverdue
		}
		out = append(out, &cloned)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})

	return out
}

func (s *Store) GetSummary() Summary {
	targets := s.GetTargets()
	var sum Summary
	sum.TotalTargets = len(targets)
	for _, t := range targets {
		switch t.Status {
		case StatusHealthy:
			sum.Healthy++
		case StatusFailed:
			sum.Failed++
		case StatusOverdue:
			sum.Overdue++
		}
	}
	return sum
}

func (s *Store) GetRecentReports() []InboundReport {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]InboundReport, len(s.reports))
	copy(out, s.reports)
	// Return in reverse chronological order
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (s *Store) load() {
	if s.filePath == "" {
		return
	}
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	for _, t := range state.Targets {
		s.targets[t.Name] = t
	}
}

func (s *Store) save() {
	if s.filePath == "" {
		return
	}
	targetsList := make([]*TargetRecord, 0, len(s.targets))
	for _, t := range s.targets {
		targetsList = append(targetsList, t)
	}
	state := persistedState{Targets: targetsList}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	_ = os.WriteFile(s.filePath, data, 0644)
}

// SeedDemoData populates representative drill records (healthy, failed, and overdue) for demonstration.
func (s *Store) SeedDemoData() {
	now := time.Now().UTC()
	demoReports := []InboundReport{
		{
			Passed:    true,
			Total:     2,
			Failed:    0,
			Hostname:  "prod-backup-runner",
			Timestamp: now.Add(-15 * time.Minute).Format(time.RFC3339),
			Results: []InboundResult{
				{
					Target:            "production-postgres",
					Passed:            true,
					Stage:             "done",
					BackupPath:        "/backups/postgres/shop-prod-latest.sql.gz",
					BackupSize:        18432000,
					BackupSizeHuman:   "17.6 MB",
					BackupAge:         "2h15m",
					DurationMs:        10240,
					RestoreDurationMs: 7820,
					Checks: []InboundCheck{
						{Name: "users table is populated", Passed: true, Value: 1841},
						{Name: "orders from last week made it in", Passed: true, Value: 327},
					},
				},
				{
					Target:            "analytics-mysql",
					Passed:            true,
					Stage:             "done",
					BackupPath:        "/backups/mysql/events-latest.sql",
					BackupSize:        45200000,
					BackupSizeHuman:   "43.1 MB",
					BackupAge:         "1h05m",
					DurationMs:        15400,
					RestoreDurationMs: 11300,
					Checks: []InboundCheck{
						{Name: "events table has records", Passed: true, Value: 98520},
					},
				},
			},
		},
		{
			Passed:    false,
			Total:     1,
			Failed:    1,
			Hostname:  "staging-drill-worker",
			Timestamp: now.Add(-45 * time.Minute).Format(time.RFC3339),
			Results: []InboundResult{
				{
					Target:            "staging-sqlite",
					Passed:            false,
					Stage:             "checks",
					Error:             "check \"tenants count\" failed: got 0, want at least 1",
					BackupPath:        "/backups/sqlite/staging.db",
					BackupSize:        1048576,
					BackupSizeHuman:   "1.0 MB",
					BackupAge:         "45m",
					DurationMs:        320,
					RestoreDurationMs: 45,
					Checks: []InboundCheck{
						{Name: "tenants count", Passed: false, Value: 0, Reason: "got 0, want at least 1"},
					},
				},
			},
		},
		{
			Passed:    true,
			Total:     1,
			Failed:    0,
			Hostname:  "archive-runner-legacy",
			Timestamp: now.Add(-72 * time.Hour).Format(time.RFC3339),
			Results: []InboundResult{
				{
					Target:            "legacy-archive-db",
					Passed:            true,
					Stage:             "done",
					BackupPath:        "/backups/postgres/archive-legacy.sql.gz",
					BackupSize:        85000000,
					BackupSizeHuman:   "81.0 MB",
					BackupAge:         "72h",
					DurationMs:        24000,
					RestoreDurationMs: 19500,
					Checks: []InboundCheck{
						{Name: "cold storage records count", Passed: true, Value: 504100},
					},
				},
			},
		},
	}

	for _, rep := range demoReports {
		s.RecordReport(rep)
	}
}
