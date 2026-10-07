package schedule

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	base := time.Date(2026, 10, 7, 16, 30, 45, 0, time.UTC) // Wednesday
	cases := []struct {
		expr string
		want time.Time
	}{
		{"0 4 * * *", time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)},
		{"@hourly", time.Date(2026, 10, 7, 17, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 10, 7, 16, 45, 0, 0, time.UTC)},
		{"31 16 * * *", time.Date(2026, 10, 7, 16, 31, 0, 0, time.UTC)},
		{"30 16 * * *", time.Date(2026, 10, 8, 16, 30, 0, 0, time.UTC)}, // strictly after
		{"0 9 * * mon-fri", time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)},
		{"0 0 * * 0", time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)},
		{"0 0 * * 7", time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)},
		{"0 3 1 * *", time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)},
		{"0 0 1 jan *", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"0 2 5,20 * *", time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)},
		// dom and dow both restricted: either matches (Friday the 9th wins).
		{"0 0 13 * 5", time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)},
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.expr, err)
		}
		if got := s.Next(base); !got.Equal(c.want) {
			t.Errorf("Next(%q) = %s, want %s", c.expr, got, c.want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "*/0 * * * *", "5-1 * * * *", "x * * * *", "* * * * 8"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestNextImpossibleDate(t *testing.T) {
	s, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(time.Now()); !got.IsZero() {
		t.Errorf("Feb 30 should never match, got %s", got)
	}
}
