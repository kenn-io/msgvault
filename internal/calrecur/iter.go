package calrecur

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/teambition/rrule-go"
)

type Instance struct {
	Identity Identity
	// Start is the first valid instant of the identity's local civil date for
	// all-day instances, or the recurrence instant for timed instances.
	Start time.Time
}

type stream struct {
	next   rrule.Next
	head   time.Time
	active bool
}

// recurrence generates timed rules on floating civil fields before resolving
// them in the series zone. rrule-go advances local date anchors with AddDate,
// which can repeat a previous date when midnight is skipped; it also
// normalizes nonexistent local times and counts them toward COUNT. Resolving
// after generation preserves civil dates and lets COUNT include only valid
// local times.
func (s Set) recurrence(line string, until time.Time) (rrule.Next, error) {
	rule, err := s.rule(line, until)
	if err != nil {
		return nil, err
	}
	if s.AllDay {
		return rule.Iterator(), nil
	}

	options := rule.OrigOptions
	count := options.Count
	location := s.location()
	dtstart := options.Dtstart.In(location)
	startWall := floatingWallTime(dtstart)
	cutoff := options.Until
	options.Dtstart = startWall
	if !cutoff.IsZero() {
		// A UTC UNTIL can map to an earlier floating clock during a fall-back
		// transition. Generate through a conservative margin, then apply the
		// original cutoff to each resolved instant below.
		options.Until = floatingWallTime(cutoff.In(location).Add(48 * time.Hour))
	}
	options.Count = 0
	floatingRule, err := rrule.NewRRule(options)
	if err != nil {
		return nil, unsupported(line, err)
	}
	nextFloating := floatingRule.Iterator()
	generated := 0
	done := false
	return func() (time.Time, bool) {
		if done || count > 0 && generated >= count {
			return time.Time{}, false
		}
		for {
			wall, ok := nextFloating()
			if !ok {
				done = true
				return time.Time{}, false
			}
			start := dtstart
			if !wall.Equal(startWall) {
				var valid bool
				start, valid = resolveWallTime(wall, location)
				if !valid {
					continue
				}
			}
			if !cutoff.IsZero() && start.After(cutoff) {
				continue
			}
			generated++
			if count > 0 && generated >= count {
				done = true
			}
			return start, true
		}
	}, nil
}

func floatingWallTime(value time.Time) time.Time {
	year, month, day := value.Date()
	hour, minute, second := value.Clock()
	return time.Date(year, month, day, hour, minute, second, value.Nanosecond(), time.UTC)
}

func resolveWallTime(wall time.Time, location *time.Location) (time.Time, bool) {
	year, month, day := wall.Date()
	hour, minute, second := wall.Clock()
	resolved := time.Date(year, month, day, hour, minute, second, wall.Nanosecond(), location)
	matchesWall := func(candidate time.Time) bool {
		local := candidate.In(location)
		actualYear, actualMonth, actualDay := local.Date()
		actualHour, actualMinute, actualSecond := local.Clock()
		return actualYear == year && actualMonth == month && actualDay == day &&
			actualHour == hour && actualMinute == minute && actualSecond == second &&
			local.Nanosecond() == wall.Nanosecond()
	}
	if !matchesWall(resolved) {
		return time.Time{}, false
	}

	// For generated recurrences, choose the earliest matching instant when a
	// local wall time repeats during a fall-back transition.
	offsets := map[int]struct{}{}
	addOffset := func(value time.Time) {
		_, offset := value.In(location).Zone()
		offsets[offset] = struct{}{}
	}
	addOffset(resolved)
	start, end := resolved.In(location).ZoneBounds()
	if !start.IsZero() {
		addOffset(start.Add(-time.Nanosecond))
	}
	if !end.IsZero() {
		addOffset(end)
	}

	wallUTC := time.Date(year, month, day, hour, minute, second, wall.Nanosecond(), time.UTC)
	var earliest time.Time
	found := false
	for offset := range offsets {
		candidate := wallUTC.Add(-time.Duration(offset) * time.Second)
		if !matchesWall(candidate) {
			continue
		}
		if !found || candidate.Before(earliest) {
			earliest = candidate
			found = true
		}
	}
	return earliest, found
}

// Iter emits the ascending, deduplicated recurrence union minus exclusions.
// Returning false from yield stops successfully. The inclusive until horizon
// limits every rule. Each candidate counts toward budget, including duplicates, exclusions
// and those before the caller's window.
func Iter(ctx context.Context, s Set, until time.Time, budget int, yield func(Instance) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if until.IsZero() {
		return unsupported("UNTIL", errors.New("iteration requires a horizon"))
	}
	start, err := s.start()
	if err != nil {
		return unsupported("DTSTART", err)
	}
	ruleUntil := until
	if s.AllDay {
		civilUntil := until.In(s.location()).Format(dateLayout)
		ruleUntil, err = time.Parse(dateLayout, civilUntil)
		if err != nil {
			return unsupported("UNTIL", err)
		}
	}
	dates := []time.Time{start}
	for _, id := range s.RDates {
		if err := ctx.Err(); err != nil {
			return err
		}
		d, err := s.identityStart(id)
		if err != nil {
			return unsupported("RDATE", err)
		}
		dates = append(dates, d)
	}
	slices.SortFunc(dates, func(a, b time.Time) int { return a.Compare(b) })
	index := 0
	streams := []stream{{next: func() (time.Time, bool) {
		if index == len(dates) || dates[index].After(ruleUntil) {
			return time.Time{}, false
		}
		d := dates[index]
		index++
		return d, true
	}}}
	for _, line := range s.RRules {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := s.recurrence(line, ruleUntil)
		if err != nil {
			return err
		}
		streams = append(streams, stream{next: next})
	}
	excluded := map[string]bool{}
	for _, id := range s.ExDates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.identityStart(id); err != nil {
			return unsupported("EXDATE", err)
		}
		excluded[id.Key()] = true
	}
	count := 0
	pull := func(st *stream) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		st.head, st.active = st.next()
		if err := ctx.Err(); err != nil {
			return err
		}
		if st.active {
			count++
			if count > budget {
				return ErrBudgetExceeded
			}
		}
		return nil
	}
	for i := range streams {
		if err := pull(&streams[i]); err != nil {
			return err
		}
	}
	lastKey := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		selected := -1
		for i := range streams {
			if streams[i].active && (selected < 0 || streams[i].head.Before(streams[selected].head)) {
				selected = i
			}
		}
		if selected < 0 {
			return nil
		}
		d := streams[selected].head
		id := s.identity(d)
		key := id.Key()
		if key != lastKey && !excluded[key] {
			instanceStart, err := s.instanceStart(d)
			if err != nil {
				return unsupported("occurrence date", err)
			}
			if instanceStart.After(until) {
				return nil
			}
			if !yield(Instance{Identity: id, Start: instanceStart}) {
				return ctx.Err()
			}
		}
		lastKey = key
		// Advance every stream at this instant to remove duplicates before the next
		// candidate. Do this after yield so early termination avoids another scan.
		for i := range streams {
			if streams[i].active && streams[i].head.Equal(d) {
				if err := pull(&streams[i]); err != nil {
					return err
				}
			}
		}
	}
}
