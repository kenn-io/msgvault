package calrecur

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

var (
	ErrUnsupportedRecurrence = errors.New("unsupported recurrence")
	ErrBudgetExceeded        = errors.New("recurrence generation budget exceeded")
)

const SeriesReadBudget, RequestReadBudget = 50000, 500000

type Set struct {
	DTStart         time.Time
	AllDay          bool
	StartDate       string
	Zone            *time.Location
	RRules          []string
	RDates, ExDates []Identity
}

func (s Set) location() *time.Location {
	if s.Zone != nil {
		return s.Zone
	}
	return time.UTC
}

func (s Set) start() (time.Time, error) {
	if s.AllDay {
		date, err := time.ParseInLocation(dateLayout, s.StartDate, s.location())
		if err != nil {
			return time.Time{}, fmt.Errorf("parse DTSTART date: %w", err)
		}
		return date, nil
	}
	if s.DTStart.IsZero() {
		return time.Time{}, errors.New("missing DTSTART")
	}
	return s.DTStart.In(s.location()), nil
}

func (s Set) identity(start time.Time) Identity {
	if s.AllDay {
		return Identity{Date: start.In(s.location()).Format(dateLayout), AllDay: true}
	}
	return Identity{Instant: start.UTC()}
}

func (s Set) identityStart(id Identity) (time.Time, error) {
	if id.AllDay != s.AllDay {
		return time.Time{}, errors.New("recurrence value type differs from DTSTART")
	}
	if id.AllDay {
		date, err := time.ParseInLocation(dateLayout, id.Date, s.location())
		if err != nil {
			return time.Time{}, fmt.Errorf("parse recurrence date: %w", err)
		}
		return date, nil
	}
	if id.Instant.IsZero() {
		return time.Time{}, errors.New("missing recurrence instant")
	}
	return id.Instant.In(s.location()), nil
}

func unsupported(line string, err error) error {
	return fmt.Errorf("%w: %q: %w", ErrUnsupportedRecurrence, line, err)
}

func (s Set) rule(line string, until time.Time) (*rrule.RRule, error) {
	if !strings.HasPrefix(line, "RRULE:") || strings.ContainsAny(line, "\r\n") {
		return nil, unsupported(line, errors.New("expected RRULE"))
	}
	start, err := s.start()
	if err != nil {
		return nil, unsupported(line, err)
	}
	seen := map[string]bool{}
	for part := range strings.SplitSeq(strings.TrimPrefix(line, "RRULE:"), ";") {
		key, _, ok := strings.Cut(part, "=")
		if !ok || seen[key] || key == "DTSTART" || key == "BYEASTER" {
			return nil, unsupported(line, errors.New("invalid or repeated rule parameter"))
		}
		seen[key] = true
	}
	opt, err := rrule.StrToROptionInLocation(strings.TrimPrefix(line, "RRULE:"), s.location())
	if err != nil {
		return nil, unsupported(line, err)
	}
	if seen["COUNT"] && opt.Count <= 0 || seen["INTERVAL"] && opt.Interval <= 0 {
		return nil, unsupported(line, errors.New("COUNT and INTERVAL must be positive"))
	}
	if err := supportedRule(opt, seen); err != nil {
		return nil, unsupported(line, err)
	}
	opt.Bysetpos = unique(opt.Bysetpos)
	opt.Bymonth = unique(opt.Bymonth)
	opt.Bymonthday = unique(opt.Bymonthday)
	opt.Byyearday = unique(opt.Byyearday)
	opt.Byweekno = unique(opt.Byweekno)
	opt.Byweekday = unique(opt.Byweekday)
	opt.Byhour = unique(opt.Byhour)
	opt.Byminute = unique(opt.Byminute)
	opt.Bysecond = unique(opt.Bysecond)
	if !until.IsZero() && (opt.Until.IsZero() || until.Before(opt.Until)) {
		opt.Until = until
	}
	opt.Dtstart = start
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, unsupported(line, err)
	}
	return rule, nil
}

// UNTIL is checked only after rrule-go finds a matching date. Limit selectors
// before building an iterator so unsupported rules cannot scan inside Next.
func supportedRule(opt *rrule.ROption, seen map[string]bool) error {
	if opt.Freq < rrule.YEARLY || opt.Freq > rrule.DAILY {
		return errors.New("only daily, weekly, monthly and yearly recurrence is supported")
	}
	for key := range seen {
		allowed := false
		switch key {
		case "FREQ", "COUNT", "UNTIL", "INTERVAL", "BYDAY":
			allowed = true
		case "BYMONTH":
			allowed = opt.Freq == rrule.DAILY || opt.Freq == rrule.YEARLY
		case "BYMONTHDAY":
			allowed = opt.Freq == rrule.MONTHLY || opt.Freq == rrule.YEARLY
		case "BYSETPOS":
			allowed = opt.Freq == rrule.MONTHLY
		case "WKST":
			allowed = opt.Freq == rrule.WEEKLY
		}
		if !allowed {
			return fmt.Errorf("rule parameter %s is not supported for this frequency", key)
		}
	}
	if opt.Freq == rrule.MONTHLY && (seen["BYMONTHDAY"] && (seen["BYDAY"] || seen["BYSETPOS"]) || seen["BYSETPOS"] && !seen["BYDAY"]) {
		return errors.New("monthly recurrence requires BYMONTHDAY or BYDAY with optional BYSETPOS")
	}
	if opt.Freq == rrule.YEARLY && (seen["BYMONTH"] || seen["BYMONTHDAY"] || seen["BYDAY"]) &&
		(!seen["BYMONTH"] || seen["BYMONTHDAY"] == seen["BYDAY"]) {
		return errors.New("yearly selectors require BYMONTH with either BYMONTHDAY or BYDAY")
	}
	for _, day := range opt.Byweekday {
		if opt.Freq >= rrule.WEEKLY && day.N() != 0 {
			return errors.New("weekday ordinals require monthly or yearly recurrence")
		}
	}
	if len(opt.Bymonth) > 0 && len(opt.Bymonthday) > 0 && !possibleMonthDay(opt.Bymonth, opt.Bymonthday) {
		return errors.New("no selected month can contain a selected month day")
	}
	return nil
}

func possibleMonthDay(months, days []int) bool {
	// Use a leap year: February 29 and its negative equivalent are possible.
	for _, month := range months {
		if month < 1 || month > 12 {
			continue
		}
		length := time.Date(2000, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
		for _, day := range days {
			if day != 0 && day >= -length && day <= length {
				return true
			}
		}
	}
	return false
}

func ParseSet(dtstart time.Time, allDay bool, startDate string, zone *time.Location, lines []string) (Set, error) {
	s := Set{DTStart: dtstart, AllDay: allDay, StartDate: startDate, Zone: zone}
	if _, err := s.start(); err != nil {
		return Set{}, unsupported("DTSTART", err)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "RRULE:") {
			if _, err := s.rule(line, time.Time{}); err != nil {
				return Set{}, err
			}
			s.RRules = append(s.RRules, line)
			continue
		}
		ids, property, err := s.parseDates(line)
		if err != nil {
			return Set{}, unsupported(line, err)
		}
		if property == "RDATE" {
			s.RDates = append(s.RDates, ids...)
		} else {
			s.ExDates = append(s.ExDates, ids...)
		}
	}
	return s, nil
}

func (s Set) parseDates(line string) ([]Identity, string, error) {
	head, value, ok := strings.Cut(line, ":")
	if !ok || strings.ContainsAny(line, "\r\n") {
		return nil, "", errors.New("invalid property")
	}
	parts := strings.Split(head, ";")
	property := parts[0]
	if property != "RDATE" && property != "EXDATE" {
		return nil, "", errors.New("unknown property")
	}
	loc := s.location()
	dateValue := false
	seen := map[string]bool{}
	for _, param := range parts[1:] {
		name, val, ok := strings.Cut(param, "=")
		if !ok || seen[name] {
			return nil, "", errors.New("invalid parameter")
		}
		seen[name] = true
		switch name {
		case "VALUE":
			switch val {
			case "DATE":
				dateValue = true
			case "DATE-TIME":
			default:
				return nil, "", errors.New("unsupported value type")
			}
		case "TZID":
			var err error
			loc, err = time.LoadLocation(val)
			if err != nil {
				return nil, "", fmt.Errorf("load recurrence time zone: %w", err)
			}
		default:
			return nil, "", errors.New("unknown parameter")
		}
	}
	if dateValue && seen["TZID"] {
		return nil, "", errors.New("DATE cannot have TZID")
	}
	if dateValue != s.AllDay {
		return nil, "", errors.New("recurrence value type differs from DTSTART")
	}
	var ids []Identity
	for v := range strings.SplitSeq(value, ",") {
		valueLoc := loc
		layout := "20060102T150405"
		if dateValue {
			layout = keyDateLayout
		} else if strings.HasSuffix(v, "Z") {
			if seen["TZID"] {
				return nil, "", errors.New("UTC value cannot have TZID")
			}
			layout = keyInstantLayout
			valueLoc = time.UTC
		}
		d, err := time.ParseInLocation(layout, v, valueLoc)
		if err != nil {
			return nil, "", fmt.Errorf("parse recurrence value: %w", err)
		}
		if dateValue {
			ids = append(ids, Identity{Date: d.Format(dateLayout), AllDay: true})
		} else {
			ids = append(ids, Identity{Instant: d.UTC()})
		}
	}
	return ids, property, nil
}

// Repeated BY selectors describe a set, not extra instances counted by COUNT.
func unique[T comparable](values []T) []T {
	seen := make(map[T]bool, len(values))
	out := values[:0]
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
