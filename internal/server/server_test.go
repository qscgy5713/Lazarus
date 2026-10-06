package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	s := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestDashboard(t *testing.T) {
	s := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("Lazarus")) {
		t.Errorf("dashboard missing 'Lazarus' in body")
	}
}

func TestPostReportAndGetTargets(t *testing.T) {
	s := New(Config{})

	report := InboundReport{
		Passed:    true,
		Total:     2,
		Failed:    0,
		Hostname:  "test-worker",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Results: []InboundResult{
			{
				Target:            "postgres-db",
				Passed:            true,
				Stage:             "done",
				BackupPath:        "/backups/postgres.sql.gz",
				BackupSize:        1024,
				BackupSizeHuman:   "1.0 KB",
				DurationMs:        320,
				RestoreDurationMs: 150,
				Checks: []InboundCheck{
					{Name: "users count", Passed: true, Value: 100},
				},
			},
			{
				Target:            "mysql-db",
				Passed:            false,
				Stage:             "checks",
				Error:             "assertion failed",
				DurationMs:        500,
				RestoreDurationMs: 200,
			},
		},
	}

	body, _ := json.Marshal(report)
	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/reports", bytes.NewReader(body))
	recPost := httptest.NewRecorder()

	s.Handler().ServeHTTP(recPost, reqPost)

	if recPost.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/reports status = %d, want %d", recPost.Code, http.StatusOK)
	}

	// Verify GET /api/v1/targets
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/targets", nil)
	recGet := httptest.NewRecorder()

	s.Handler().ServeHTTP(recGet, reqGet)

	if recGet.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/targets status = %d, want %d", recGet.Code, http.StatusOK)
	}

	var targets []*TargetRecord
	if err := json.Unmarshal(recGet.Body.Bytes(), &targets); err != nil {
		t.Fatalf("unmarshal targets: %v", err)
	}

	if len(targets) != 2 {
		t.Fatalf("targets len = %d, want 2", len(targets))
	}

	// Verify target statuses
	for _, target := range targets {
		if target.Name == "postgres-db" {
			if target.Status != StatusHealthy {
				t.Errorf("postgres-db status = %s, want %s", target.Status, StatusHealthy)
			}
			if target.LastRestoreMs != 150 {
				t.Errorf("postgres-db restore_ms = %d, want 150", target.LastRestoreMs)
			}
		}
		if target.Name == "mysql-db" {
			if target.Status != StatusFailed {
				t.Errorf("mysql-db status = %s, want %s", target.Status, StatusFailed)
			}
		}
	}

	// Verify summary
	reqSum := httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	recSum := httptest.NewRecorder()

	s.Handler().ServeHTTP(recSum, reqSum)

	var sum Summary
	if err := json.Unmarshal(recSum.Body.Bytes(), &sum); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if sum.TotalTargets != 2 || sum.Healthy != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want total=2 healthy=1 failed=1", sum)
	}
}

func TestAuthProtection(t *testing.T) {
	s := New(Config{APIKey: "secret-key"})

	// Unauthorized request
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/reports", bytes.NewReader([]byte("{}")))
	rec1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without auth", rec1.Code)
	}

	// Authorized request via Bearer header
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/reports", bytes.NewReader([]byte(`{"results":[]}`)))
	req2.Header.Set("Authorization", "Bearer secret-key")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with Bearer auth", rec2.Code)
	}
}

func TestOverdueCalculation(t *testing.T) {
	// Overdue threshold of 100ms
	store := NewStore("", 100*time.Millisecond)

	pastTime := time.Now().UTC().Add(-500 * time.Millisecond).Format(time.RFC3339)
	store.RecordReport(InboundReport{
		Timestamp: pastTime,
		Results: []InboundResult{
			{Target: "db-stale", Passed: true, Stage: "done"},
		},
	})

	targets := store.GetTargets()
	if len(targets) != 1 {
		t.Fatalf("len(targets) = %d, want 1", len(targets))
	}
	if targets[0].Status != StatusOverdue {
		t.Errorf("status = %s, want %s (overdue)", targets[0].Status, StatusOverdue)
	}
}

func TestDemoMode(t *testing.T) {
	s := New(Config{DemoMode: true})
	targets := s.store.GetTargets()
	if len(targets) == 0 {
		t.Fatalf("expected seeded demo targets, got 0")
	}

	sum := s.store.GetSummary()
	if sum.TotalTargets == 0 || sum.Healthy == 0 || sum.Failed == 0 || sum.Overdue == 0 {
		t.Errorf("demo mode should seed healthy, failed, and overdue targets; got %+v", sum)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := New(Config{DemoMode: true})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	expectedMetrics := []string{
		"lazarus_targets_total{status=\"healthy\"}",
		"lazarus_targets_total{status=\"failed\"}",
		"lazarus_targets_total{status=\"overdue\"}",
		"lazarus_target_status{target=",
		"lazarus_target_last_drill_timestamp_seconds{target=",
		"lazarus_target_restore_duration_seconds{target=",
		"lazarus_target_duration_seconds{target=",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics body missing expected metric prefix %q; body:\n%s", metric, body)
		}
	}
}

func TestMuteAndUnmuteTarget(t *testing.T) {
	s := New(Config{})

	// Post report
	report := InboundReport{
		Passed: true,
		Total:  1,
		Results: []InboundResult{
			{Target: "db-mute-test", Passed: true, Stage: "done"},
		},
	}
	body, _ := json.Marshal(report)
	reqPost := httptest.NewRequest(http.MethodPost, "/api/v1/reports", bytes.NewReader(body))
	recPost := httptest.NewRecorder()
	s.Handler().ServeHTTP(recPost, reqPost)

	// Mute target
	reqMute := httptest.NewRequest(http.MethodPost, "/api/v1/targets/db-mute-test/mute", nil)
	recMute := httptest.NewRecorder()
	s.Handler().ServeHTTP(recMute, reqMute)

	if recMute.Code != http.StatusOK {
		t.Fatalf("Mute target code = %d, want 200", recMute.Code)
	}

	targets := s.store.GetTargets()
	if len(targets) != 1 || !targets[0].Muted || targets[0].Status != StatusMuted {
		t.Fatalf("expected target to be muted with StatusMuted, got %+v", targets[0])
	}

	sum := s.store.GetSummary()
	if sum.Muted != 1 || sum.Healthy != 0 {
		t.Fatalf("expected summary muted=1 healthy=0, got %+v", sum)
	}

	// Unmute target
	reqUnmute := httptest.NewRequest(http.MethodPost, "/api/v1/targets/db-mute-test/unmute", nil)
	recUnmute := httptest.NewRecorder()
	s.Handler().ServeHTTP(recUnmute, reqUnmute)

	if recUnmute.Code != http.StatusOK {
		t.Fatalf("Unmute target code = %d, want 200", recUnmute.Code)
	}

	targets = s.store.GetTargets()
	if targets[0].Muted || targets[0].Status != StatusHealthy {
		t.Fatalf("expected target to be unmuted with StatusHealthy, got %+v", targets[0])
	}
}

func TestExportCSV(t *testing.T) {
	s := New(Config{DemoMode: true})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/export/csv", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("export csv code = %d, want 200", rec.Code)
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/csv") {
		t.Errorf("content-type = %s, want text/csv", contentType)
	}

	body := rec.Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected header + at least 1 row in CSV, got: %s", body)
	}

	// First line should be header
	if !strings.Contains(lines[0], "Target,DrilledAt,Passed") {
		t.Errorf("header = %s, missing required columns", lines[0])
	}
}

func TestCheckAndSendAlerts(t *testing.T) {
	alertCount := 0
	serverWebhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alertCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer serverWebhook.Close()

	s := New(Config{
		OverdueThreshold: 100 * time.Millisecond,
		AlertWebhookURL:  serverWebhook.URL,
	})

	// Add an overdue target
	pastTime := time.Now().UTC().Add(-500 * time.Millisecond).Format(time.RFC3339)
	s.store.RecordReport(InboundReport{
		Timestamp: pastTime,
		Results: []InboundResult{
			{Target: "db-overdue-alert", Passed: true, Stage: "done"},
		},
	})

	// Run alert check
	s.CheckAndSendAlerts()

	if alertCount != 1 {
		t.Fatalf("expected 1 alert sent, got %d", alertCount)
	}

	// Immediate second check should not re-alert (throttling)
	s.CheckAndSendAlerts()
	if alertCount != 1 {
		t.Fatalf("expected throttled alert, count stayed at 1, got %d", alertCount)
	}

	// If muted, should not alert
	_ = s.store.SetTargetMuted("db-overdue-alert", true)
	s.alertMu.Lock()
	delete(s.lastAlerted, "db-overdue-alert")
	s.alertMu.Unlock()

	s.CheckAndSendAlerts()
	if alertCount != 1 {
		t.Fatalf("muted target should not send alert, got %d", alertCount)
	}
}
