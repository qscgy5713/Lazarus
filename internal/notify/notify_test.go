package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"lazarus/internal/backup"
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

	var body map[string]string
	if err := json.Unmarshal(received, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["text"]; !ok {
		t.Errorf("payload = %s, want a \"text\" field for Slack", received)
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

	var body map[string]string
	json.Unmarshal(received, &body)
	if _, ok := body["content"]; !ok {
		t.Errorf("payload = %s, want a \"content\" field for Discord", received)
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
