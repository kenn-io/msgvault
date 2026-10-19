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
// Missing all-day dates are derived from effective bounds in Zone, or the series
// zone when Zone is nil. Missing timed endpoints inherit their nominal instants.
type Exception struct {
	Original           Identity
	Start, End         *time.Time
	StartDate, EndDate string
	Zone               *time.Location // Date-bound zone; nil inherits the recurrence set zone.
	Cancelled          bool
	Ref                int64
}

type Occurrence struct {
	Original           Identity
	Start, End         time.Time
	StartDate, EndDate string
	AllDay, Cancelled  bool
	ExceptionRef       int64
	HasException       bool
}

func nominal(s Set, id Identity, duration time.Duration, durationDays int) (Occurrence, error) {
	start, err := s.identityStart(id)
	if err != nil {
		return Occurrence{}, unsupported("original identity", err)
	}
	o := Occurrence{Original: id, Start: start, End: start.Add(duration), AllDay: s.AllDay}
	if s.AllDay {
		o.Start, _, err = s.instanceStart(start)
		if err != nil {
			return Occurrence{}, unsupported("occurrence start date", err)
		}
		o.StartDate = id.Date
		o.EndDate, err = addCivilDays(id.Date, durationDays)
		if err != nil {
			return Occurrence{}, unsupported("occurrence end date", err)
		}
		o.End, _, err = CivilDateBoundary(o.EndDate, s.location())
		if err != nil {
			return Occurrence{}, unsupported("occurrence end date", err)
		}
	}
	return o, nil
}

// EffectiveException resolves an archived exception without iterating its series.
func EffectiveException(s Set, e Exception, duration time.Duration, durationDays int) (Occurrence, error) {
	start, err := s.identityStart(e.Original)
	if err != nil {
		return Occurrence{}, unsupported("original identity", err)
	}
	zone := e.Zone
	if zone == nil {
		zone = s.location()
	}
	o := Occurrence{Original: e.Original, AllDay: s.AllDay}
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
			o.StartDate = start.In(zone).Format(dateLayout)
			o.EndDate = start.Add(duration).In(zone).Format(dateLayout)
		}
		if err != nil {
			return Occurrence{}, unsupported("exception nominal dates", err)
		}
		if e.Start != nil {
			o.StartDate = e.Start.In(zone).Format(dateLayout)
		}
		if e.End != nil {
			o.EndDate = e.End.In(zone).Format(dateLayout)
		}
		if e.StartDate != "" {
			o.StartDate = e.StartDate
		}
		if e.EndDate != "" {
			o.EndDate = e.EndDate
		}
		o.Start, _, err = CivilDateBoundary(o.StartDate, zone)
		if err != nil {
			return Occurrence{}, unsupported("exception start date", err)
		}
		o.End, _, err = CivilDateBoundary(o.EndDate, zone)
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
				o.Start, _, err = CivilDateBoundary(e.Original.Date, s.location())
				if err != nil {
					return Occurrence{}, unsupported("exception nominal start date", err)
				}
			}
			if e.End == nil {
				o.End, _, err = CivilDateBoundary(o.EndDate, s.location())
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
	o.HasException = true
	return o, nil
}

// Expand returns effective occurrences overlapping the half-open [from,to)
// window, sorted by effective start and original identity.
// A generation error returns no partial result.
func Expand(ctx context.Context, s Set, duration time.Duration, durationDays int, from, to time.Time, budget int) ([]Occurrence, int, error) {
	counter := &candidateBudget{ctx: ctx, limit: budget}
	out, err := expand(ctx, s, duration, durationDays, from, to, counter)
	return out, counter.used, err
}

func expand(ctx context.Context, s Set, duration time.Duration, durationDays int, from, to time.Time, counter *candidateBudget) ([]Occurrence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if duration < 0 || durationDays < 0 {
		return nil, unsupported("duration", errors.New("duration must not be negative"))
	}
	if !to.After(from) {
		return nil, nil
	}
	var out []Occurrence
	add := func(o Occurrence) {
		if o.Start.Before(to) && o.End.After(from) {
			out = append(out, o)
		}
	}
	var boundsErr error
	// Iter's horizon is inclusive; step back one nanosecond to preserve Expand's half-open window.
	err := iter(ctx, s, to.Add(-time.Nanosecond), counter, func(i instance) bool {
		o, err := nominal(s, i.Identity, duration, durationDays)
		if err != nil {
			boundsErr = err
			return false
		}
		add(o)
		return true
	})
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

// ConservativeEnd includes DTSTART and RDATEs even when excluded. It bounds
// only the generated recurrence set; callers bounding effective occurrences
// must also account for moved exceptions' effective bounds. Unbounded rules,
// malformed input, context cancellation or exhausted budget make the bound
// unknown. All-day ends use civil days and a 14-hour conservative margin.
func ConservativeEnd(ctx context.Context, s Set, duration time.Duration, durationDays int, budget int) (end time.Time, bounded bool) {
	return conservativeEnd(ctx, s, duration, durationDays, &candidateBudget{ctx: ctx, limit: budget})
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
			candidate, _, err = CivilDateBoundary(endDate, s.location())
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
	for _, opt := range s.rules {
		if ctx.Err() != nil {
			return time.Time{}, false
		}
		if opt.Count == 0 && opt.Until.IsZero() {
			return time.Time{}, false
		}
		until := opt.Until
		if until.IsZero() {
			until = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		}
		iterator, err := s.ruleIterator(opt, until, budgetCounter)
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

// Resolve confirms a generated original identity using the same steps as Expand.
func Resolve(ctx context.Context, s Set, duration time.Duration, durationDays int, id Identity, budget int) (Occurrence, bool, error) {
	if err := ctx.Err(); err != nil {
		return Occurrence{}, false, err
	}
	if id.AllDay != s.AllDay {
		return Occurrence{}, false, nil
	}
	if duration < 0 || durationDays < 0 {
		return Occurrence{}, false, unsupported("duration", errors.New("duration must not be negative"))
	}
	until, err := s.identityStart(id)
	if err != nil {
		return Occurrence{}, false, err
	}
	if s.AllDay {
		var exists bool
		until, exists, err = s.instanceStart(until)
		if err != nil {
			return Occurrence{}, false, unsupported("original date", err)
		}
		if !exists {
			return Occurrence{}, false, nil
		}
	}

	found := false
	err = iter(ctx, s, until, &candidateBudget{ctx: ctx, limit: budget}, func(i instance) bool {
		found = i.Identity.Key() == id.Key()
		return !found
	})
	if err != nil || !found {
		return Occurrence{}, false, err
	}
	o, err := nominal(s, id, duration, durationDays)
	return o, err == nil, err
}
