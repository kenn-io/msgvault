package calrecur

import (
	"errors"
	"fmt"
	"slices"
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
		date, err := time.Parse(dateLayout, s.StartDate)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse DTSTART date: %w", err)
		}
		return date, nil
	}
	if s.DTStart.IsZero() {
		return time.Time{}, errors.New("missing DTSTART")
	}
	if s.DTStart.Nanosecond() != 0 {
		return time.Time{}, errors.New("DTSTART must have whole-second precision")
	}
	return s.DTStart.In(s.location()), nil
}

func (s Set) identity(start time.Time) Identity {
	if s.AllDay {
		return Identity{Date: start.UTC().Format(dateLayout), AllDay: true}
	}
	return Identity{Instant: start.UTC()}
}

func (s Set) identityStart(id Identity) (time.Time, error) {
	if id.AllDay != s.AllDay {
		return time.Time{}, errors.New("recurrence value type differs from DTSTART")
	}
	if id.AllDay {
		date, err := time.Parse(dateLayout, id.Date)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse recurrence date: %w", err)
		}
		return date, nil
	}
	if id.Instant.IsZero() {
		return time.Time{}, errors.New("missing recurrence instant")
	}
	if id.Instant.Nanosecond() != 0 {
		return time.Time{}, errors.New("recurrence instant must have whole-second precision")
	}
	return id.Instant.In(s.location()), nil
}

// localDateBoundary resolves a civil date to the first instant whose local
// date is at or after it. For an entirely skipped date, that is the boundary
// at the start of the next existing date.
func localDateBoundary(date string, loc *time.Location) (time.Time, bool, error) {
	civil, err := time.Parse(dateLayout, date)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse civil date boundary: %w", err)
	}
	year, month, day := civil.Date()
	compare := func(t time.Time) int {
		gotYear, gotMonth, gotDay := t.In(loc).Date()
		if gotYear != year {
			if gotYear < year {
				return -1
			}
			return 1
		}
		if gotMonth != month {
			if gotMonth < month {
				return -1
			}
			return 1
		}
		if gotDay < day {
			return -1
		}
		if gotDay > day {
			return 1
		}
		return 0
	}
	low, high := civil.Add(-72*time.Hour).Unix(), civil.Add(72*time.Hour).Unix()
	if compare(time.Unix(low, 0)) >= 0 || compare(time.Unix(high, 0)) < 0 {
		return time.Time{}, false, errors.New("cannot resolve civil date in time zone")
	}
	for low+1 < high {
		mid := low + (high-low)/2
		if compare(time.Unix(mid, 0)) < 0 {
			low = mid
		} else {
			high = mid
		}
	}
	start := time.Unix(high, 0).In(loc)
	return start, compare(start) == 0, nil
}

// localDateStart resolves a civil date to the first instant belonging to it.
// A zone may skip local midnight or the entire date.
func localDateStart(date string, loc *time.Location) (time.Time, error) {
	start, exists, err := localDateBoundary(date, loc)
	if err != nil {
		return time.Time{}, err
	}
	if !exists {
		return time.Time{}, errors.New("civil date does not exist in time zone")
	}
	return start, nil
}

// CivilDateStart resolves a civil date to its first valid instant in loc.
// It preserves the supplied date when local midnight is skipped.
func CivilDateStart(date string, loc *time.Location) (time.Time, error) {
	return localDateStart(date, loc)
}

// CivilDateBoundary resolves a civil date to the first instant whose local
// date is at or after it. exists reports whether that date occurs in loc.
func CivilDateBoundary(date string, loc *time.Location) (start time.Time, exists bool, err error) {
	return localDateBoundary(date, loc)
}

func addCivilDays(date string, days int) (string, error) {
	civil, err := time.Parse(dateLayout, date)
	if err != nil {
		return "", fmt.Errorf("parse civil date for arithmetic: %w", err)
	}
	return civil.AddDate(0, 0, days).Format(dateLayout), nil
}

// AddCivilDays advances an ISO civil date without interpreting it in a zone.
func AddCivilDays(date string, days int) (string, error) {
	return addCivilDays(date, days)
}

func (s Set) instanceStart(anchor time.Time) (time.Time, error) {
	if !s.AllDay {
		return anchor, nil
	}
	return localDateStart(anchor.UTC().Format(dateLayout), s.location())
}

func unsupported(line string, err error) error {
	return fmt.Errorf("%w: %q: %w", ErrUnsupportedRecurrence, line, err)
}

func (s Set) rule(line string, until time.Time) (*rrule.RRule, time.Time, error) {
	if !strings.HasPrefix(line, "RRULE:") || strings.ContainsAny(line, "\r\n") {
		return nil, time.Time{}, unsupported(line, errors.New("expected RRULE"))
	}
	start, err := s.start()
	if err != nil {
		return nil, time.Time{}, unsupported(line, err)
	}
	seen := map[string]bool{}
	for part := range strings.SplitSeq(strings.TrimPrefix(line, "RRULE:"), ";") {
		key, _, ok := strings.Cut(part, "=")
		if !ok || seen[key] || key == "DTSTART" || key == "BYEASTER" {
			return nil, time.Time{}, unsupported(line, errors.New("invalid or repeated rule parameter"))
		}
		seen[key] = true
	}
	loc := time.UTC
	ruleStart := start
	if !s.AllDay {
		loc = s.location()
		ruleStart = s.wallAnchor(start)
	}
	opt, err := rrule.StrToROptionInLocation(strings.TrimPrefix(line, "RRULE:"), loc)
	if err != nil {
		return nil, time.Time{}, unsupported(line, err)
	}
	if seen["COUNT"] && opt.Count <= 0 || seen["INTERVAL"] && opt.Interval <= 0 {
		return nil, time.Time{}, unsupported(line, errors.New("COUNT and INTERVAL must be positive"))
	}
	if err := supportedRule(opt, seen, start); err != nil {
		return nil, time.Time{}, unsupported(line, err)
	}
	if opt.Interval > maxRuleInterval(opt.Freq) {
		return nil, time.Time{}, unsupported(line, errors.New("INTERVAL exceeds the supported calendar range"))
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
	absoluteUntil := opt.Until
	if !until.IsZero() && (absoluteUntil.IsZero() || until.Before(absoluteUntil)) {
		absoluteUntil = until
	}
	if !s.AllDay && !absoluteUntil.IsZero() {
		opt.Until = s.wallHorizon(absoluteUntil)
	} else {
		opt.Until = absoluteUntil
	}
	opt.Dtstart = ruleStart
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, time.Time{}, unsupported(line, err)
	}
	return rule, absoluteUntil, nil
}

// wallAnchor represents a local date and clock as a UTC timestamp so rule
// iteration advances civil dates without DST changing the recurrence clock.
func (s Set) wallAnchor(value time.Time) time.Time {
	local := value.In(s.location())
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
}

// wallHorizon extends an absolute limit by two civil days before converting it
// to a wall-clock anchor. That margin covers candidates in a repeated local
// hour whose wall ordering differs from their absolute ordering. Candidates
// are still checked against the original absolute limit after zone resolution.
func (s Set) wallHorizon(limit time.Time) time.Time {
	horizon := limit.In(s.location()).AddDate(0, 0, 2)
	if horizon.Year() > 9999 {
		horizon = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	}
	return s.wallAnchor(horizon)
}

func maxRuleInterval(freq rrule.Frequency) int {
	switch freq {
	case rrule.YEARLY:
		return rrule.MAXYEAR
	case rrule.MONTHLY:
		return rrule.MAXYEAR * 12
	case rrule.WEEKLY:
		return rrule.MAXYEAR * 53
	case rrule.DAILY:
		return rrule.MAXYEAR * 366
	default:
		return 0
	}
}

func (s Set) ruleIterator(line string, until time.Time) (rrule.Next, error) {
	rule, absoluteUntil, err := s.rule(line, until)
	if err != nil {
		return nil, err
	}
	options := rule.OrigOptions
	countLimit := options.Count
	if countLimit > 0 {
		options.Count = 0
		rule, err = rrule.NewRRule(options)
		if err != nil {
			return nil, unsupported(line, err)
		}
	}
	start, err := s.start()
	if err != nil {
		return nil, unsupported(line, err)
	}
	wallClock := start.In(s.location())
	next := rule.Iterator()
	generated := 0
	return func() (time.Time, bool) {
		for {
			candidate, ok := next()
			if !ok {
				return time.Time{}, false
			}
			if candidate.Hour() != start.Hour() || candidate.Minute() != start.Minute() || candidate.Second() != start.Second() {
				continue
			}
			if !s.AllDay {
				year, month, day := candidate.UTC().Date()
				if candidate.Equal(s.wallAnchor(start)) {
					// A wall time in the repeated hour is ambiguous. DTSTART already
					// identifies which instant starts the series; retain that instant.
					candidate = start
				} else {
					candidate = time.Date(year, month, day, wallClock.Hour(), wallClock.Minute(), wallClock.Second(), wallClock.Nanosecond(), s.location())
					if candidate.Year() != year || candidate.Month() != month || candidate.Day() != day ||
						candidate.Hour() != wallClock.Hour() || candidate.Minute() != wallClock.Minute() || candidate.Second() != wallClock.Second() {
						continue
					}
				}
			}
			if candidate.Before(start) {
				continue
			}
			if !absoluteUntil.IsZero() && candidate.After(absoluteUntil) {
				return time.Time{}, false
			}
			generated++
			if countLimit > 0 && generated > countLimit {
				return time.Time{}, false
			}
			return candidate, true
		}
	}, nil
}

// UNTIL is checked only after rrule-go finds a matching date. Verify that
// selector and interval combinations can reach a date before building an
// iterator, so Next cannot scan to the library's maximum year without
// returning to Iter's budget and cancellation checks. A reachable date repeats
// within one Gregorian cycle: at most 146097 daily, 4800 monthly, or 400 yearly
// periods. Every active weekly period contains a selected weekday.
func supportedRule(opt *rrule.ROption, seen map[string]bool, start time.Time) error {
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
	monthBased := opt.Freq == rrule.MONTHLY || opt.Freq == rrule.YEARLY && seen["BYMONTH"]
	hasPlainWeekday, hasOrdinalWeekday := false, false
	for _, day := range opt.Byweekday {
		if day.N() == 0 {
			hasPlainWeekday = true
		} else {
			hasOrdinalWeekday = true
		}
		if opt.Freq >= rrule.WEEKLY && day.N() != 0 {
			return errors.New("weekday ordinals require monthly or yearly recurrence")
		}
		if monthBased && (day.N() < -5 || day.N() > 5) {
			return errors.New("month-based weekday ordinals must be between -5 and 5")
		}
	}
	if monthBased && hasPlainWeekday && hasOrdinalWeekday {
		return errors.New("mixed plain and ordinal weekdays are unsupported")
	}
	if len(opt.Bymonth) > 0 && len(opt.Bymonthday) > 0 && !possibleMonthDay(opt.Bymonth, opt.Bymonthday) {
		return errors.New("no selected month can contain a selected month day")
	}
	if !hasReachableDate(opt, start) {
		return errors.New("selectors and interval do not reach a recurrence date")
	}
	return nil
}

const (
	gregorianCycleDays   = 146097
	gregorianCycleMonths = 4800
	gregorianCycleYears  = 400
)

func hasReachableDate(opt *rrule.ROption, start time.Time) bool {
	interval := opt.Interval
	if interval <= 0 {
		interval = 1
	}
	switch opt.Freq {
	case rrule.DAILY:
		return dailyCycleHasDate(opt, start, interval)
	case rrule.MONTHLY:
		return monthlyCycleHasDate(opt, start, interval)
	case rrule.YEARLY:
		return yearlyCycleHasDate(opt, start, interval)
	default:
		return true
	}
}

func dailyCycleHasDate(opt *rrule.ROption, start time.Time, interval int) bool {
	year, month, day := start.Date()
	base := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	step := interval % gregorianCycleDays
	periods := gregorianCycleDays / greatestCommonDivisor(step, gregorianCycleDays)
	offset := 0
	for range periods {
		date := base.AddDate(0, 0, offset)
		if (len(opt.Bymonth) == 0 || slices.Contains(opt.Bymonth, int(date.Month()))) &&
			ruleWeekdayMatches(opt, date.Year(), date.Month(), date.Day()) {
			return true
		}
		offset = (offset + step) % gregorianCycleDays
	}
	return false
}

func monthlyCycleHasDate(opt *rrule.ROption, start time.Time, interval int) bool {
	step := interval % gregorianCycleMonths
	periods := gregorianCycleMonths / greatestCommonDivisor(step, gregorianCycleMonths)
	offset := 0
	for range periods {
		first := time.Date(start.Year(), start.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC)
		if monthHasRuleDate(opt, start, first.Year(), first.Month()) {
			return true
		}
		offset = (offset + step) % gregorianCycleMonths
	}
	return false
}

func yearlyCycleHasDate(opt *rrule.ROption, start time.Time, interval int) bool {
	step := interval % gregorianCycleYears
	periods := gregorianCycleYears / greatestCommonDivisor(step, gregorianCycleYears)
	offset := 0
	for range periods {
		year := start.Year() + offset
		for month := time.January; month <= time.December; month++ {
			if len(opt.Bymonth) > 0 && !slices.Contains(opt.Bymonth, int(month)) {
				continue
			}
			if monthHasRuleDate(opt, start, year, month) {
				return true
			}
		}
		offset = (offset + step) % gregorianCycleYears
	}
	return false
}

func monthHasRuleDate(opt *rrule.ROption, start time.Time, year int, month time.Month) bool {
	monthDays := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	var matchingDays []int
	for day := 1; day <= monthDays; day++ {
		if len(opt.Bymonthday) > 0 && !monthDayMatches(opt.Bymonthday, day, monthDays) {
			continue
		}
		if len(opt.Bymonthday) == 0 && len(opt.Byweekday) == 0 {
			if opt.Freq == rrule.MONTHLY && day != start.Day() ||
				opt.Freq == rrule.YEARLY && (month != start.Month() || day != start.Day()) {
				continue
			}
		}
		if len(opt.Byweekday) > 0 && !ruleWeekdayMatches(opt, year, month, day) {
			continue
		}
		matchingDays = append(matchingDays, day)
	}
	if len(opt.Bysetpos) == 0 {
		return len(matchingDays) > 0
	}
	for _, position := range opt.Bysetpos {
		index := position - 1
		if position < 0 {
			index = len(matchingDays) + position
		}
		if index >= 0 && index < len(matchingDays) {
			return true
		}
	}
	return false
}

func monthDayMatches(values []int, day, monthDays int) bool {
	for _, value := range values {
		if value > 0 && value == day || value < 0 && monthDays+value+1 == day {
			return true
		}
	}
	return false
}

func ruleWeekdayMatches(opt *rrule.ROption, year int, month time.Month, day int) bool {
	weekday := (int(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday()) + 6) % 7
	hasPlain, matchesPlain := false, false
	hasOrdinal, matchesOrdinal := false, false
	for _, selector := range opt.Byweekday {
		if selector.N() == 0 || opt.Freq > rrule.MONTHLY {
			hasPlain = true
			if selector.Day() == weekday {
				matchesPlain = true
			}
			continue
		}
		hasOrdinal = true
		if selector.Day() == weekday && nthWeekdayMatches(year, month, day, selector.Day(), selector.N()) {
			matchesOrdinal = true
		}
	}
	return (!hasPlain || matchesPlain) && (!hasOrdinal || matchesOrdinal)
}

func nthWeekdayMatches(year int, month time.Month, day, weekday, ordinal int) bool {
	monthDays := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if ordinal > 0 {
		firstWeekday := (int(time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).Weekday()) + 6) % 7
		first := 1 + (weekday-firstWeekday+7)%7
		return first+(ordinal-1)*7 == day
	}
	lastWeekday := (int(time.Date(year, month, monthDays, 0, 0, 0, 0, time.UTC).Weekday()) + 6) % 7
	last := monthDays - (lastWeekday-weekday+7)%7
	return last+(ordinal+1)*7 == day
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
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
			if _, _, err := s.rule(line, time.Time{}); err != nil {
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
			valueLoc = time.UTC
		} else if strings.HasSuffix(v, "Z") {
			if seen["TZID"] {
				return nil, "", errors.New("UTC value cannot have TZID")
			}
			layout = keyInstantLayout
			valueLoc = time.UTC
		}
		var d time.Time
		var err error
		if dateValue {
			d, err = time.Parse(layout, v)
		} else {
			if strings.HasSuffix(v, "Z") {
				d, err = time.Parse(layout, v)
			} else {
				d, err = parseLocalDateTime(layout, v, valueLoc)
			}
		}
		if err != nil {
			return nil, "", fmt.Errorf("parse recurrence value: %w", err)
		}
		if dateValue {
			ids = append(ids, Identity{Date: d.Format(dateLayout), AllDay: true})
		} else {
			if d.Nanosecond() != 0 {
				return nil, "", errors.New("recurrence instant must have whole-second precision")
			}
			ids = append(ids, Identity{Instant: d.UTC()})
		}
	}
	return ids, property, nil
}

// parseLocalDateTime rejects local values that time.Date would normalize
// across a time-zone gap. Ambiguous values remain valid and use Go's selected
// occurrence, while nonexistent values must not silently change their wall
// clock fields.
func parseLocalDateTime(layout, value string, loc *time.Location) (time.Time, error) {
	wall, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse local recurrence time: %w", err)
	}
	year, month, day := wall.Date()
	resolved := time.Date(year, month, day, wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), loc)
	local := resolved.In(loc)
	if local.Year() != year || local.Month() != month || local.Day() != day ||
		local.Hour() != wall.Hour() || local.Minute() != wall.Minute() || local.Second() != wall.Second() {
		return time.Time{}, errors.New("local recurrence time does not exist in time zone")
	}
	return resolved, nil
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
