package calevent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"go.kenn.io/msgvault/internal/calrecur"
)

// Check each referenced date's offsets before verifying transitions across the event range.
func validateICSTimeZones(ctx context.Context, cal *ical.Calendar, event *ical.Component, budget *ICSParseBudget) error {
	for _, props := range event.Props {
		for _, prop := range props {
			zone := prop.Params.Get("TZID")
			if zone == "" || !strings.EqualFold(string(prop.ValueType()), string(ical.ValueDateTime)) {
				continue
			}
			zone = icsMappedTimeZone(zone)
			for value := range strings.SplitSeq(prop.Value, ",") {
				date, err := time.Parse("20060102T150405", strings.TrimSuffix(value, "Z"))
				if err != nil {
					// Event and recurrence parsers own malformed-value diagnostics.
					continue
				}
				for _, comp := range cal.Children {
					if comp.Name != "VTIMEZONE" || comp.Props.Get("TZID") == nil || icsMappedTimeZone(comp.Props.Get("TZID").Value) != zone {
						continue
					}
					if err := validateICSObservances(ctx, comp, zone, date, budget); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func icsMappedTimeZone(zone string) string {
	if mapped := windowsZones[zone]; mapped != "" {
		return mapped
	}
	return zone
}

type icsTimezoneTransition struct {
	at       time.Time
	from, to int
}

const maxICSTimezoneTransitions = calrecur.SeriesReadBudget

// validateICSTimeZoneUseRange compares every transition that can affect this
// event's recurrence window with the selected IANA zone. An unbounded event
// recurrence is checked across a full Gregorian cycle.
func validateICSTimeZoneUseRange(ctx context.Context, cal *ical.Calendar, component *ical.Component, event Event, budget *ICSParseBudget) error {
	if event.Start == nil || event.End == nil {
		return nil
	}
	checked := make(map[string]bool)
	for _, props := range component.Props {
		for _, prop := range props {
			zone := prop.Params.Get("TZID")
			if zone == "" {
				continue
			}
			zone = icsMappedTimeZone(zone)
			if checked[zone] {
				continue
			}
			var matchingTimezones []*ical.Component
			for _, timezone := range cal.Children {
				if timezone.Name != "VTIMEZONE" {
					continue
				}
				timezoneID := timezone.Props.Get("TZID")
				if timezoneID == nil || icsMappedTimeZone(timezoneID.Value) != zone {
					continue
				}
				matchingTimezones = append(matchingTimezones, timezone)
			}
			if len(matchingTimezones) == 0 {
				continue
			}
			checked[zone] = true
			if err := validateICSReferencedZoneRange(ctx, matchingTimezones, zone, event, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateICSReferencedZoneRange(ctx context.Context, timezones []*ical.Component, zone string, event Event, budget *ICSParseBudget) error {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return fmt.Errorf("VTIMEZONE: %w", err)
	}
	eventLoc, err := time.LoadLocation(icsMappedTimeZone(event.EventTimeZone))
	if err != nil {
		return fmt.Errorf("VTIMEZONE event zone: %w", err)
	}
	start, end, err := icsEventTimeZoneRange(ctx, event, eventLoc, budget)
	if err != nil {
		return fmt.Errorf("VTIMEZONE event range: %w", err)
	}
	for _, timezone := range timezones {
		if err := validateICSTransitionRange(ctx, timezone, loc, start, end, budget); err != nil {
			return err
		}
	}
	return nil
}

func icsEventTimeZoneRange(ctx context.Context, event Event, loc *time.Location, budget *ICSParseBudget) (time.Time, time.Time, error) {
	if event.Start == nil || event.End == nil {
		return time.Time{}, time.Time{}, nil
	}
	startWall := icsEventCivilStart(event, loc)
	// Verify the same recurrence forms expansion accepts, including exclusions.
	// Unsupported input must reach the per-event retention path before any
	// verification-specific parser can turn it into a document error.
	set, err := calrecur.ParseSetWithCivilStart(*event.Start, startWall, event.AllDay, event.StartDate, loc, event.Recurrence)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	firstWall, lastWall := startWall, startWall
	unbounded := false
	for _, line := range event.Recurrence {
		value, ok := strings.CutPrefix(line, "RRULE:")
		if !ok {
			continue
		}
		rule, open, err := icsEventVerificationRule(value, event, loc, startWall)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		if open {
			unbounded = true
			continue
		}
		last, err := icsLastRuleWall(ctx, rule, budget)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		if last.After(lastWall) {
			lastWall = last
		}
	}
	for _, id := range set.RDates {
		wall := icsCivilTime(id.Instant.In(loc))
		if id.AllDay {
			wall, _ = time.Parse(time.DateOnly, id.Date)
		}
		if wall.Before(firstWall) {
			firstWall = wall
		}
		if wall.After(lastWall) {
			lastWall = wall
		}
	}
	if unbounded {
		last := startWall.AddDate(400, 0, 0)
		if last.After(icsMaximumCivilDate()) {
			last = icsMaximumCivilDate()
		}
		if last.After(lastWall) {
			lastWall = last
		}
	}
	return icsTimeZoneBounds(event, loc, firstWall, lastWall)
}

func icsTimeZoneBounds(event Event, loc *time.Location, firstWall, lastWall time.Time) (time.Time, time.Time, error) {
	first, err := icsRecurrenceWallInstant(firstWall, event.AllDay, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("resolve first recurrence time: %w", err)
	}
	last, err := icsRecurrenceWallInstant(lastWall, event.AllDay, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("resolve final recurrence time: %w", err)
	}
	start := event.Start.UTC()
	if first.Before(start) {
		start = first.UTC()
	}
	end := last.Add(event.End.Sub(*event.Start)).UTC()
	if end.Before(event.End.UTC()) {
		end = event.End.UTC()
	}
	return start, end, nil
}

func icsRecurrenceWallInstant(wall time.Time, allDay bool, loc *time.Location) (time.Time, error) {
	if allDay {
		instant, _, err := calrecur.CivilDateBoundary(wall.Format(time.DateOnly), loc)
		return instant, err
	}
	return calrecur.ParseLocalDateTime("20060102T150405", wall.Format("20060102T150405"), loc)
}

func icsEventCivilStart(event Event, loc *time.Location) time.Time {
	if event.AllDay {
		date, _ := time.Parse(time.DateOnly, event.StartDate)
		return date.UTC()
	}
	if event.StartWall != nil {
		return event.StartWall.UTC()
	}
	return icsCivilTime(event.Start.In(loc))
}

func icsCivilTime(value time.Time) time.Time {
	year, month, day := value.Date()
	hour, minute, second := value.Clock()
	return time.Date(year, month, day, hour, minute, second, value.Nanosecond(), time.UTC)
}

func icsMaximumCivilDate() time.Time {
	return time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
}

func validateICSTransitionRange(ctx context.Context, comp *ical.Component, loc *time.Location, start, end time.Time, budget *ICSParseBudget) error {
	var expected []icsTimezoneTransition
	for _, obs := range comp.Children {
		if obs.Name != "STANDARD" && obs.Name != "DAYLIGHT" {
			continue
		}
		from, err := icsObservanceOffsetField(obs, "TZOFFSETFROM")
		if err != nil {
			return err
		}
		// VTIMEZONE transition onset is a local wall time interpreted with
		// TZOFFSETFROM. Derive the search interval from absolute event bounds
		// so a fall-back transition remains visible when its local onset is
		// later than the event's displayed end time.
		wallStart := start.UTC().Add(time.Duration(from) * time.Second)
		wallEnd := end.UTC().Add(time.Duration(from) * time.Second)
		transitions, err := icsObservanceTransitions(ctx, obs, wallStart, wallEnd, budget)
		if err != nil {
			return err
		}
		expected = append(expected, transitions...)
	}
	if len(expected) == 0 {
		return errors.New("VTIMEZONE has no verifiable observances")
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].at.Before(expected[j].at) })
	activeOffset := expected[0].from
	for _, transition := range expected {
		if !transition.at.After(start) {
			activeOffset = transition.to
		}
	}
	_, actualOffset := start.In(loc).Zone()
	if actualOffset != activeOffset {
		return fmt.Errorf("VTIMEZONE active offset %d contradicts %s offset %d at event start", activeOffset, loc, actualOffset)
	}

	actual := icsZoneTransitionsInRange(loc, start, end)
	wanted := make(map[int64]icsTimezoneTransition)
	for _, transition := range expected {
		if transition.from == transition.to || transition.at.Before(start) || transition.at.After(end) {
			continue
		}
		wanted[transition.at.Unix()] = transition
	}
	if len(actual) != len(wanted) {
		return fmt.Errorf("VTIMEZONE transition dates contradict %s in event range", loc)
	}
	for timestamp, transition := range actual {
		candidate, ok := wanted[timestamp]
		if !ok || candidate.from != transition.from || candidate.to != transition.to {
			return fmt.Errorf("VTIMEZONE transition dates contradict %s in event range", loc)
		}
	}
	return nil
}

func icsObservanceTransitions(ctx context.Context, obs *ical.Component, start, end time.Time, budget *ICSParseBudget) ([]icsTimezoneTransition, error) {
	startProp := obs.Props.Get("DTSTART")
	if startProp == nil {
		return nil, fmt.Errorf("VTIMEZONE %s missing DTSTART", obs.Name)
	}
	wallStart, err := time.ParseInLocation("20060102T150405", startProp.Value, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("VTIMEZONE %s DTSTART: %w", obs.Name, err)
	}
	from, err := icsObservanceOffsetField(obs, "TZOFFSETFROM")
	if err != nil {
		return nil, err
	}
	to, err := icsObservanceOffset(obs)
	if err != nil {
		return nil, err
	}
	walls := map[int64]time.Time{wallStart.Unix(): wallStart}
	for _, prop := range obs.Props.Values("RRULE") {
		if err := icsExpandObservanceRule(ctx, obs.Name, prop.Value, wallStart, start, end, from, walls, budget); err != nil {
			return nil, err
		}
	}
	for _, prop := range obs.Props.Values("RDATE") {
		for value := range strings.SplitSeq(prop.Value, ",") {
			var wall time.Time
			var err error
			if len(value) == 8 {
				wall, err = time.ParseInLocation("20060102", value, time.UTC)
			} else {
				wall, err = time.ParseInLocation("20060102T150405", value, time.UTC)
			}
			if err != nil {
				return nil, fmt.Errorf("VTIMEZONE %s RDATE: %w", obs.Name, err)
			}
			walls[wall.Unix()] = wall
			if len(walls) > maxICSTimezoneTransitions {
				return nil, errors.New("VTIMEZONE exceeds the transition verification budget")
			}
		}
	}
	if len(walls) > maxICSTimezoneTransitions {
		return nil, errors.New("VTIMEZONE exceeds the transition verification budget")
	}
	out := make([]icsTimezoneTransition, 0, len(walls))
	for _, wall := range walls {
		out = append(out, icsTimezoneTransition{at: wall.Add(-time.Duration(from) * time.Second), from: from, to: to})
	}
	return out, nil
}

func icsZoneTransitionsInRange(loc *time.Location, start, end time.Time) map[int64]icsTimezoneTransition {
	out := make(map[int64]icsTimezoneTransition)
	add := func(at time.Time) {
		before := at.Add(-time.Second).In(loc)
		after := at.In(loc)
		_, from := before.Zone()
		_, to := after.Zone()
		if from != to {
			out[at.Unix()] = icsTimezoneTransition{at: at, from: from, to: to}
		}
	}
	if zoneStart, _ := start.In(loc).ZoneBounds(); zoneStart.Equal(start) {
		add(zoneStart)
	}
	for current := start; ; {
		_, next := current.In(loc).ZoneBounds()
		if next.IsZero() || next.After(end) {
			break
		}
		add(next)
		if !next.After(current) {
			break
		}
		current = next
	}
	return out
}

func validateICSObservances(ctx context.Context, comp *ical.Component, zone string, date time.Time, budget *ICSParseBudget) error {
	if mapped := windowsZones[zone]; mapped != "" {
		zone = mapped
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return fmt.Errorf("VTIMEZONE: %w", err)
	}
	at, err := calrecur.ParseLocalDateTime("20060102T150405", date.Format("20060102T150405"), loc)
	if err != nil {
		return fmt.Errorf("VTIMEZONE DTSTART: %w", err)
	}
	if err := validateICSDaylight(comp, loc, at); err != nil {
		return err
	}
	kind := "STANDARD"
	if at.IsDST() {
		kind = "DAYLIGHT"
	}
	obs, err := applicableICSObservance(comp, kind, date)
	if err != nil {
		return err
	}
	if obs == nil {
		return fmt.Errorf("VTIMEZONE %s contradicts %s near DTSTART", kind, zone)
	}
	if err := validateICSOffset(obs, zone, at); err != nil {
		return err
	}
	return validateICSTransitionRange(ctx, comp, loc, at, at, budget)
}

func validateICSOffset(obs *ical.Component, zone string, at time.Time) error {
	offset, err := icsObservanceOffset(obs)
	if err != nil {
		return err
	}
	_, actual := at.Zone()
	if actual != offset {
		return fmt.Errorf("VTIMEZONE %s TZOFFSETTO %s contradicts %s near DTSTART", obs.Name, obs.Props.Get("TZOFFSETTO").Value, zone)
	}
	return nil
}

// Historical daylight time may have ended permanently, and a winter-only
// definition may carry no DAYLIGHT at all. The kind check in
// validateICSObservances still requires DAYLIGHT for a summer DTSTART, and the
// range check rejects recurrences that cross a transition. Any DAYLIGHT that is
// supplied must still match the zone.
func validateICSDaylight(comp *ical.Component, loc *time.Location, at time.Time) error {
	standard, daylight := nearbyICSOffsets(at)
	if daylight == nil {
		return nil
	}
	obs, err := applicableICSObservance(comp, "DAYLIGHT", at)
	if err != nil {
		return err
	}
	if obs == nil {
		return nil
	}
	if standard != nil {
		from, fromErr := icsObservanceOffsetField(obs, "TZOFFSETFROM")
		to, toErr := icsObservanceOffset(obs)
		if fromErr == nil && toErr == nil && from == *standard && to == *daylight {
			return nil
		}
	}
	return fmt.Errorf("VTIMEZONE DAYLIGHT contradicts %s near DTSTART", loc)
}

// Sample the surrounding year every two days. Walking ZoneBounds never
// advanced on the Windows runner and hung the tests; no zone period is that short.
func nearbyICSOffsets(at time.Time) (standard, daylight *int) {
	start, end := at.AddDate(0, -6, 0), at.AddDate(0, 6, 0)
	for current := start; current.Before(end); current = current.AddDate(0, 0, 2) {
		_, offset := current.Zone()
		if current.IsDST() {
			daylight = &offset
		} else {
			standard = &offset
		}
	}
	return standard, daylight
}

func icsObservanceOffset(obs *ical.Component) (int, error) {
	return icsObservanceOffsetField(obs, "TZOFFSETTO")
}

func icsObservanceOffsetField(obs *ical.Component, field string) (int, error) {
	p := obs.Props.Get(field)
	if p == nil {
		return 0, fmt.Errorf("VTIMEZONE %s missing %s", obs.Name, field)
	}
	layout := "-0700"
	if len(p.Value) == 7 {
		layout = "-070000"
	}
	parsed, err := time.Parse(layout, p.Value)
	if err != nil {
		return 0, fmt.Errorf("VTIMEZONE %s: %w", field, err)
	}
	_, offset := parsed.Zone()
	return offset, nil
}

// Choose by local onset, independent of the order of observances in the file.
func applicableICSObservance(comp *ical.Component, kind string, date time.Time) (*ical.Component, error) {
	var selected, earliest *ical.Component
	var selectedAt, earliestAt time.Time
	for _, obs := range comp.Children {
		if obs.Name != kind {
			continue
		}
		start := obs.Props.Get("DTSTART")
		if start == nil {
			return nil, fmt.Errorf("VTIMEZONE %s missing DTSTART", kind)
		}
		at, err := time.Parse("20060102T150405", start.Value)
		if err != nil {
			return nil, fmt.Errorf("VTIMEZONE %s DTSTART: %w", kind, err)
		}
		if earliest == nil || at.Before(earliestAt) {
			earliest, earliestAt = obs, at
		}
		if !at.After(date) && (selected == nil || at.After(selectedAt)) {
			selected, selectedAt = obs, at
		}
	}
	if selected == nil {
		selected = earliest
	}
	return selected, nil
}
