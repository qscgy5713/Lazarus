package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func doReq(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRBACEndpointMatrix(t *testing.T) {
	s := New(Config{AdminKey: "adm", ViewerKey: "view", DemoMode: true})
	report := InboundReport{Results: []InboundResult{{Target: "x", Passed: true}}}

	cases := []struct {
		method, path, token string
		body                any
		want                int
	}{
		{"GET", "/api/v1/targets", "", nil, http.StatusUnauthorized},
		{"GET", "/api/v1/reports", "", nil, http.StatusUnauthorized},
		{"GET", "/api/v1/summary", "", nil, http.StatusUnauthorized},
		{"GET", "/api/v1/export/csv", "", nil, http.StatusUnauthorized},
		{"GET", "/api/v1/export/certificate.pdf", "", nil, http.StatusUnauthorized},
		{"GET", "/api/v1/targets", "view", nil, http.StatusOK},
		{"GET", "/api/v1/export/csv?api_key=view", "", nil, http.StatusOK},
		{"GET", "/api/v1/export/certificate.pdf", "view", nil, http.StatusOK},
		// A read-only viewer must not forge audit evidence or act as a worker.
		{"POST", "/api/v1/reports", "view", report, http.StatusForbidden},
		{"POST", "/api/v1/workers/register", "view", map[string]string{"id": "w"}, http.StatusForbidden},
		{"POST", "/api/v1/workers/poll", "view", map[string]any{}, http.StatusForbidden},
		{"POST", "/api/v1/reports", "adm", report, http.StatusOK},
		{"POST", "/api/v1/workers/register", "adm", map[string]string{"id": "w"}, http.StatusOK},
		// Health and Prometheus scraping stay open.
		{"GET", "/healthz", "", nil, http.StatusOK},
		{"GET", "/metrics", "", nil, http.StatusOK},
	}
	for _, c := range cases {
		rec := doReq(t, s, c.method, c.path, c.token, c.body)
		if rec.Code != c.want {
			t.Errorf("%s %s (token %q) = %d, want %d", c.method, c.path, c.token, rec.Code, c.want)
		}
	}
}

func TestAuthMeReportsAuthRequiredForLegacyAPIKey(t *testing.T) {
	s := New(Config{APIKey: "legacy"})
	var got AuthStatusResponse
	_ = json.Unmarshal(doReq(t, s, "GET", "/api/v1/auth/me", "", nil).Body.Bytes(), &got)
	if !got.AuthRequired || got.Role != RoleAnonymous {
		t.Fatalf("auth/me without token = %+v, want auth_required=true role=anonymous", got)
	}
	_ = json.Unmarshal(doReq(t, s, "GET", "/api/v1/auth/me", "legacy", nil).Body.Bytes(), &got)
	if got.Role != RoleAdmin {
		t.Fatalf("legacy api key role = %s, want admin", got.Role)
	}
}

func TestDailyMetricsDaysIsCapped(t *testing.T) {
	s := New(Config{})
	rec := doReq(t, s, "GET", "/api/v1/metrics/daily?days=100000000", "", nil)
	var metrics []DailyMetric
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatal(err)
	}
	if len(metrics) != maxDailyMetricsDays {
		t.Fatalf("got %d days, want cap %d", len(metrics), maxDailyMetricsDays)
	}
}

// Run with -race: GetSummary used to walk s.targets after releasing the lock.
func TestGetSummaryConcurrentWithRecordReport(t *testing.T) {
	st := NewStore("", 0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			st.RecordReport(InboundReport{Results: []InboundResult{{Target: fmt.Sprint("t", i), Passed: true, RestoreDurationMs: 5}}})
		}(i)
		go func() {
			defer wg.Done()
			_ = st.GetSummary()
		}()
	}
	wg.Wait()
	if got := st.GetSummary().TotalTargets; got != 50 {
		t.Fatalf("TotalTargets = %d, want 50", got)
	}
}

func TestHeartbeatClearsFinishedTask(t *testing.T) {
	st := NewStore("", 0)
	st.RegisterWorker(WorkerRecord{ID: "w1"})
	st.HeartbeatWorker("w1", "orders-db", WorkerStatusBusy)
	st.HeartbeatWorker("w1", "", WorkerStatusOnline)
	w := st.GetWorkers()[0]
	if w.CurrentTask != "" || w.Status != WorkerStatusOnline {
		t.Fatalf("after finishing, worker = %+v, want online with no task", w)
	}
}

func TestRegisterWorkerRejectsUnknownStatus(t *testing.T) {
	s := New(Config{})
	rec := doReq(t, s, "POST", "/api/v1/workers/register", "", map[string]string{"id": "w", "status": "<img src=x onerror=alert(1)>"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestClaimPendingTargetOnlyClaimsWorkerTargets(t *testing.T) {
	st := NewStore("", 0)
	st.RecordReport(InboundReport{Results: []InboundResult{{Target: "orders"}, {Target: "billing"}}})
	_ = st.TriggerTarget("orders")

	// A worker without tags used to claim any pending target, then drop it
	// because the target wasn't in its local config.
	if rec, ok := st.ClaimPendingTarget("w1", nil, []string{"billing"}); ok {
		t.Fatalf("worker without 'orders' claimed %q", rec.Name)
	}
	rec, ok := st.ClaimPendingTarget("w1", nil, []string{"billing", "orders"})
	if !ok || rec.Name != "orders" {
		t.Fatalf("claim = %v %v, want orders", rec, ok)
	}
	if _, ok := st.ClaimPendingTarget("w1", nil, []string{"orders"}); ok {
		t.Fatal("target claimed twice")
	}
}

func TestShutdownEndsSSEStreams(t *testing.T) {
	s := New(Config{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)

	select {
	case body := <-done:
		if !strings.Contains(body, "event: connected") {
			t.Fatalf("unexpected stream body %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream still open after Shutdown")
	}
}
