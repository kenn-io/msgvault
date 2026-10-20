package calevent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/calrecur"
)

// An unbounded series is verified over a Gregorian cycle and returns no rule.
func icsEventVerificationRule(value string, event Event, loc *time.Location, startWall time.Time) (rule *rrule.RRule, unbounded bool, err error) {
	ruleLoc := loc
	if event.AllDay {
		ruleLoc = time.UTC
	}
	option, err := rrule.StrToROptionInLocation(value, ruleLoc)
	if err != nil {
		return nil, false, fmt.Errorf("cannot verify recurrence rule: %w", err)
	}
	// The verification iterator uses civil fields in UTC. Reuse expansion's
	// rule checks before applying work limits or iterating unsupported rules.
	if _, err := calrecur.ParseSet(startWall, event.AllDay, startWall.Format(time.DateOnly), time.UTC, []string{"RRULE:" + value}); err != nil {
		return nil, false, fmt.Errorf("cannot verify recurrence rule: %w", err)
	}
	if option.Count > maxICSTimezoneTransitions || icsRuleCandidatesExceedBudget(option) {
		return nil, false, errors.New("recurrence exceeds the VTIMEZONE verification budget")
	}
	option.Dtstart = startWall
	if !event.AllDay && !option.Until.IsZero() {
		option.Until = icsCivilTime(option.Until.In(loc))
	}
	if option.Count == 0 && option.Until.IsZero() {
		return nil, true, nil
	}
	if option.Count == 0 && !option.Until.IsZero() && option.Freq != rrule.YEARLY && option.Until.Year()-startWall.Year() > 400 {
		return nil, false, errors.New("recurrence exceeds the VTIMEZONE verification budget")
	}
	rule, err = rrule.NewRRule(*option)
	if err != nil {
		return nil, false, fmt.Errorf("cannot verify recurrence rule: %w", err)
	}
	return rule, false, nil
}

// Count every occurrence read, including those before the event window.
func icsLastRuleWall(ctx context.Context, rule *rrule.RRule, budget *ICSParseBudget) (time.Time, error) {
	var last time.Time
	next := rule.Iterator()
	for reads := 0; ; reads++ {
		wall, ok, err := budget.nextVerification(ctx, next)
		if err != nil {
			return time.Time{}, err
		}
		if !ok || wall.After(icsMaximumCivilDate()) {
			return last, nil
		}
		if reads >= maxICSTimezoneTransitions {
			return time.Time{}, errors.New("recurrence exceeds the VTIMEZONE verification budget")
		}
		last = wall
	}
}

func icsObservanceRule(kind, value string, onset, end time.Time, offsetFrom int) (*rrule.RRule, error) {
	option, err := rrule.StrToROptionInLocation(value, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("VTIMEZONE %s RRULE: %w", kind, err)
	}
	if option.Freq != rrule.YEARLY {
		return nil, fmt.Errorf("VTIMEZONE %s RRULE must use FREQ=YEARLY", kind)
	}
	if option.Interval < 0 || option.Interval > 400 || option.Count < 0 || option.Count > maxICSTimezoneTransitions {
		return nil, fmt.Errorf("VTIMEZONE %s RRULE INTERVAL or COUNT exceeds the verification budget", kind)
	}
	if icsRuleCandidatesExceedBudget(option) {
		return nil, fmt.Errorf("VTIMEZONE %s RRULE exceeds the transition verification budget", kind)
	}
	option.Dtstart = onset
	if !option.Until.IsZero() {
		// Observance DTSTART uses wall-clock fields; UNTIL is a UTC instant.
		option.Until = option.Until.Add(time.Duration(offsetFrom) * time.Second)
	}
	if option.Until.IsZero() || option.Until.After(end) {
		option.Until = end
	}
	rule, err := rrule.NewRRule(*option)
	if err != nil {
		return nil, fmt.Errorf("VTIMEZONE %s RRULE: %w", kind, err)
	}
	return rule, nil
}

// The library buffers a period's candidates before yielding an occurrence.
// A year has at most 366 candidate days, regardless of the BYDAY/BYMONTHDAY lists.
func icsRuleCandidatesExceedBudget(option *rrule.ROption) bool {
	clocks := max(1, len(option.Byhour)) * max(1, len(option.Byminute)) * max(1, len(option.Bysecond))
	return clocks > maxICSTimezoneTransitions/366
}

func icsExpandObservanceRule(ctx context.Context, kind, value string, onset, start, end time.Time, offsetFrom int, walls map[int64]time.Time, budget *ICSParseBudget) error {
	rule, err := icsObservanceRule(kind, value, onset, end, offsetFrom)
	if err != nil {
		return err
	}
	var previous time.Time
	next := rule.Iterator()
	for reads := 0; ; reads++ {
		wall, ok, err := budget.nextVerification(ctx, next)
		if err != nil {
			return err
		}
		if !ok || wall.After(end) {
			break
		}
		if reads >= maxICSTimezoneTransitions || len(walls) >= maxICSTimezoneTransitions {
			return errors.New("VTIMEZONE exceeds the transition verification budget")
		}
		if !wall.After(start) {
			previous = wall
		} else {
			walls[wall.Unix()] = wall
		}
	}
	if !previous.IsZero() {
		walls[previous.Unix()] = previous
	}
	return nil
}
