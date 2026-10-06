package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
