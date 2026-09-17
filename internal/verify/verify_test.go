package verify

import (
	"strings"
	"testing"
	"time"
)

func TestExceedsRTO(t *testing.T) {
	cases := []struct {
		name   string
		actual time.Duration
		limit  time.Duration
		want   bool
	}{
		{name: "under the limit", actual: 30 * time.Second, limit: time.Minute, want: false},
		{name: "exactly at the limit", actual: time.Minute, limit: time.Minute, want: false},
		{name: "over the limit", actual: 90 * time.Second, limit: time.Minute, want: true},
		{name: "no limit configured", actual: 6 * time.Hour, limit: 0, want: false},
		{name: "negative limit treated as no limit", actual: time.Hour, limit: -1, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceedsRTO(tc.actual, tc.limit); got != tc.want {
				t.Errorf("exceedsRTO(%s, %s) = %v, want %v", tc.actual, tc.limit, got, tc.want)
			}
		})
	}
}

func TestRTOErrorReportsSubSecondDurationsAccurately(t *testing.T) {
	// Regression test: rounding to whole seconds turned a real 116ms
	// restore into a misleading "restore took 0s".
	err := rtoError(116*time.Millisecond, 50*time.Millisecond)

	if strings.Contains(err.Error(), "took 0s") {
		t.Errorf("error = %q, a 116ms restore must not be reported as 0s", err)
	}
	if !strings.Contains(err.Error(), "116ms") {
		t.Errorf("error = %q, want it to state the actual duration", err)
	}
}
