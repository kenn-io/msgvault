package calrecur

import (
	"context"
	"errors"
	"fmt"
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
	next   func() (time.Time, bool, int, error)
	head   time.Time
	active bool
}

func (s Set) instances(rule *rrule.RRule, absoluteUntil time.Time) (func() (time.Time, bool, int, error), error) {
	count := rule.OrigOptions.Count
	options := rule.OrigOptions
	options.Count = 0
	uncounted, err := rrule.NewRRule(options)
	if err != nil {
		return nil, fmt.Errorf("rebuild uncounted recurrence rule: %w", err)
	}
	next := uncounted.Iterator()
	var emitted int
	return func() (time.Time, bool, int, error) {
		if count > 0 && emitted >= count {
			return time.Time{}, false, 0, nil
		}
		consumed := 0
		for {
			wall, ok := next()
			if !ok {
				return time.Time{}, false, consumed, nil
			}
			consumed++
			if s.AllDay {
				_, exists, err := localDateBoundary(wall.UTC().Format(dateLayout), s.location())
				if err != nil {
					return time.Time{}, false, consumed, unsupported("occurrence date", err)
				}
				if !exists {
					continue
				}
				emitted++
				return wall, true, consumed, nil
			}
			actual, valid := s.resolveWallTime(wall)
			if !valid {
				continue
			}
			if !absoluteUntil.IsZero() && actual.After(absoluteUntil) {
				// The terminal candidate beyond the inclusive horizon is outside
				// this iteration's budget. Earlier nonexistent local times remain
				// charged because they were examined while resolving the horizon.
				return time.Time{}, false, consumed - 1, nil
			}
			emitted++
			return actual, true, consumed, nil
		}
	}, nil
}

func (s Set) resolveWallTime(wall time.Time) (time.Time, bool) {
	wall = wall.UTC()
	loc := s.location()
	var earliest time.Time
	probe := wall.Add(-48 * time.Hour)
	last := wall.Add(48 * time.Hour)
	for !probe.After(last) {
		_, offset := probe.In(loc).Zone()
		actual := wall.Add(-time.Duration(offset) * time.Second)
		local := actual.In(loc)
		if local.Year() == wall.Year() && local.Month() == wall.Month() && local.Day() == wall.Day() &&
			local.Hour() == wall.Hour() && local.Minute() == wall.Minute() && local.Second() == wall.Second() && local.Nanosecond() == wall.Nanosecond() &&
			(earliest.IsZero() || actual.Before(earliest)) {
			earliest = actual
		}
		_, end := probe.In(loc).ZoneBounds()
		if end.IsZero() || end.After(last) {
			break
		}
		if !end.After(probe) {
			probe = probe.Add(time.Second)
		} else {
			probe = end
		}
	}
	return earliest.In(loc), !earliest.IsZero()
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
	streams := []stream{{next: func() (time.Time, bool, int, error) {
		if index == len(dates) || dates[index].After(ruleUntil) {
			return time.Time{}, false, 0, nil
		}
		d := dates[index]
		index++
		return d, true, 1, nil
	}}}
	for _, line := range s.RRules {
		if err := ctx.Err(); err != nil {
			return err
		}
		ruleBound := until
		if s.AllDay {
			ruleBound = ruleUntil
		}
		rule, err := s.rule(line, ruleBound)
		if err != nil {
			return err
		}
		instanceUntil := recurrenceUntil(line, s.location())
		if instanceUntil.IsZero() || until.Before(instanceUntil) {
			instanceUntil = until
		}
		next, err := s.instances(rule, instanceUntil)
		if err != nil {
			return unsupported(line, err)
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
		var consumed int
		var err error
		st.head, st.active, consumed, err = st.next()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count += consumed
		if count > budget {
			return ErrBudgetExceeded
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
