package check

import (
	"strings"
	"testing"

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
