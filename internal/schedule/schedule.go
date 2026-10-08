// Package schedule parses pipeline schedules and computes their next run.
//
// Two forms are accepted:
//
//   - an interval, any Go duration such as "30m" or "24h": the pipeline runs
//     that long after its previous run started;
//   - a calendar schedule, a five-field cron expression ("0 8 * * 1-5") or one
//     of @hourly, @daily, @weekly, @monthly: the pipeline runs at those wall
//     clock times in the pipeline's time zone.
//
// "manual", "webhook" and "" are not timer schedules; Parse rejects them and
// callers check IsTimer first.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IsTimer reports whether spec drives automatic timer runs.
func IsTimer(spec string) bool {
	spec = strings.TrimSpace(spec)
	return spec != "" && spec != "manual" && spec != "webhook"
}

// Schedule is a parsed timer schedule.
type Schedule struct {
	spec     string
	interval time.Duration
	cron     *cronSpec
	loc      *time.Location
}

// Parse parses a timer schedule. tz is an IANA zone name ("Europe/Berlin");
// empty means the process's local zone. tz applies to calendar schedules only.
func Parse(spec, tz string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if !IsTimer(spec) {
		return Schedule{}, fmt.Errorf("%q is not a timer schedule", spec)
	}
	loc := time.Local
	if tz = strings.TrimSpace(tz); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return Schedule{}, fmt.Errorf("unknown time zone %q", tz)
		}
		loc = l
	}
	if d, err := time.ParseDuration(spec); err == nil {
		if d <= 0 {
			return Schedule{}, fmt.Errorf("interval %q must be positive", spec)
		}
		return Schedule{spec: spec, interval: d, loc: loc}, nil
	}
	c, err := parseCron(spec)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{spec: spec, cron: c, loc: loc}, nil
}

// String returns the schedule as written.
func (s Schedule) String() string { return s.spec }

// IsInterval reports whether s is a duration schedule.
func (s Schedule) IsInterval() bool { return s.interval > 0 }

// Interval returns the duration of an interval schedule, zero otherwise.
func (s Schedule) Interval() time.Duration { return s.interval }

// Location returns the zone calendar schedules are evaluated in.
func (s Schedule) Location() *time.Location { return s.loc }

// Next returns the first run strictly after t. For an interval schedule that
// is t plus the interval. For a calendar schedule it is the next matching
// minute in the schedule's zone; the zero time means no match within five
// years (for example "0 0 31 2 *").
func (s Schedule) Next(t time.Time) time.Time {
	if s.interval > 0 {
		return t.Add(s.interval)
	}
	if s.cron == nil {
		return time.Time{}
	}
	return s.cron.next(t.In(s.loc))
}

// --- cron ---

type cronSpec struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
}

var descriptors = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseCron(spec string) (*cronSpec, error) {
	if d, ok := descriptors[strings.ToLower(spec)]; ok {
		spec = d
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("invalid schedule %q: use a duration ('30m', '24h'), a five-field cron expression ('0 8 * * 1-5') or @hourly/@daily/@weekly/@monthly", spec)
	}
	c := &cronSpec{}
	var err error
	if c.minute, err = parseField(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if c.hour, err = parseField(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	if c.dom, err = parseField(fields[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	if c.month, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	if c.dow, err = parseField(fields[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}
	// 7 is Sunday too.
	if c.dow&(1<<7) != 0 {
		c.dow |= 1
		c.dow &^= 1 << 7
	}
	c.domStar = fields[2] == "*" || strings.HasPrefix(fields[2], "*/")
	c.dowStar = fields[4] == "*" || strings.HasPrefix(fields[4], "*/")
	return c, nil
}

func parseField(field string, lo, hi int, names map[string]int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty list item in %q", field)
		}
		rangePart, step := part, 1
		if i := strings.Index(part, "/"); i >= 0 {
			rangePart = part[:i]
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("invalid step in %q", part)
			}
			step = n
		}
		start, end := lo, hi
		switch {
		case rangePart == "*":
		case strings.Contains(rangePart, "-"):
			bounds := strings.SplitN(rangePart, "-", 2)
			a, err := value(bounds[0], names)
			if err != nil {
				return 0, err
			}
			b, err := value(bounds[1], names)
			if err != nil {
				return 0, err
			}
			start, end = a, b
		default:
			v, err := value(rangePart, names)
			if err != nil {
				return 0, err
			}
			start, end = v, v
			if strings.Contains(part, "/") {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return n, nil
}

func has(set uint64, v int) bool { return v >= 0 && v < 64 && set&(1<<v) != 0 }

func (c *cronSpec) firstMinute() int {
	for m := 0; m < 60; m++ {
		if has(c.minute, m) {
			return m
		}
	}
	return 0
}

func (c *cronSpec) dayMatches(t time.Time) bool {
	dom := has(c.dom, t.Day())
	dow := has(c.dow, int(t.Weekday()))
	// Standard cron: when both day fields are restricted, either may match.
	if !c.domStar && !c.dowStar {
		return dom || dow
	}
	return dom && dow
}

// next walks forward in wall-clock steps (month, day, hour, minute), skipping
// whole units that cannot match. A wall time that a DST change skips is
// normalized forward by time.Date and still runs once.
func (c *cronSpec) next(t time.Time) time.Time {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !has(c.month, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !has(c.hour, t.Hour()) {
			n := time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			if !n.After(t) { // DST fall-back repeats an hour
				n = t.Add(time.Hour).Truncate(time.Hour)
			}
			// Spring-forward: the wall hour after t does not exist. A slot
			// scheduled inside it runs once, right after the gap.
			if skipped := (t.Hour() + 1) % 24; n.Hour() != skipped && n.Day() == t.Day() && has(c.hour, skipped) {
				return time.Date(n.Year(), n.Month(), n.Day(), n.Hour(), c.firstMinute(), 0, 0, loc)
			}
			t = n
			continue
		}
		if !has(c.minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
