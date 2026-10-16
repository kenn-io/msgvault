package calrecur

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// Exception overrides a generated instance at Original, preserving its identity.
// Any date bound makes the effective event all-day; otherwise any instant bound
// makes it timed. With no bounds, the master's type and bounds are inherited.
// All-day endpoints use their date, instant's local date, or nominal local date,
// in that order. Missing timed endpoints inherit their nominal instants.
type Exception struct {
	Original           Identity
	Start, End         *time.Time
	StartDate, EndDate string
	Cancelled          bool
	Ref                int64
}

type Occurrence struct {
	Original                       Identity
	Start, End                     time.Time
	StartDate, EndDate             string
	AllDay, Cancelled, BoundsKnown bool
	ExceptionRef                   int64
}

// ExpandWithStats behaves like Expand and reports its generation scan's raw
// candidate work before exclusion, deduplication and exception suppression.
// Detached exception rows are separate caller-owned work.
func ExpandWithStats(ctx context.Context, s Set, duration time.Duration, durationDays int, exc []Exception, from, to time.Time, budget int) ([]Occurrence, IterStats, error) {
	var stats IterStats
	out, err := expand(ctx, s, duration, durationDays, exc, from, to, budget, &stats)
	return out, stats, err
}

// ConservativeEndWithStats behaves like ConservativeEnd and reports its raw
// scan work, including excluded candidates and work before an unknown bound.
func ConservativeEndWithStats(ctx context.Context, s Set, duration time.Duration, durationDays int, budget int) (time.Time, bool, IterStats) {
	counter := &candidateBudget{ctx: ctx, limit: budget}
	end, bounded := conservativeEnd(ctx, s, duration, durationDays, counter)
	return end, bounded, IterStats{Candidates: counter.used}
}

func nominal(s Set, id Identity, duration time.Duration, durationDays int) (Occurrence, error) {
	start, err := s.identityStart(id)
	if err != nil {
		return Occurrence{}, unsupported("original identity", err)
	}
	o := Occurrence{Original: id, Start: start, End: start.Add(duration), AllDay: s.AllDay, BoundsKnown: true}
	if s.AllDay {
		o.Start, err = s.instanceStart(start)
		if err != nil {
			return Occurrence{}, unsupported("occurrence start date", err)
		}
		o.StartDate = id.Date
		o.EndDate, err = addCivilDays(id.Date, durationDays)
		if err != nil {
			return Occurrence{}, unsupported("occurrence end date", err)
		}
		o.End, _, err = localDateBoundary(o.EndDate, s.location())
		if err != nil {
			return Occurrence{}, unsupported("occurrence end date", err)
		}
	}
	return o, nil
}

func effective(s Set, e Exception, duration time.Duration, durationDays int) (Occurrence, error) {
	start, err := s.identityStart(e.Original)
	if err != nil {
		return Occurrence{}, unsupported("original identity", err)
	}
	o := Occurrence{Original: e.Original, AllDay: s.AllDay, BoundsKnown: true}
	if e.StartDate != "" || e.EndDate != "" {
		o.AllDay = true
	} else if e.Start != nil || e.End != nil {
		o.AllDay = false
	}
	if o.AllDay {
		if s.AllDay {
			o.StartDate = e.Original.Date
			o.EndDate, err = addCivilDays(e.Original.Date, durationDays)
		} else {
			o.StartDate = start.In(s.location()).Format(dateLayout)
			o.EndDate = start.Add(duration).In(s.location()).Format(dateLayout)
		}
		if err != nil {
			return Occurrence{}, unsupported("exception nominal dates", err)
		}
		if e.Start != nil {
			o.StartDate = e.Start.In(s.location()).Format(dateLayout)
		}
		if e.End != nil {
			o.EndDate = e.End.In(s.location()).Format(dateLayout)
		}
		if e.StartDate != "" {
			o.StartDate = e.StartDate
		}
		if e.EndDate != "" {
			o.EndDate = e.EndDate
		}
		o.Start, err = localDateStart(o.StartDate, s.location())
		if err != nil {
			return Occurrence{}, unsupported("exception start date", err)
		}
		o.End, _, err = localDateBoundary(o.EndDate, s.location())
		if err != nil {
			return Occurrence{}, unsupported("exception end date", err)
		}
	} else {
		if s.AllDay {
			o.Start = start
			o.EndDate, err = addCivilDays(e.Original.Date, durationDays)
			if err != nil {
				return Occurrence{}, unsupported("exception nominal end date", err)
			}
			if e.Start == nil {
				o.Start, err = localDateStart(e.Original.Date, s.location())
				if err != nil {
					return Occurrence{}, unsupported("exception nominal start date", err)
				}
			}
			if e.End == nil {
				o.End, _, err = localDateBoundary(o.EndDate, s.location())
				if err != nil {
					return Occurrence{}, unsupported("exception nominal end date", err)
				}
			}
		} else {
			o.Start = start
			o.End = start.Add(duration)
		}
		if e.Start != nil {
			o.Start = *e.Start
		}
		if e.End != nil {
			o.End = *e.End
		}
		o.StartDate, o.EndDate = "", ""
	}
	// Civil ordering remains meaningful when a time zone skips an entire date.
	if o.AllDay && o.EndDate < o.StartDate {
		return Occurrence{}, unsupported("exception bounds", errors.New("end date precedes start date"))
	}
	if o.End.Before(o.Start) {
		return Occurrence{}, unsupported("exception bounds", errors.New("end precedes start"))
	}
	o.Cancelled, o.ExceptionRef = e.Cancelled, e.Ref
	return o, nil
}

// Expand returns effective occurrences overlapping the half-open [from,to)
// window, sorted by effective start and original identity. Exceptions are
// considered independently so a move can enter a window beyond the series end.
// A generation error returns no partial result.
func Expand(ctx context.Context, s Set, duration time.Duration, durationDays int, exc []Exception, from, to time.Time, budget int) ([]Occurrence, error) {
	out, _, err := ExpandWithStats(ctx, s, duration, durationDays, exc, from, to, budget)
	return out, err
}

func expand(ctx context.Context, s Set, duration time.Duration, durationDays int, exc []Exception, from, to time.Time, budget int, stats *IterStats) ([]Occurrence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if duration < 0 || durationDays < 0 {
		return nil, unsupported("duration", errors.New("duration must not be negative"))
	}
	if !to.After(from) {
		return nil, nil
	}
	overrides := map[string]Exception{}
	for _, e := range exc {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		overrides[e.Original.Key()] = e
	}
	var out []Occurrence
	add := func(o Occurrence) {
		if o.Start.Before(to) && o.End.After(from) {
			out = append(out, o)
		}
	}
	for _, e := range overrides {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o, err := effective(s, e, duration, durationDays)
		if err != nil {
			return nil, err
		}
		add(o)
	}
	iterationSet := s
	if len(overrides) > 0 {
		iterationSet.ExDates = slices.Clone(s.ExDates)
		for _, e := range overrides {
			iterationSet.ExDates = append(iterationSet.ExDates, e.Original)
		}
	}
	var boundsErr error
	// Iter's horizon is inclusive; step back one nanosecond to preserve Expand's half-open window.
	work, err := IterWithStats(ctx, iterationSet, to.Add(-time.Nanosecond), budget, func(i Instance) bool {
		if !i.Start.Before(to) {
			return false
		}
		if _, ok := overrides[i.Identity.Key()]; ok {
			return true
		}
		o, err := nominal(s, i.Identity, duration, durationDays)
		if err != nil {
			boundsErr = err
			return false
		}
		add(o)
		return true
	})
	*stats = work
	if err != nil {
		return nil, err
	}
	if boundsErr != nil {
		return nil, boundsErr
	}
	slices.SortFunc(out, func(a, b Occurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Original.Key(), b.Original.Key())
	})
	return out, nil
}

// Contains checks original identity, rather than effective start. A detached
// exception establishes existence even when cancelled or excluded by EXDATE.
func Contains(ctx context.Context, s Set, exc []Exception, original Identity, budget int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	target, err := s.identityStart(original)
	if err != nil {
		return false, unsupported("original identity", err)
	}
	for _, e := range exc {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if e.Original.AllDay == original.AllDay && e.Original.Key() == original.Key() {
			return true, nil
		}
	}
	if original.AllDay {
		target, _, err = localDateBoundary(original.Date, s.location())
		if err != nil {
			return false, unsupported("original identity", err)
		}
	}
	iterationSet := s
	if len(exc) > 0 {
		iterationSet.ExDates = slices.Clone(s.ExDates)
		for _, e := range exc {
			iterationSet.ExDates = append(iterationSet.ExDates, e.Original)
		}
	}
	found := false
	err = Iter(ctx, iterationSet, target, budget, func(i Instance) bool {
		found = i.Identity.Key() == original.Key()
		return !found && i.Start.Before(target)
	})
	return found, err
}

// ConservativeEnd includes DTSTART and RDATEs even when excluded. It bounds
// only the generated recurrence set; callers bounding effective occurrences
// must also account for moved exceptions' effective bounds. Unbounded rules,
// malformed input, context cancellation or exhausted budget make the bound
// unknown. All-day ends use civil days and a 14-hour conservative margin.
func ConservativeEnd(ctx context.Context, s Set, duration time.Duration, durationDays int, budget int) (end time.Time, bounded bool) {
	end, bounded, _ = ConservativeEndWithStats(ctx, s, duration, durationDays, budget)
	return end, bounded
}

func conservativeEnd(ctx context.Context, s Set, duration time.Duration, durationDays int, budgetCounter *candidateBudget) (end time.Time, bounded bool) {
	if duration < 0 || durationDays < 0 || ctx.Err() != nil {
		return time.Time{}, false
	}
	start, err := s.start()
	if err != nil {
		return time.Time{}, false
	}
	add := func(start time.Time) bool {
		if ctx.Err() != nil {
			return false
		}
		candidate := start.Add(duration)
		if s.AllDay {
			endDate, err := addCivilDays(start.UTC().Format(dateLayout), durationDays)
			if err != nil {
				return false
			}
			candidate, _, err = localDateBoundary(endDate, s.location())
			if err != nil {
				return false
			}
			candidate = candidate.Add(14 * time.Hour)
		}
		if candidate.After(end) {
			end = candidate
		}
		return true
	}
	if budgetCounter.charge() != nil || !add(start) {
		return time.Time{}, false
	}
	for _, id := range s.RDates {
		start, err := s.identityStart(id)
		if err != nil || budgetCounter.charge() != nil || !add(start) {
			return time.Time{}, false
		}
	}
	for _, id := range s.ExDates {
		if _, err := s.identityStart(id); err != nil {
			return time.Time{}, false
		}
	}
	for _, line := range s.RRules {
		if ctx.Err() != nil {
			return time.Time{}, false
		}
		rule, err := s.rule(line, time.Time{})
		if err != nil {
			return time.Time{}, false
		}
		opt := rule.OrigOptions
		if opt.Count == 0 && opt.Until.IsZero() {
			return time.Time{}, false
		}
		until := opt.Until
		if until.IsZero() {
			until = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		}
		iterator, err := s.ruleIterator(line, until, budgetCounter)
		if err != nil {
			return time.Time{}, false
		}
		for {
			if ctx.Err() != nil {
				return time.Time{}, false
			}
			start, ok, err := iterator.next()
			if err != nil {
				return time.Time{}, false
			}
			if !ok {
				break
			}
			if !add(start) {
				return time.Time{}, false
			}
		}
		// COUNT without UNTIL must reach its count. The library's maximum year
		// cannot establish an end for a rule whose requested count was not reached.
		if opt.Until.IsZero() && iterator.generated < opt.Count {
			return time.Time{}, false
		}
	}
	if ctx.Err() != nil {
		return time.Time{}, false
	}
	return end, true
}
