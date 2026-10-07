package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu               sync.RWMutex
	filePath         string
	overdueThreshold time.Duration
	historyLimit     int
	retention        time.Duration
	targets          map[string]*TargetRecord
	reports          []InboundReport
	workers          map[string]*WorkerRecord
	audit            []AuditEvent
	// started lets RequeueExpiredClaims give workers one heartbeat window
	// to re-register after a restart (the worker registry isn't persisted).
	started time.Time
}

type persistedState struct {
	Targets []*TargetRecord `json:"targets"`
	Reports []InboundReport `json:"reports,omitempty"`
	Audit   []AuditEvent    `json:"audit,omitempty"`
}

// workerOfflineAfter is how long without a heartbeat before a worker is
// considered offline (workers heartbeat every 10s).
const workerOfflineAfter = 60 * time.Second

const (
	defaultHistoryLimit = 1000
	defaultRetention    = 400 * 24 * time.Hour
	// uiHistoryLen is how much history /api/v1/targets returns per target;
	// the full retained history is served by the CSV export.
	uiHistoryLen = 20
	maxReports   = 50
	maxAudit     = 20000
)

func NewStore(filePath string, overdueThreshold time.Duration) *Store {
	return NewStoreWithRetention(filePath, overdueThreshold, 0, 0)
}

// NewStoreWithRetention is NewStore with explicit history caps; zero values
// pick the defaults (1000 records per target, 400 days).
func NewStoreWithRetention(filePath string, overdueThreshold time.Duration, historyLimit int, retention time.Duration) *Store {
	if overdueThreshold <= 0 {
		overdueThreshold = 26 * time.Hour
	}
	if historyLimit <= 0 {
		historyLimit = defaultHistoryLimit
	}
	if retention <= 0 {
		retention = defaultRetention
	}
	s := &Store{
		filePath:         filePath,
		overdueThreshold: overdueThreshold,
		historyLimit:     historyLimit,
		retention:        retention,
		targets:          make(map[string]*TargetRecord),
		reports:          make([]InboundReport, 0),
		workers:          make(map[string]*WorkerRecord),
		started:          time.Now(),
	}
	s.load()
	return s
}

func (s *Store) RecordReport(report InboundReport) {
	s.RecordReportBy(report, "")
}

// RecordReportBy records a drill report submitted by reporter.
func (s *Store) RecordReportBy(report InboundReport, reporter string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if report.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, report.Timestamp); err == nil {
			now = t
		}
	}

	s.reports = append(s.reports, report)
	if len(s.reports) > maxReports {
		s.reports = s.reports[len(s.reports)-maxReports:]
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

		if len(res.Tags) > 0 {
			rec.Tags = res.Tags
		}

		// The report closes any lease a worker held on this target.
		rec.ClaimedBy = ""
		rec.ClaimedAt = time.Time{}

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
		rec.LastLogsTail = res.LogsTail
		rec.PeakMemoryBytes = res.PeakMemoryBytes
		rec.DiskFootprintBytes = res.DiskFootprintBytes
		rec.IncrementalPatchesApplied = res.IncrementalPatchesApplied
		rec.Remediation = res.Remediation
		rec.HasRPOCheck = res.HasRPOCheck
		rec.MaxRPOLagSec = res.MaxRPOLagSec
		rec.RPOViolated = res.RPOViolated
		rec.SLARTOMs = res.SLARTOMs
		rec.SLARTO = ""
		if res.SLARTOMs > 0 {
			rec.SLARTO = (time.Duration(res.SLARTOMs) * time.Millisecond).String()
		}

		if res.Passed {
			rec.Status = StatusHealthy
		} else {
			rec.Status = StatusFailed
		}

		checksTotal := len(res.Checks)
		checksPassed := 0
		checksFailed := 0
		for _, c := range res.Checks {
			if c.Passed {
				checksPassed++
			} else {
				checksFailed++
			}
		}

		// Append to history, keeping last 20
		rec.RecentHistory = append(rec.RecentHistory, HistoryRecord{
			Target:                    res.Target,
			Tags:                      res.Tags,
			DrilledAt:                 now,
			Passed:                    res.Passed,
			Stage:                     res.Stage,
			BackupPath:                res.BackupPath,
			BackupSize:                res.BackupSizeHuman,
			RestoreMs:                 res.RestoreDurationMs,
			TotalDuration:             res.DurationMs,
			ChecksTotal:               checksTotal,
			ChecksPassed:              checksPassed,
			ChecksFailed:              checksFailed,
			Error:                     res.Error,
			LogsTail:                  res.LogsTail,
			PeakMemoryBytes:           res.PeakMemoryBytes,
			DiskFootprintBytes:        res.DiskFootprintBytes,
			IncrementalPatchesApplied: res.IncrementalPatchesApplied,
			Remediation:               res.Remediation,
			HasRPOCheck:               res.HasRPOCheck,
			MaxRPOLagSec:              res.MaxRPOLagSec,
			RPOViolated:               res.RPOViolated,
			ReportedBy:                reporter,
		})
		s.pruneHistory(rec)
	}

	s.save()
}

// pruneHistory applies the count and age limits, and drops log tails from
// all but the newest uiHistoryLen records so a year of history stays small.
func (s *Store) pruneHistory(rec *TargetRecord) {
	cutoff := time.Now().Add(-s.retention)
	h := rec.RecentHistory
	start := 0
	for start < len(h) && h[start].DrilledAt.Before(cutoff) {
		start++
	}
	if len(h)-start > s.historyLimit {
		start = len(h) - s.historyLimit
	}
	h = h[start:]
	for i := 0; i < len(h)-uiHistoryLen; i++ {
		h[i].LogsTail = ""
	}
	rec.RecentHistory = h
}

// RecordAudit appends an operator action to the persisted audit log.
func (s *Store) RecordAudit(actor Principal, action, target, remoteAddr, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordAuditLocked(AuditEvent{
		Time:       time.Now().UTC(),
		Actor:      actor.Name,
		Role:       actor.Role,
		Action:     action,
		Target:     target,
		RemoteAddr: remoteAddr,
		Detail:     detail,
	})
	s.save()
}

func (s *Store) recordAuditLocked(e AuditEvent) {
	s.audit = append(s.audit, e)
	s.pruneAuditLocked()
}

func (s *Store) pruneAuditLocked() {
	cutoff := time.Now().Add(-s.retention)
	start := 0
	for start < len(s.audit) && s.audit[start].Time.Before(cutoff) {
		start++
	}
	if len(s.audit)-start > maxAudit {
		start = len(s.audit) - maxAudit
	}
	s.audit = s.audit[start:]
}

// GetAuditEvents returns the newest events first; limit <= 0 means all.
func (s *Store) GetAuditEvents(limit int) []AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.audit)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]AuditEvent, 0, n)
	for i := len(s.audit) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.audit[i])
	}
	return out
}

func (s *Store) SetTargetMuted(name string, muted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.targets[name]
	if !exists {
		return fmt.Errorf("target %q not found", name)
	}
	rec.Muted = muted
	s.save()
	return nil
}

func (s *Store) TriggerTarget(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, exists := s.targets[name]
	if !exists {
		return fmt.Errorf("target %q not found", name)
	}
	rec.TriggerPending = true
	s.save()
	return nil
}

// RegisterWorker adds or refreshes a worker and reports whether it is new.
func (s *Store) RegisterWorker(w WorkerRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.LastHeartbeat = time.Now().UTC()
	if w.Status == "" {
		w.Status = WorkerStatusOnline
	}
	_, existed := s.workers[w.ID]
	s.workers[w.ID] = &w
	return !existed
}

func (s *Store) HeartbeatWorker(id string, currentTask string, status WorkerStatus) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, exists := s.workers[id]
	if !exists {
		return false
	}
	w.LastHeartbeat = time.Now().UTC()
	// A heartbeat carries the worker's full current state, so an empty task
	// means the previous drill finished and must be cleared.
	w.CurrentTask = currentTask
	if status != "" {
		w.Status = status
	} else if w.CurrentTask != "" {
		w.Status = WorkerStatusBusy
	} else {
		w.Status = WorkerStatusOnline
	}
	return true
}

func (s *Store) GetWorkers() []WorkerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	out := make([]WorkerRecord, 0, len(s.workers))
	for _, w := range s.workers {
		if now.Sub(w.LastHeartbeat) > workerOfflineAfter {
			w.Status = WorkerStatusOffline
			w.CurrentTask = ""
		}
		out = append(out, *w)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// ClaimPendingTarget hands one triggered target to a polling worker. When
// targetNames is non-empty only those targets are eligible — a worker can
// only run targets present in its own config, and claiming one it lacks
// would silently drop the trigger. Otherwise workerTags (if any) must
// intersect the target's tags.
func (s *Store) ClaimPendingTarget(workerID string, workerTags, targetNames []string) (*TargetRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tagSet := make(map[string]bool)
	for _, t := range workerTags {
		tagSet[strings.ToLower(strings.TrimSpace(t))] = true
	}
	nameSet := make(map[string]bool, len(targetNames))
	for _, n := range targetNames {
		nameSet[n] = true
	}

	for _, rec := range s.targets {
		if !rec.TriggerPending {
			continue
		}
		if len(nameSet) > 0 {
			if !nameSet[rec.Name] {
				continue
			}
		} else if len(tagSet) > 0 {
			matched := false
			for _, t := range rec.Tags {
				if tagSet[strings.ToLower(strings.TrimSpace(t))] {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		rec.TriggerPending = false
		rec.ClaimedBy = workerID
		rec.ClaimedAt = time.Now().UTC()
		s.save()
		cloned := *rec
		return &cloned, true
	}

	return nil, false
}

// RequeueExpiredClaims puts a claimed drill back in the queue when its
// worker stopped heartbeating (crashed mid-drill) or the lease ran past
// leaseTimeout. Without this a lost worker silently swallows the trigger.
// It returns the requeued target names.
func (s *Store) RequeueExpiredClaims(leaseTimeout time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	var requeued []string
	for _, rec := range s.targets {
		if rec.ClaimedBy == "" && rec.ClaimedAt.IsZero() {
			continue
		}
		reason := ""
		w, known := s.workers[rec.ClaimedBy]
		switch {
		case leaseTimeout > 0 && now.Sub(rec.ClaimedAt) > leaseTimeout:
			reason = "lease timed out"
		case rec.ClaimedBy == "":
			// Claimed by a poller that didn't identify itself: there's no
			// heartbeat to watch, so only the lease timeout applies.
		case !known:
			if now.Sub(s.started) > workerOfflineAfter {
				reason = "worker unknown"
			}
		case now.Sub(w.LastHeartbeat) > workerOfflineAfter:
			reason = "worker offline"
		}
		if reason == "" {
			continue
		}
		s.recordAuditLocked(AuditEvent{
			Time: now, Actor: "system", Action: "requeue", Target: rec.Name,
			Detail: fmt.Sprintf("claimed by %q: %s", rec.ClaimedBy, reason),
		})
		rec.ClaimedBy = ""
		rec.ClaimedAt = time.Time{}
		rec.TriggerPending = true
		requeued = append(requeued, rec.Name)
	}
	if len(requeued) > 0 {
		s.save()
	}
	sort.Strings(requeued)
	return requeued
}

func hasTag(tags []string, targetTag string) bool {
	if targetTag == "" {
		return true
	}
	targetTag = strings.ToLower(strings.TrimSpace(targetTag))
	for _, t := range tags {
		if strings.ToLower(strings.TrimSpace(t)) == targetTag {
			return true
		}
	}
	return false
}

func (s *Store) GetTargetsFiltered(tagFilter string) []*TargetRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetsFilteredLocked(tagFilter)
}

// targetsFilteredLocked requires s.mu to be held (read or write).
func (s *Store) targetsFilteredLocked(tagFilter string) []*TargetRecord {
	now := time.Now().UTC()
	out := make([]*TargetRecord, 0, len(s.targets))

	for _, rec := range s.targets {
		if tagFilter != "" && !hasTag(rec.Tags, tagFilter) {
			continue
		}
		// Clone record to prevent mutating internal pointer
		cloned := *rec
		if n := len(cloned.RecentHistory); n > uiHistoryLen {
			cloned.RecentHistory = cloned.RecentHistory[n-uiHistoryLen:]
		}
		if rec.Muted {
			cloned.Status = StatusMuted
		} else if s.overdueThreshold > 0 && now.Sub(rec.LastDrilledAt) > s.overdueThreshold {
			// Dead Man's Snitch check: if older than threshold, mark as Overdue
			cloned.Status = StatusOverdue
		}
		// SLA compliance: still healthy (not failed/overdue) and, when an RTO
		// is configured, the last restore finished within it. Derived here so
		// it also holds for records persisted before sla_rto existed.
		cloned.SLAPassed = cloned.Status == StatusHealthy &&
			(cloned.SLARTOMs <= 0 || cloned.LastRestoreMs <= cloned.SLARTOMs)
		out = append(out, &cloned)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})

	return out
}

func (s *Store) GetTargets() []*TargetRecord {
	return s.GetTargetsFiltered("")
}

func (s *Store) GetSummary() Summary {
	// Hold the lock for the whole computation: the MTTR loop below walks
	// s.targets directly, which races with RecordReport otherwise.
	s.mu.RLock()
	defer s.mu.RUnlock()

	targets := s.targetsFilteredLocked("")
	var sum Summary
	sum.TotalTargets = len(targets)
	slaPassed := 0
	for _, t := range targets {
		switch t.Status {
		case StatusHealthy:
			sum.Healthy++
		case StatusFailed:
			sum.Failed++
		case StatusOverdue:
			sum.Overdue++
		case StatusMuted:
			sum.Muted++
		}
		if t.SLAPassed {
			slaPassed++
		}
	}
	// Muted targets are in planned maintenance, so they're left out of the
	// denominator rather than counted as SLA misses.
	if eligible := sum.TotalTargets - sum.Muted; eligible > 0 {
		sum.SLAPercentage = float64(slaPassed) / float64(eligible) * 100.0
	}

	// Compute 30-day MTTR (Mean Time to Restore) and drill volume
	var totalRestoreMs int64
	var restoreCount int64
	cutoff30d := time.Now().Add(-30 * 24 * time.Hour)

	for _, rec := range s.targets {
		for _, h := range rec.RecentHistory {
			if h.DrilledAt.After(cutoff30d) {
				sum.TotalDrills30d++
				if h.Passed {
					sum.PassedDrills30d++
				}
				if h.RestoreMs > 0 {
					totalRestoreMs += h.RestoreMs
					restoreCount++
				}
			}
		}
	}
	if restoreCount > 0 {
		sum.AvgRestoreMs = totalRestoreMs / restoreCount
	}

	return sum
}

func (s *Store) GetAuditHistory(targetFilter, statusFilter, tagFilter string, limit int) []HistoryRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var all []HistoryRecord
	for _, rec := range s.targets {
		if targetFilter != "" && rec.Name != targetFilter {
			continue
		}
		if tagFilter != "" && !hasTag(rec.Tags, tagFilter) {
			continue
		}
		for _, h := range rec.RecentHistory {
			if statusFilter == "passed" && !h.Passed {
				continue
			}
			if statusFilter == "failed" && h.Passed {
				continue
			}
			record := h
			if record.Target == "" {
				record.Target = rec.Name
			}
			if len(record.Tags) == 0 {
				record.Tags = rec.Tags
			}
			all = append(all, record)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].DrilledAt.After(all[j].DrilledAt)
	})

	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (s *Store) GetDailyMetrics(days int) []DailyMetric {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if days <= 0 {
		days = 30
	}

	type dayBucket struct {
		total     int
		passed    int
		failed    int
		restoreMs int64
		restoreN  int64
	}

	buckets := make(map[string]*dayBucket)
	now := time.Now().UTC()

	dateKeys := make([]string, days)
	for i := days - 1; i >= 0; i-- {
		t := now.AddDate(0, 0, -i)
		dateStr := t.Format("2006-01-02")
		dateKeys[days-1-i] = dateStr
		buckets[dateStr] = &dayBucket{}
	}

	cutoff := now.AddDate(0, 0, -days)
	for _, rec := range s.targets {
		for _, h := range rec.RecentHistory {
			hTime := h.DrilledAt.UTC()
			if hTime.After(cutoff) {
				dateStr := hTime.Format("2006-01-02")
				if b, ok := buckets[dateStr]; ok {
					b.total++
					if h.Passed {
						b.passed++
					} else {
						b.failed++
					}
					if h.RestoreMs > 0 {
						b.restoreMs += h.RestoreMs
						b.restoreN++
					}
				}
			}
		}
	}

	metrics := make([]DailyMetric, len(dateKeys))
	for i, dateStr := range dateKeys {
		b := buckets[dateStr]
		var avgRestore int64
		if b.restoreN > 0 {
			avgRestore = b.restoreMs / b.restoreN
		}
		var rate float64
		if b.total > 0 {
			rate = float64(b.passed) / float64(b.total) * 100.0
		}
		metrics[i] = DailyMetric{
			Date:         dateStr,
			TotalDrills:  b.total,
			PassedDrills: b.passed,
			FailedDrills: b.failed,
			AvgRestoreMs: avgRestore,
			SuccessRate:  rate,
		}
	}

	return metrics
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
		s.pruneHistory(t)
		s.targets[t.Name] = t
	}
	s.reports = state.Reports
	if len(s.reports) > maxReports {
		s.reports = s.reports[len(s.reports)-maxReports:]
	}
	s.audit = state.Audit
	s.pruneAuditLocked()
}

func (s *Store) save() {
	if s.filePath == "" {
		return
	}
	targetsList := make([]*TargetRecord, 0, len(s.targets))
	for _, t := range s.targets {
		targetsList = append(targetsList, t)
	}
	sort.Slice(targetsList, func(i, j int) bool { return targetsList[i].Name < targetsList[j].Name })
	state := persistedState{Targets: targetsList, Reports: s.reports, Audit: s.audit}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return
		}
	} else {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, ".lazarus-server-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Chmod(tmpPath, 0644)
	_ = os.Rename(tmpPath, s.filePath)
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
					Tags:              []string{"prod", "aws", "postgres"},
					Passed:            true,
					Stage:             "done",
					BackupPath:        "/backups/postgres/shop-prod-latest.sql.gz",
					BackupSize:        18432000,
					BackupSizeHuman:   "17.6 MB",
					BackupAge:         "2h15m",
					DurationMs:        10240,
					RestoreDurationMs: 7820,
					HasRPOCheck:       true,
					MaxRPOLagSec:      900.0,
					RPOViolated:       false,
					Checks: []InboundCheck{
						{Name: "users table is populated", Passed: true, Value: 1841},
						{Name: "orders from last week made it in", Passed: true, Value: 327, IsRPOCheck: true, RPOLagSec: 900.0},
					},
				},
				{
					Target:            "analytics-mysql",
					Tags:              []string{"analytics", "gcp", "mysql"},
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
					Tags:              []string{"staging", "sqlite"},
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
					Tags:              []string{"archive", "legacy"},
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

	s.RegisterWorker(WorkerRecord{
		ID:       "runner-aws-us-east-1",
		Hostname: "worker-ec2-01.us-east-1.internal",
		Version:  "1.4.0",
		Tags:     []string{"prod", "aws", "postgres"},
		Status:   WorkerStatusOnline,
	})
	s.RegisterWorker(WorkerRecord{
		ID:       "runner-gcp-europe-west3",
		Hostname: "worker-gce-02.europe-west3.c.project.internal",
		Version:  "1.4.0",
		Tags:     []string{"analytics", "gcp", "mysql"},
		Status:   WorkerStatusOnline,
	})
	s.RegisterWorker(WorkerRecord{
		ID:          "runner-k8s-staging",
		Hostname:    "lazarus-runner-7b9c6f8f4-xj2kl",
		Version:     "1.4.0",
		Tags:        []string{"staging", "sqlite"},
		Status:      WorkerStatusBusy,
		CurrentTask: "staging-sqlite",
	})
}
