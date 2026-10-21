package calrecur

import (
	"context"
	"errors"
	"slices"
	"time"
)

type instance struct {
	Identity Identity
	// Start is the first valid instant of the identity's local civil date for
	// all-day instances, or the recurrence instant for timed instances.
	Start time.Time
}

type stream struct {
	next   func() (time.Time, bool, error)
	head   time.Time
	active bool
}

func iter(ctx context.Context, s Set, until time.Time, budgetCounter *candidateBudget, yield func(instance) bool) error {
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
		civilUntil := civilHorizon(until, s.location()).Format(dateLayout)
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
	dateNext := func() (time.Time, bool) {
		if index == len(dates) || dates[index].After(ruleUntil) {
			return time.Time{}, false
		}
		d := dates[index]
		index++
		return d, true
	}
	streams := []stream{{next: func() (time.Time, bool, error) { return budgetCounter.pull(dateNext) }}}
	for _, opt := range s.rules {
		if err := ctx.Err(); err != nil {
			return err
		}
		rule, err := s.ruleIterator(opt, ruleUntil, budgetCounter)
		if err != nil {
			return err
		}
		streams = append(streams, stream{next: rule.next})
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
	pull := func(st *stream) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		st.head, st.active, err = st.next()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
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
			instanceStart, exists, err := s.instanceStart(d)
			if err != nil {
				return unsupported("occurrence date", err)
			}
			if exists && instanceStart.After(until) {
				return nil
			}
			if exists && !yield(instance{Identity: id, Start: instanceStart}) {
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
