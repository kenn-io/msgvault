package calrecur

import (
	"context"
	"time"

	"github.com/teambition/rrule-go"
)

type candidateBudget struct {
	ctx         context.Context
	limit, used int
}

func (b *candidateBudget) charge() error {
	b.used++
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.used > b.limit {
		return ErrBudgetExceeded
	}
	return nil
}

func (b *candidateBudget) pull(next rrule.Next) (time.Time, bool, error) {
	if err := b.ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	value, ok := next()
	if ok {
		if err := b.charge(); err != nil {
			return time.Time{}, false, err
		}
	} else if err := b.ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	return value, ok, nil
}

// civilTime represents local calendar fields in UTC, without resolving them
// through a local midnight that the time zone might skip.
func civilTime(t time.Time) time.Time {
	year, month, day := t.Date()
	hour, minute, second := t.Clock()
	return time.Date(year, month, day, hour, minute, second, t.Nanosecond(), time.UTC)
}

// civilHorizon includes wall times before a backward offset transition even
// when they compare later than the horizon's local clock. The original
// absolute horizon is checked again after resolving each candidate.
func civilHorizon(until time.Time, loc *time.Location) time.Time {
	until = until.In(loc)
	upper := civilTime(until)
	for cursor := until; ; {
		start, _ := cursor.ZoneBounds()
		if start.IsZero() || start.Before(until.Add(-72*time.Hour)) {
			return upper
		}
		cursor = start.Add(-time.Second).In(loc)
		if previous := civilTime(cursor); previous.After(upper) {
			upper = previous
		}
	}
}

// firstLocalWallTime resolves a UTC civil anchor to the earliest matching
// instant. A repeated wall time can have two valid instants during a fold.
func firstLocalWallTime(anchor time.Time, loc *time.Location) (time.Time, bool) {
	searchStart := anchor.Add(-72 * time.Hour).Unix()
	searchEnd := anchor.Add(72 * time.Hour).Unix()
	var earliest time.Time
	found := false
	for cursor := searchStart; cursor < searchEnd; {
		instant := time.Unix(cursor, 0).In(loc)
		_, offset := instant.Zone()
		_, zoneEnd := instant.ZoneBounds()
		intervalEnd := searchEnd
		if !zoneEnd.IsZero() && zoneEnd.Unix() < intervalEnd {
			intervalEnd = zoneEnd.Unix()
		}
		if intervalEnd <= cursor {
			return time.Time{}, false
		}

		candidateUnix := anchor.Unix() - int64(offset)
		if candidateUnix >= cursor && candidateUnix < intervalEnd {
			candidate := time.Unix(candidateUnix, int64(anchor.Nanosecond())).In(loc)
			if civilTime(candidate).Equal(anchor) && (!found || candidate.Before(earliest)) {
				earliest, found = candidate, true
			}
		}
		cursor = intervalEnd
	}
	return earliest, found
}

type ruleIterator struct {
	nextRaw      rrule.Next
	budget       *candidateBudget
	set          Set
	start, until time.Time
	count        int
	generated    int
	first        bool
}

func (s Set) ruleIterator(opt rrule.ROption, until time.Time, budget *candidateBudget) (*ruleIterator, error) {
	if !until.IsZero() && (opt.Until.IsZero() || until.Before(opt.Until)) {
		opt.Until = until
	}
	_, exists, err := s.instanceStart(opt.Dtstart)
	if err != nil {
		return nil, unsupported("DTSTART", err)
	}
	iterator := &ruleIterator{budget: budget, set: s, start: opt.Dtstart, until: opt.Until, count: opt.Count, first: exists}
	if !s.AllDay {
		opt.Dtstart = civilTime(opt.Dtstart.In(s.location()))
		if !s.startCivil.IsZero() {
			opt.Dtstart = s.startCivil
		}
		opt.Until = civilHorizon(opt.Until, s.location())
	}
	// COUNT applies to existing civil dates and wall times.
	opt.Count = 0
	rule, err := rrule.NewRRule(opt)
	if err != nil {
		return nil, unsupported("RRULE", err)
	}
	iterator.nextRaw = rule.Iterator()
	return iterator, nil
}

func (i *ruleIterator) next() (time.Time, bool, error) {
	if i.count > 0 && i.generated >= i.count {
		return time.Time{}, false, nil
	}
	for {
		anchor, ok, err := i.budget.pull(i.nextRaw)
		if err != nil || !ok {
			return time.Time{}, false, err
		}
		value := anchor
		if !i.set.AllDay {
			var exists bool
			value, exists = firstLocalWallTime(anchor, i.set.location())
			if anchor.Equal(i.set.startCivil) {
				value, exists = i.start, true
			}
			if !exists {
				continue // A nonexistent wall time must not consume COUNT.
			}
			if anchor.Equal(civilTime(i.start.In(i.set.location()))) {
				value = i.start // Preserve DTSTART's explicit fold occurrence.
			}
			if value.Before(i.start) {
				continue
			}
			if value.After(i.until) {
				return time.Time{}, false, nil
			}
		}
		if i.set.AllDay {
			_, exists, err := i.set.instanceStart(value)
			if err != nil {
				return time.Time{}, false, err
			}
			if !exists {
				continue
			}
		}
		if i.first {
			i.first = false
			if !value.Equal(i.start) {
				i.generated++
			}
		}
		if i.count > 0 && i.generated >= i.count {
			return time.Time{}, false, nil
		}
		i.generated++
		return value, true, nil
	}
}
