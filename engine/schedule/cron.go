// Package schedule parses the five-field cron expressions used for
// recurring work and computes when they next fire.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed expression. Each field is a bit set of the values it
// matches.
type Spec struct {
	minute, hour, dom, month, dow uint64
	// domAny and dowAny record a "*" in the day fields. When both are
	// restricted, cron fires on a day that matches either.
	domAny, dowAny bool
}

var aliases = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

// Parse reads "minute hour day-of-month month day-of-week". Fields accept
// "*", single values, ranges (1-5), lists (1,15) and steps (*/15, 0-30/10).
// Sunday is 0 or 7. @hourly, @daily, @weekly and @monthly are also accepted.
func Parse(expr string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	if full, ok := aliases[expr]; ok {
		expr = full
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Spec{}, fmt.Errorf("a schedule has five fields (minute hour day month weekday), found %d", len(fields))
	}
	var s Spec
	var err error
	if s.minute, err = field(fields[0], 0, 59, "minute"); err != nil {
		return s, err
	}
	if s.hour, err = field(fields[1], 0, 23, "hour"); err != nil {
		return s, err
	}
	if s.dom, err = field(fields[2], 1, 31, "day of month"); err != nil {
		return s, err
	}
	if s.month, err = field(fields[3], 1, 12, "month"); err != nil {
		return s, err
	}
	if s.dow, err = field(fields[4], 0, 7, "weekday"); err != nil {
		return s, err
	}
	if s.dow&(1<<7) != 0 {
		s.dow |= 1
	}
	s.domAny, s.dowAny = strings.HasPrefix(fields[2], "*"), strings.HasPrefix(fields[4], "*")
	return s, nil
}

func field(text string, min, max int, name string) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(text, ",") {
		step := 1
		if base, st, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%s: %q is not a valid step", name, st)
			}
			part, step = base, n
		}
		lo, hi := min, max
		if part != "*" {
			from, to, isRange := strings.Cut(part, "-")
			var err error
			if lo, err = strconv.Atoi(from); err != nil {
				return 0, fmt.Errorf("%s: %q is not a number", name, from)
			}
			hi = lo
			if isRange {
				if hi, err = strconv.Atoi(to); err != nil {
					return 0, fmt.Errorf("%s: %q is not a number", name, to)
				}
			} else if step > 1 {
				hi = max
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func (s Spec) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domAny || s.dowAny {
		return dom && dow
	}
	return dom || dow
}

// Next returns the first time strictly after t that the schedule fires, in
// t's time zone. It returns the zero time when the expression can never
// fire, such as the 31st of February.
func (s Spec) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case s.hour&(1<<uint(t.Hour())) == 0:
			t = t.Truncate(time.Hour).Add(time.Hour)
		case s.minute&(1<<uint(t.Minute())) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
