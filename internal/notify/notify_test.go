package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lazarus/internal/backup"
	"lazarus/internal/check"
	"lazarus/internal/config"
	"lazarus/internal/verify"
)

func passing(target string) verify.Result {
	return verify.Result{Target: target, Passed: true, Stage: verify.StageDone}
}

func failing(target string, stage verify.Stage, err string) verify.Result {
	return verify.Result{
		Target: target,
		Stage:  stage,
		Err:    errors.New(err),
		Backup: &backup.File{Path: "/backups/" + target + ".sql"},
	}
}

func TestShouldSend(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		when    When
		results []verify.Result
		want    bool
	}{
		{
			name:    "on_failure with a failure",
			url:     "https://hooks.example.com/x",
			when:    WhenOnFailure,
			results: []verify.Result{passing("a"), failing("b", verify.StageRestore, "boom")},
			want:    true,
		},
		{
			name:    "on_failure with everything passing",
			url:     "https://hooks.example.com/x",
			when:    WhenOnFailure,
			results: []verify.Result{passing("a")},
			want:    false,
		},
		{
			// A heartbeat matters: if Lazarus stops running entirely,
			// silence is indistinguishable from healthy backups.
			name:    "always, even when everything passes",
			url:     "https://hooks.example.com/x",
			when:    WhenAlways,
			results: []verify.Result{passing("a")},
			want:    true,
		},
		{
			name:    "never, even with failures",
			url:     "https://hooks.example.com/x",
			when:    WhenNever,
			results: []verify.Result{failing("a", verify.StageRestore, "boom")},
			want:    false,
		},
		{
			name:    "no url configured",
			url:     "",
			when:    WhenAlways,
			results: []verify.Result{failing("a", verify.StageRestore, "boom")},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := New(tc.url, FormatSlack, tc.when)
			if got := n.ShouldSend(tc.results); got != tc.want {
				t.Errorf("ShouldSend() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatMessageForFailures(t *testing.T) {
	results := []verify.Result{
		passing("healthy-db"),
		failing("prod-db", verify.StageChecks, `check "users have rows" failed: got 0, want at least 1`),
	}

	msg := FormatMessage(results)

	for _, want := range []string{"1 of 2", "prod-db", "checks", "got 0, want at least 1", "/backups/prod-db.sql"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "healthy-db") {
		t.Errorf("failure message should focus on what broke, not list passing targets:\n%s", msg)
	}
}

func TestFormatMessageForSuccess(t *testing.T) {
	results := []verify.Result{passing("db-one"), passing("db-two")}

	msg := FormatMessage(results)

	if !strings.Contains(msg, "2 backup(s) verified") {
		t.Errorf("message should summarise the count:\n%s", msg)
	}
	for _, want := range []string{"db-one", "db-two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("success message missing %q:\n%s", want, msg)
		}
	}
}

func TestFormatMessageKeepsMultiLineErrorsShort(t *testing.T) {
	results := []verify.Result{
		failing("prod", verify.StageRestore, "restore failed: ERROR: relation does not exist\nLINE 1: SELECT...\n        ^"),
	}

	msg := FormatMessage(results)

	if strings.Contains(msg, "LINE 1") {
		t.Errorf("only the first line of a database error belongs in chat:\n%s", msg)
	}
	if !strings.Contains(msg, "relation does not exist") {
		t.Errorf("message lost the actual error:\n%s", msg)
	}
}

func TestFormatMessageTruncatesRunawayOutput(t *testing.T) {
	results := make([]verify.Result, 0, 200)
	for i := 0; i < 200; i++ {
		results = append(results, failing(strings.Repeat("target", 5), verify.StageRestore, strings.Repeat("boom", 20)))
	}

	msg := FormatMessage(results)

	if len(msg) > maxMessageLen+len("… (truncated)") {
		t.Errorf("message is %d chars, want it truncated near %d", len(msg), maxMessageLen)
	}
	if !strings.Contains(msg, "truncated") {
		t.Error("a truncated message should say so")
	}
}

func TestTruncateNeverSplitsAMultiByteRune(t *testing.T) {
	// Regression test: truncating by raw byte offset used to have a real
	// chance of landing inside a multi-byte UTF-8 character (Chinese text,
	// an emoji) whenever the cutoff fell in the middle of one, leaving an
	// invalid byte sequence in the outgoing JSON payload.
	for cut := maxMessageLen - 2; cut <= maxMessageLen+2; cut++ {
		prefix := strings.Repeat("a", cut)
		s := prefix + "中文內容"

		got := truncate(s)

		if !utf8.ValidString(got) {
			t.Fatalf("cut=%d: truncate produced invalid UTF-8: %q", cut, got)
		}
	}
}

func TestSendSlackFormat(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatSlack, WhenAlways)
	if err := n.Send(context.Background(), []verify.Result{failing("prod", verify.StageRestore, "boom")}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(received, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["text"]; !ok {
		t.Errorf("payload = %s, want a \"text\" field for Slack", received)
	}
	if blocks, ok := body["blocks"].([]any); !ok || len(blocks) == 0 {
		t.Errorf("payload = %s, want non-empty \"blocks\" for Slack", received)
	}
}

func TestSendDiscordFormat(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatDiscord, WhenAlways)
	if err := n.Send(context.Background(), []verify.Result{passing("prod")}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(received, &body)
	if _, ok := body["content"]; !ok {
		t.Errorf("payload = %s, want a \"content\" field for Discord", received)
	}
	if embeds, ok := body["embeds"].([]any); !ok || len(embeds) == 0 {
		t.Errorf("payload = %s, want non-empty \"embeds\" for Discord", received)
	}
}

func TestSendGenericFormatCarriesStructuredResults(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatGeneric, WhenAlways)
	results := []verify.Result{passing("ok-db"), failing("bad-db", verify.StageRestore, "boom")}
	if err := n.Send(context.Background(), results); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var payload genericPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Passed {
		t.Error("payload.Passed = true, want false when a target failed")
	}
	if payload.Total != 2 || payload.Failed != 1 {
		t.Errorf("payload totals = %d/%d, want total=2 failed=1", payload.Failed, payload.Total)
	}
	if len(payload.Results) != 2 || payload.Results[1].Error == "" {
		t.Errorf("payload.Results = %+v, want the failure to carry its error", payload.Results)
	}
}

func TestSendReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatSlack, WhenAlways)
	err := n.Send(context.Background(), []verify.Result{passing("a")})
	if err == nil {
		t.Fatal("Send() error = nil, want an error for a 500 — a dropped alert must not pass silently")
	}
}

func TestSendReturnsErrorOnUnreachableHost(t *testing.T) {
	n := New("http://127.0.0.1:1/nope", FormatSlack, WhenAlways)
	if err := n.Send(context.Background(), []verify.Result{passing("a")}); err == nil {
		t.Fatal("Send() error = nil, want an error when the webhook is unreachable")
	}
}

func TestSendWithAPIKey(t *testing.T) {
	var authHeader, keyHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		keyHeader = r.Header.Get("X-Lazarus-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatGeneric, WhenAlways).WithAPIKey("test-secret-key")
	if err := n.Send(context.Background(), []verify.Result{passing("ok-db")}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if authHeader != "Bearer test-secret-key" {
		t.Errorf("Authorization header = %q, want %q", authHeader, "Bearer test-secret-key")
	}
	if keyHeader != "test-secret-key" {
		t.Errorf("X-Lazarus-Key header = %q, want %q", keyHeader, "test-secret-key")
	}
}

func TestSendLazarusFormatEnriched(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatLazarus, WhenAlways)
	results := []verify.Result{passing("postgres-prod")}
	if err := n.Send(context.Background(), results); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var payload genericPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Timestamp == "" {
		t.Error("payload.Timestamp is empty")
	}
	if len(payload.Results) != 1 || payload.Results[0].Target != "postgres-prod" {
		t.Errorf("unexpected results in payload: %+v", payload.Results)
	}
}

func TestSendTelegramFormat(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatTelegram, WhenAlways)
	results := []verify.Result{
		failing("orders-db", verify.StageChecks, "row count too low"),
	}

	if err := n.Send(context.Background(), results); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var payload telegramPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal telegram payload: %v", err)
	}

	if payload.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", payload.ParseMode)
	}
	if !strings.Contains(payload.Text, "<b>🚨 Lazarus: Restoration Drill Failed</b>") {
		t.Errorf("telegram text missing header; got %q", payload.Text)
	}
	if !strings.Contains(payload.Text, "<pre>") {
		t.Errorf("telegram text missing <pre> tag; got %q", payload.Text)
	}
}

func TestSendTeamsFormat(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatTeams, WhenAlways)
	results := []verify.Result{
		failing("payments-db", verify.StageRestore, "connection timeout"),
	}

	if err := n.Send(context.Background(), results); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var payload teamsPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal teams payload: %v", err)
	}

	if payload.Type != "MessageCard" {
		t.Errorf("@type = %q, want MessageCard", payload.Type)
	}
	if payload.ThemeColor != "e74c3c" {
		t.Errorf("themeColor = %q, want red (e74c3c) for failure", payload.ThemeColor)
	}
	if len(payload.Sections) == 0 || !strings.Contains(payload.Sections[0].Text, "payments-db") {
		t.Errorf("teams sections missing failure detail; got %+v", payload.Sections)
	}
}

func TestSendPagerDutyFormat(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatPagerDuty, WhenAlways).WithAPIKey("pd-test-routing-key")
	results := []verify.Result{
		failing("orders-db", verify.StageChecks, "count mismatch"),
	}

	if err := n.Send(context.Background(), results); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	var payload pagerDutyPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("unmarshal pagerduty payload: %v", err)
	}

	if payload.RoutingKey != "pd-test-routing-key" {
		t.Errorf("RoutingKey = %q, want pd-test-routing-key", payload.RoutingKey)
	}
	if payload.EventAction != "trigger" {
		t.Errorf("EventAction = %q, want trigger", payload.EventAction)
	}
	if payload.Payload.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", payload.Payload.Severity)
	}
	if !strings.Contains(payload.Payload.Summary, "1 target(s) failed") {
		t.Errorf("Summary = %q, want mention of failure", payload.Payload.Summary)
	}
}

func TestSendRetriesOnServerError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusBadGateway) // 502
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, FormatSlack, WhenAlways)
	err := n.Send(context.Background(), []verify.Result{passing("db")})
	if err != nil {
		t.Fatalf("Send() expected success after retry, got error: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestShouldSendEmail(t *testing.T) {
	smtpCfg := &config.SMTPConfig{
		Host: "smtp.example.com",
		Port: 587,
		From: "lazarus@example.com",
		To:   []string{"admin@example.com"},
	}

	nNilSMTP := New("", FormatEmail, WhenAlways)
	if nNilSMTP.ShouldSend([]verify.Result{passing("db")}) {
		t.Errorf("ShouldSend() = true, want false when SMTP config is nil")
	}

	nWithSMTP := New("", FormatEmail, WhenOnFailure).WithSMTP(smtpCfg)
	if nWithSMTP.ShouldSend([]verify.Result{passing("db")}) {
		t.Errorf("ShouldSend() = true, want false on passing results with WhenOnFailure")
	}
	if !nWithSMTP.ShouldSend([]verify.Result{failing("db", verify.StageRestore, "err")}) {
		t.Errorf("ShouldSend() = false, want true on failing results with WhenOnFailure")
	}
}

func TestSendEmailFormat(t *testing.T) {
	smtpCfg := &config.SMTPConfig{
		Host:     "smtp.example.com",
		Port:     587,
		From:     "sender@example.com",
		To:       []string{"ops@example.com"},
		Username: "ops_user",
		Password: "secret_password",
	}

	var sentAddr, sentFrom string
	var sentTo []string
	var sentMsg []byte

	mockSender := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		sentAddr = addr
		sentFrom = from
		sentTo = to
		sentMsg = msg
		return nil
	}

	n := New("", FormatEmail, WhenAlways).WithSMTP(smtpCfg)
	n.smtpSender = mockSender

	results := []verify.Result{
		{
			Target:          "prod-postgres",
			Tags:            []string{"prod", "us-east"},
			Passed:          true,
			Stage:           verify.StageDone,
			Duration:        1200 * time.Millisecond,
			RestoreDuration: 800 * time.Millisecond,
			Backup: &backup.File{
				Path: "/backups/prod.sql",
				Size: 1024 * 1024 * 50,
			},
			Checks: []check.Result{
				{Name: "users table count", Passed: true, Value: 1000},
			},
		},
	}

	err := n.Send(context.Background(), results)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if sentAddr != "smtp.example.com:587" {
		t.Errorf("sentAddr = %q, want smtp.example.com:587", sentAddr)
	}
	if sentFrom != "sender@example.com" {
		t.Errorf("sentFrom = %q, want sender@example.com", sentFrom)
	}
	if len(sentTo) != 1 || sentTo[0] != "ops@example.com" {
		t.Errorf("sentTo = %v, want [ops@example.com]", sentTo)
	}

	msgStr := string(sentMsg)
	if !strings.Contains(msgStr, "Subject: [Lazarus] ✅ All Database Restorations Passed") {
		t.Errorf("email missing expected subject: %s", msgStr)
	}
	if !strings.Contains(msgStr, "Content-Type: text/html; charset=UTF-8") {
		t.Errorf("email missing text/html header")
	}
	if !strings.Contains(msgStr, "prod-postgres") {
		t.Errorf("email body missing target name: %s", msgStr)
	}
	if !strings.Contains(msgStr, "#prod") || !strings.Contains(msgStr, "#us-east") {
		t.Errorf("email body missing tags: %s", msgStr)
	}
}

func TestSendEmailFailureReport(t *testing.T) {
	smtpCfg := &config.SMTPConfig{
		Host: "smtp.example.com",
		Port: 25,
		From: "sender@example.com",
		To:   []string{"alerts@example.com"},
	}

	var sentMsg []byte
	mockSender := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		sentMsg = msg
		return nil
	}

	n := New("", FormatEmail, WhenAlways).WithSMTP(smtpCfg)
	n.smtpSender = mockSender

	results := []verify.Result{
		{
			Target:    "broken-mysql",
			Tags:      []string{"staging"},
			Passed:    false,
			Stage:     verify.StageRestore,
			Err:       errors.New("table corrupted"),
			DebugHint: "check disk space or schema sync",
		},
	}

	err := n.Send(context.Background(), results)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	msgStr := string(sentMsg)
	if !strings.Contains(msgStr, "Subject: [Lazarus] 🚨 1/1 Database Restorations FAILED") {
		t.Errorf("email missing failure subject: %s", msgStr)
	}
	if !strings.Contains(msgStr, "table corrupted") {
		t.Errorf("email body missing error message")
	}
	if !strings.Contains(msgStr, "check disk space or schema sync") {
		t.Errorf("email body missing debug hint")
	}
}
