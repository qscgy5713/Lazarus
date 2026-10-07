package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDailyMetricsEndpoint(t *testing.T) {
	s := New(Config{DemoMode: true})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/daily?days=7", nil)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var metrics []DailyMetric
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(metrics) != 7 {
		t.Fatalf("expected 7 days of metrics, got %d", len(metrics))
	}

	// Verify dates are ordered chronologically up to today
	nowStr := time.Now().UTC().Format("2006-01-02")
	if metrics[len(metrics)-1].Date != nowStr {
		t.Errorf("latest metric date = %s, want %s", metrics[len(metrics)-1].Date, nowStr)
	}
}

func TestStoreGetDailyMetricsCalculation(t *testing.T) {
	store := NewStore("", 26*time.Hour)

	// Ingest drill for today (pass, restore 2000ms)
	store.RecordReport(InboundReport{
		Passed: true, Total: 1, Failed: 0,
		Results: []InboundResult{
			{Target: "test-db", Passed: true, Stage: "done", RestoreDurationMs: 2000, DurationMs: 3000},
		},
	})

	// Ingest drill for today (fail, restore 4000ms)
	store.RecordReport(InboundReport{
		Passed: false, Total: 1, Failed: 1,
		Results: []InboundResult{
			{Target: "test-db", Passed: false, Stage: "checks", RestoreDurationMs: 4000, DurationMs: 5000},
		},
	})

	metrics := store.GetDailyMetrics(3)
	if len(metrics) != 3 {
		t.Fatalf("expected 3 days, got %d", len(metrics))
	}

	todayMetric := metrics[2]
	if todayMetric.TotalDrills != 2 {
		t.Errorf("today TotalDrills = %d, want 2", todayMetric.TotalDrills)
	}
	if todayMetric.PassedDrills != 1 {
		t.Errorf("today PassedDrills = %d, want 1", todayMetric.PassedDrills)
	}
	if todayMetric.FailedDrills != 1 {
		t.Errorf("today FailedDrills = %d, want 1", todayMetric.FailedDrills)
	}
	if todayMetric.AvgRestoreMs != 3000 {
		t.Errorf("today AvgRestoreMs = %d, want 3000", todayMetric.AvgRestoreMs)
	}
	if todayMetric.SuccessRate != 50.0 {
		t.Errorf("today SuccessRate = %.1f, want 50.0", todayMetric.SuccessRate)
	}
}
