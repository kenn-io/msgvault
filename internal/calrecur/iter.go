package calrecur

import (
	"context"
	"errors"
	"slices"
	"time"
)

type Instance struct {
	Identity Identity
	// Start is the first valid instant of the identity's local civil date for
	// all-day instances, or the recurrence instant for timed instances.
	Start time.Time
}

// IterStats reports work performed by one recurrence scan, including work
// retained when the scan stops early or returns an error.
type IterStats struct {
	// Candidates counts successful raw DTSTART/RDATE and RRULE pulls, including
	// duplicates, exclusions, nonexistent local times and prefetched lookahead.
	// It includes the candidate that exceeds budget; exhausted pulls and
	// detached exception rows are not counted.
	Candidates int
}

// IterWithStats behaves like Iter and reports actual candidate work before
// deduplication and exclusion filtering, even on cancellation or budget errors.
func IterWithStats(ctx context.Context, s Set, until time.Time, budget int, yield func(Instance) bool) (IterStats, error) {
	counter := &candidateBudget{ctx: ctx, limit: budget}
	err := iter(ctx, s, until, counter, yield)
	return IterStats{Candidates: counter.used}, err
}

type stream struct {
	next   func() (time.Time, bool, error)
	head   time.Time
	active bool
}

// Iter emits the ascending, deduplicated recurrence union minus exclusions.
// Returning false from yield stops successfully. The inclusive until horizon
// limits every rule. Each candidate counts toward budget, including duplicates, exclusions
// and those before the caller's window.
func Iter(ctx context.Context, s Set, until time.Time, budget int, yield func(Instance) bool) error {
	_, err := IterWithStats(ctx, s, until, budget, yield)
	return err
}

func iter(ctx context.Context, s Set, until time.Time, budgetCounter *candidateBudget, yield func(Instance) bool) error {
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
	for _, line := range s.RRules {
		if err := ctx.Err(); err != nil {
			return err
		}
		rule, err := s.ruleIterator(line, ruleUntil, budgetCounter)
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
