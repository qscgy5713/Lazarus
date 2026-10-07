package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeUsers(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadUsersAndAttribution(t *testing.T) {
	sum := sha256.Sum256([]byte("bob-key"))
	path := writeUsers(t, `users:
  - name: alice
    role: admin
    key: alice-key
  - name: bob
    role: viewer
    key_sha256: `+hex.EncodeToString(sum[:])+`
`)
	users, err := LoadUsers(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Users: users})
	s.store.RecordReport(InboundReport{Results: []InboundResult{{Target: "db", Passed: true}}})

	if rec := doReq(t, s, "GET", "/api/v1/targets", "bob-key", nil); rec.Code != http.StatusOK {
		t.Fatalf("bob (viewer via sha256) GET targets = %d", rec.Code)
	}
	if rec := doReq(t, s, "POST", "/api/v1/targets/db/trigger", "bob-key", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("bob trigger = %d, want 403", rec.Code)
	}
	if rec := doReq(t, s, "POST", "/api/v1/targets/db/mute", "alice-key", nil); rec.Code != http.StatusOK {
		t.Fatalf("alice mute = %d", rec.Code)
	}
	rep := InboundReport{Results: []InboundResult{{Target: "db", Passed: true}}}
	if rec := doReq(t, s, "POST", "/api/v1/reports", "alice-key", rep); rec.Code != http.StatusOK {
		t.Fatalf("alice report = %d", rec.Code)
	}

	events := s.store.GetAuditEvents(0)
	if len(events) != 1 || events[0].Actor != "alice" || events[0].Action != "mute" || events[0].Target != "db" {
		t.Fatalf("audit = %+v, want alice muting db", events)
	}
	hist := s.store.GetAuditHistory("db", "", "", 0)
	if hist[0].ReportedBy != "alice" {
		t.Errorf("newest history ReportedBy = %q, want alice", hist[0].ReportedBy)
	}

	var me AuthStatusResponse
	_ = json.Unmarshal(doReq(t, s, "GET", "/api/v1/auth/me", "bob-key", nil).Body.Bytes(), &me)
	if me.Username != "bob" || me.Role != RoleViewer {
		t.Errorf("auth/me = %+v", me)
	}
}

func TestLoadUsersRejectsBadEntries(t *testing.T) {
	for _, body := range []string{
		"users:\n  - {name: a, role: root, key: k}\n",
		"users:\n  - {name: a, role: admin}\n",
		"users:\n  - {name: a, role: admin, key: k, key_sha256: abc}\n",
		"users:\n  - {name: a, role: admin, key_sha256: nothex}\n",
		"users:\n  - {name: a, role: admin, key: k}\n  - {name: a, role: viewer, key: j}\n",
	} {
		if _, err := LoadUsers(writeUsers(t, body)); err == nil {
			t.Errorf("LoadUsers accepted %q", body)
		}
	}
}

func TestSessionCookieLoginCSRFAndLogout(t *testing.T) {
	s := New(Config{AdminKey: "adm"})
	s.store.RecordReport(InboundReport{Results: []InboundResult{{Target: "db", Passed: true}}})

	rec := doReq(t, s, "POST", "/api/v1/auth/login", "", map[string]string{"token": "adm"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v, want HttpOnly SameSite=Strict", cookies)
	}
	if strings.Contains(cookies[0].Value, "adm") {
		t.Fatal("cookie must hold an opaque session id, not the key")
	}

	withCookie := func(method, path string, csrf bool) int {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(cookies[0])
		if csrf {
			req.Header.Set(csrfHeader, "1")
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if code := withCookie("GET", "/api/v1/targets", false); code != http.StatusOK {
		t.Fatalf("cookie GET = %d", code)
	}
	if code := withCookie("POST", "/api/v1/targets/db/trigger", false); code != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF header = %d, want 403", code)
	}
	if code := withCookie("POST", "/api/v1/targets/db/trigger", true); code != http.StatusAccepted {
		t.Fatalf("cookie POST with CSRF header = %d, want 202", code)
	}

	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(cookies[0])
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if code := withCookie("GET", "/api/v1/targets", false); code != http.StatusUnauthorized {
		t.Fatalf("after logout = %d, want 401", code)
	}
}

func TestFailedAuthIsRateLimited(t *testing.T) {
	s := New(Config{AdminKey: "adm"})
	for i := 0; i < 10; i++ {
		doReq(t, s, "GET", "/api/v1/targets", "wrong", nil)
	}
	// Even the right key is refused while the IP is blocked.
	if rec := doReq(t, s, "GET", "/api/v1/targets", "adm", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures = %d, want 429", rec.Code)
	}
	if rec := doReq(t, s, "POST", "/api/v1/auth/login", "", map[string]string{"token": "adm"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login while blocked = %d, want 429", rec.Code)
	}
	// Requests without credentials don't count toward the limit.
	s2 := New(Config{AdminKey: "adm"})
	for i := 0; i < 20; i++ {
		doReq(t, s2, "GET", "/api/v1/targets", "", nil)
	}
	if rec := doReq(t, s2, "GET", "/api/v1/targets", "adm", nil); rec.Code != http.StatusOK {
		t.Fatalf("anonymous probes should not block = %d", rec.Code)
	}
}

func TestLeaseRequeueWhenWorkerGoesOffline(t *testing.T) {
	st := NewStore("", 0)
	st.RecordReport(InboundReport{Results: []InboundResult{{Target: "db"}}})
	st.RegisterWorker(WorkerRecord{ID: "w1"})
	_ = st.TriggerTarget("db")
	if _, ok := st.ClaimPendingTarget("w1", nil, []string{"db"}); !ok {
		t.Fatal("claim failed")
	}

	if got := st.RequeueExpiredClaims(time.Hour); len(got) != 0 {
		t.Fatalf("healthy lease requeued: %v", got)
	}

	// Simulate the worker crashing mid-drill.
	st.mu.Lock()
	st.workers["w1"].LastHeartbeat = time.Now().Add(-2 * time.Minute)
	st.mu.Unlock()
	if got := st.RequeueExpiredClaims(time.Hour); len(got) != 1 || got[0] != "db" {
		t.Fatalf("requeued = %v, want [db]", got)
	}
	tgt := st.GetTargets()[0]
	if !tgt.TriggerPending || tgt.ClaimedBy != "" {
		t.Fatalf("after requeue = pending %v claimed %q", tgt.TriggerPending, tgt.ClaimedBy)
	}
	if ev := st.GetAuditEvents(1); len(ev) != 1 || ev[0].Action != "requeue" {
		t.Errorf("requeue not audited: %+v", ev)
	}
}

func TestLeaseTimeoutAndReportClearsClaim(t *testing.T) {
	st := NewStore("", 0)
	st.RecordReport(InboundReport{Results: []InboundResult{{Target: "db"}}})
	st.RegisterWorker(WorkerRecord{ID: "w1"})
	_ = st.TriggerTarget("db")
	st.ClaimPendingTarget("w1", nil, nil)

	st.RecordReport(InboundReport{Results: []InboundResult{{Target: "db", Passed: true}}})
	if tgt := st.GetTargets()[0]; tgt.ClaimedBy != "" {
		t.Fatal("report should release the lease")
	}

	_ = st.TriggerTarget("db")
	st.ClaimPendingTarget("w1", nil, nil)
	st.mu.Lock()
	st.targets["db"].ClaimedAt = time.Now().Add(-3 * time.Hour)
	st.mu.Unlock()
	if got := st.RequeueExpiredClaims(2 * time.Hour); len(got) != 1 {
		t.Fatalf("expired lease not requeued: %v", got)
	}
}

func TestHistoryRetentionPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := NewStoreWithRetention(path, 0, 30, 24*time.Hour)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	st.RecordReport(InboundReport{Timestamp: old, Results: []InboundResult{{Target: "db", Passed: true}}})
	for i := 0; i < 40; i++ {
		st.RecordReport(InboundReport{Results: []InboundResult{{Target: "db", Passed: true, LogsTail: "log"}}})
	}
	st.RecordAudit(Principal{Name: "alice", Role: RoleAdmin}, "mute", "db", "1.2.3.4", "")

	re := NewStoreWithRetention(path, 0, 30, 24*time.Hour)
	hist := re.GetAuditHistory("db", "", "", 0)
	if len(hist) != 30 {
		t.Fatalf("history after restart = %d, want cap 30 (and the 48h-old one dropped)", len(hist))
	}
	withLogs := 0
	for _, h := range hist {
		if h.LogsTail != "" {
			withLogs++
		}
	}
	if withLogs != uiHistoryLen {
		t.Errorf("records keeping logs = %d, want %d", withLogs, uiHistoryLen)
	}
	if got := re.GetTargets()[0].RecentHistory; len(got) != uiHistoryLen {
		t.Errorf("targets API history = %d, want %d", len(got), uiHistoryLen)
	}
	if ev := re.GetAuditEvents(0); len(ev) != 1 || ev[0].Actor != "alice" {
		t.Errorf("audit after restart = %+v", ev)
	}
	if len(re.GetRecentReports()) == 0 {
		t.Error("reports should persist across restart")
	}
}

func TestAuditCSVExport(t *testing.T) {
	s := New(Config{})
	s.store.RecordAudit(Principal{Name: "=cmd", Role: RoleAdmin}, "trigger", "db", "1.1.1.1", "")
	rec := doReq(t, s, "GET", "/api/v1/export/audit.csv", "", nil)
	body := rec.Body.String()
	if !strings.HasPrefix(body, "Time,Actor,Role,Action") || !strings.Contains(body, "'=cmd") {
		t.Fatalf("audit csv = %q (want header and formula-escaped actor)", body)
	}
}

func TestStaleSessionCookieDoesNotTriggerLockout(t *testing.T) {
	s := New(Config{AdminKey: "adm"})
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest("GET", "/api/v1/targets", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "expired-session"})
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	if rec := doReq(t, s, "POST", "/api/v1/auth/login", "", map[string]string{"token": "adm"}); rec.Code != http.StatusOK {
		t.Fatalf("login after stale-cookie refreshes = %d, want 200", rec.Code)
	}
}

func TestLeaseRequeueGraceAfterRestartAndAnonymousClaims(t *testing.T) {
	st := NewStore("", 0)
	st.RecordReport(InboundReport{Results: []InboundResult{{Target: "a"}, {Target: "b"}}})
	_ = st.TriggerTarget("a")
	_ = st.TriggerTarget("b")
	st.ClaimPendingTarget("restarted-worker", nil, []string{"a"}) // worker not yet re-registered
	st.ClaimPendingTarget("", nil, []string{"b"})                 // poller without an ID

	if got := st.RequeueExpiredClaims(time.Hour); len(got) != 0 {
		t.Fatalf("requeued %v right after start; workers need a heartbeat window to re-register", got)
	}
	st.mu.Lock()
	st.started = time.Now().Add(-2 * time.Minute)
	st.mu.Unlock()
	if got := st.RequeueExpiredClaims(time.Hour); len(got) != 1 || got[0] != "a" {
		t.Fatalf("requeued %v, want only [a] (anonymous claim waits for the lease timeout)", got)
	}
}

func TestUnclaimedTargetOmitsClaimedAt(t *testing.T) {
	s := New(Config{})
	s.store.RecordReport(InboundReport{Results: []InboundResult{{Target: "db", Passed: true}}})
	if body := doReq(t, s, "GET", "/api/v1/targets", "", nil).Body.String(); strings.Contains(body, "claimed_at") {
		t.Fatalf("unclaimed target should omit claimed_at: %s", body)
	}
}
