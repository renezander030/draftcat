package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec, tz string) Schedule {
	t.Helper()
	s, err := Parse(spec, tz)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", spec, tz, err)
	}
	return s
}

func TestIntervalNext(t *testing.T) {
	s := mustParse(t, "90m", "")
	if !s.IsInterval() || s.Interval() != 90*time.Minute {
		t.Fatalf("not parsed as interval: %+v", s)
	}
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if got := s.Next(at); !got.Equal(at.Add(90 * time.Minute)) {
		t.Fatalf("Next = %v", got)
	}
}

func TestCronWeekdayMorningInZone(t *testing.T) {
	s := mustParse(t, "0 8 * * 1-5", "Europe/Berlin")
	berlin, _ := time.LoadLocation("Europe/Berlin")
	// Friday 2026-10-09 09:00 Berlin -> next is Monday 2026-10-12 08:00 Berlin.
	from := time.Date(2026, 10, 9, 9, 0, 0, 0, berlin)
	want := time.Date(2026, 10, 12, 8, 0, 0, 0, berlin)
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
	// From UTC input the zone still applies: 05:00 UTC Monday = 07:00 Berlin.
	fromUTC := time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC)
	if got := s.Next(fromUTC); !got.Equal(want) {
		t.Fatalf("Next(UTC) = %v, want %v", got, want)
	}
}

func TestCronIsStrictlyAfter(t *testing.T) {
	s := mustParse(t, "30 * * * *", "UTC")
	at := time.Date(2026, 10, 8, 10, 30, 0, 0, time.UTC)
	if got := s.Next(at); !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("Next = %v", got)
	}
}

func TestCronDescriptorsListsStepsAndNames(t *testing.T) {
	cases := []struct {
		spec string
		from time.Time
		want time.Time
	}{
		{"@daily", time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)},
		{"@hourly", time.Date(2026, 10, 8, 10, 5, 0, 0, time.UTC), time.Date(2026, 10, 8, 11, 0, 0, 0, time.UTC)},
		{"@monthly", time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"*/15 9-17 * * *", time.Date(2026, 10, 8, 17, 50, 0, 0, time.UTC), time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)},
		{"0 9,13 * * MON", time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)},
		{"0 0 1 jan *", time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"0 6 * * 7", time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		s := mustParse(t, c.spec, "UTC")
		if got := s.Next(c.from); !got.Equal(c.want) {
			t.Errorf("%s from %v: got %v want %v", c.spec, c.from, got, c.want)
		}
	}
}

func TestCronDayFieldsOr(t *testing.T) {
	// Restricted day-of-month AND day-of-week match on either (standard cron).
	s := mustParse(t, "0 12 13 * 5", "UTC")
	// 2026-10-08 is a Thursday; next Friday is 10-09, before the 13th.
	got := s.Next(time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCronAcrossSpringForward(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	s := mustParse(t, "30 2 * * *", "Europe/Berlin")
	// 2027-03-28 02:30 does not exist in Berlin; the run still happens once,
	// shifted forward, and the following day is back at 02:30.
	from := time.Date(2027, 3, 27, 12, 0, 0, 0, berlin)
	first := s.Next(from)
	if first.IsZero() || first.Day() != 28 {
		t.Fatalf("first = %v", first)
	}
	second := s.Next(first)
	if want := time.Date(2027, 3, 29, 2, 30, 0, 0, berlin); !second.Equal(want) {
		t.Fatalf("second = %v want %v", second, want)
	}
}

func TestImpossibleCronHasNoNext(t *testing.T) {
	s := mustParse(t, "0 0 31 2 *", "UTC")
	if got := s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Fatalf("got %v, want zero", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{"manual", "webhook", "", "0s", "-5m", "61 * * * *", "* * * *", "0 25 * * *", "0 0 0 * *", "*/0 * * * *", "0 0 * * funday", "5-1 * * * *", "1,,2 * * * *"} {
		if _, err := Parse(spec, ""); err == nil {
			t.Errorf("Parse(%q) accepted", spec)
		}
	}
	if _, err := Parse("0 8 * * *", "Mars/Olympus"); err == nil {
		t.Error("unknown zone accepted")
	}
}

func TestIsTimer(t *testing.T) {
	for spec, want := range map[string]bool{"": false, "manual": false, "webhook": false, "5m": true, "0 8 * * *": true, " @daily ": true} {
		if got := IsTimer(spec); got != want {
			t.Errorf("IsTimer(%q) = %v", spec, got)
		}
	}
}
