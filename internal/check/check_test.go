package check

import (
	"strings"
	"testing"
	"time"

	"lazarus/internal/config"
)

func int64ptr(v int64) *int64 { return &v }

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name       string
		value      int64
		check      config.Check
		wantPassed bool
	}{
		{
			name:       "min satisfied",
			value:      42,
			check:      config.Check{Min: int64ptr(1)},
			wantPassed: true,
		},
		{
			// The failure this whole tool exists for: schema restored, zero rows.
			name:       "min violated by an empty table",
			value:      0,
			check:      config.Check{Min: int64ptr(1)},
			wantPassed: false,
		},
		{
			name:       "min met exactly",
			value:      1,
			check:      config.Check{Min: int64ptr(1)},
			wantPassed: true,
		},
		{
			name:       "max satisfied",
			value:      5,
			check:      config.Check{Max: int64ptr(10)},
			wantPassed: true,
		},
		{
			name:       "max violated",
			value:      11,
			check:      config.Check{Max: int64ptr(10)},
			wantPassed: false,
		},
		{
			name:       "equal satisfied",
			value:      7,
			check:      config.Check{Equal: int64ptr(7)},
			wantPassed: true,
		},
		{
			name:       "equal violated",
			value:      6,
			check:      config.Check{Equal: int64ptr(7)},
			wantPassed: false,
		},
		{
			name:       "range satisfied",
			value:      5,
			check:      config.Check{Min: int64ptr(1), Max: int64ptr(10)},
			wantPassed: true,
		},
		{
			name:       "range violated at the bottom",
			value:      0,
			check:      config.Check{Min: int64ptr(1), Max: int64ptr(10)},
			wantPassed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			passed, reason := evaluate(tc.value, tc.check)
			if passed != tc.wantPassed {
				t.Errorf("evaluate(%d) passed = %v, want %v (reason: %s)", tc.value, passed, tc.wantPassed, reason)
			}
			if !passed && reason == "" {
				t.Error("a failed check must explain why")
			}
		})
	}
}

func TestParseScalar(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "plain number", raw: "42", want: 42},
		{name: "padded by the client", raw: "\n 42 \n", want: 42},
		{name: "zero", raw: "0", want: 0},
		{
			name: "mysql password warning is ignored",
			raw:  "mysql: [Warning] Using a password on the command line interface can be insecure.\n17\n",
			want: 17,
		},
		{name: "empty output", raw: "", wantErr: true},
		{name: "not a number", raw: "users", wantErr: true},
		{name: "multiple rows", raw: "1\n2\n3", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseScalar(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseScalar(%q) error = nil, want an error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseScalar(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseScalar(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseScalarErrorMentionsWhatItGot(t *testing.T) {
	_, err := parseScalar("users")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "users") {
		t.Errorf("error = %q, want it to quote the unexpected output", err)
	}
}

func TestEvaluatePattern(t *testing.T) {
	// Pattern match success
	passCheck := config.Check{Pattern: "^.+@test\\.local$"}
	passed, reason := EvaluatePattern("user123@test.local\n", passCheck)
	if !passed {
		t.Errorf("expected pass, got fail: %s", reason)
	}

	// Pattern match failure
	failed, reason := EvaluatePattern("user123@realcompany.com\n", passCheck)
	if failed {
		t.Errorf("expected fail for non-matching pattern, got passed")
	}
	if !strings.Contains(reason, "did not match expected pattern") {
		t.Errorf("unexpected failure reason: %s", reason)
	}

	// NotPattern (prohibited pattern) success (clean output)
	cleanCheck := config.Check{NotPattern: "^\\d{3}-\\d{2}-\\d{4}$"}
	passed, reason = EvaluatePattern("REDACTED-SSN\n", cleanCheck)
	if !passed {
		t.Errorf("expected pass for non-matching prohibited pattern, got fail: %s", reason)
	}

	// NotPattern failure (leak detected)
	leaked, reason := EvaluatePattern("123-45-6789\n", cleanCheck)
	if leaked {
		t.Errorf("expected fail when prohibited pattern matched")
	}
	if !strings.Contains(reason, "sanitization failed") {
		t.Errorf("expected reason to mention sanitization failed, got: %s", reason)
	}
}

func TestEvaluateString(t *testing.T) {
	expected := "e4d909c290d0fb1ca068ffaddf22cbd0"
	chk := config.Check{ExpectString: &expected}

	// Match success (with whitespace trimmed)
	passed, reason := EvaluateString("  e4d909c290d0fb1ca068ffaddf22cbd0\n", chk)
	if !passed {
		t.Errorf("expected pass, got fail: %s", reason)
	}

	// Match failure
	passed, reason = EvaluateString("wrong_checksum", chk)
	if passed {
		t.Error("expected fail on mismatched string, got pass")
	}
	if !strings.Contains(reason, "want exactly") {
		t.Errorf("unexpected failure reason: %s", reason)
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "RFC3339", raw: "2026-10-07T14:30:00Z", wantErr: false},
		{name: "SQL standard", raw: "2026-10-07 14:30:00", wantErr: false},
		{name: "Date only", raw: "2026-10-07", wantErr: false},
		{name: "Unix epoch seconds", raw: "1791350400", wantErr: false},
		{name: "Unix epoch millis", raw: "1791350400000", wantErr: false},
		{name: "Quoted timestamp", raw: `"2026-10-07 14:30:00"`, wantErr: false},
		{name: "Invalid text", raw: "not-a-timestamp", wantErr: true},
		{name: "Empty string", raw: "   ", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTimestamp(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseTimestamp(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestEvaluateRPO(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	stale := now.Add(-48 * time.Hour).Format("2006-01-02 15:04:05")

	chk := config.Check{
		MaxRPO: 1 * time.Hour,
	}

	// 1. Recent timestamp: complies with RPO
	passed, lag, reason := EvaluateRPO(recent, chk)
	if !passed {
		t.Errorf("expected recent timestamp to pass RPO, got fail: %s", reason)
	}
	if lag < 9*time.Minute || lag > 11*time.Minute {
		t.Errorf("unexpected lag duration: %v", lag)
	}

	// 2. Stale timestamp: violates RPO
	passed, lag, reason = EvaluateRPO(stale, chk)
	if passed {
		t.Errorf("expected 48h stale timestamp to violate 1h RPO, but it passed")
	}
	if !strings.Contains(reason, "RPO exceeded") {
		t.Errorf("expected reason to mention 'RPO exceeded', got: %s", reason)
	}

	// 3. Invalid timestamp format: fails gracefully
	passed, _, reason = EvaluateRPO("garbage-time", chk)
	if passed {
		t.Errorf("expected invalid timestamp to fail")
	}
	if !strings.Contains(reason, "invalid timestamp") {
		t.Errorf("unexpected error message: %s", reason)
	}
}
