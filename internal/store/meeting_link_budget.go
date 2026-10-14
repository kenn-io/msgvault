package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

func spendLinkBudget(ctx context.Context, budget *int, cost int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget == nil || *budget < cost {
		return ErrQueryTooBroad
	}
	*budget -= cost
	return nil
}

// ParseSet's supported rules yield at most one candidate per civil day per
// rule. Reserve that upper bound, plus DTSTART/RDATE and a zone margin. This
// accounts for excluded and duplicate pulls without charging every short
// series the entire 50,000-candidate allowance. Unsupported selectors are
// rejected by ParseSet before reaching this calculation.
func reserveLinkRecurrence(ctx context.Context, set calrecur.Set, until time.Time, budget *int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	start := set.DTStart
	if set.AllDay {
		zone := set.Zone
		if zone == nil {
			zone = time.UTC
		}
		var err error
		start, err = time.ParseInLocation("2006-01-02", set.StartDate, zone)
		if err != nil {
			return 0, fmt.Errorf("parse all-day recurrence start: %w", err)
		}
	}
	days := max(0, int(until.Sub(start)/(24*time.Hour))) + 3
	allowance := min(calrecur.SeriesReadBudget, 1+len(set.RDates))
	for _, rule := range set.RRules {
		ruleLimit := days
		for part := range strings.SplitSeq(strings.TrimPrefix(rule, "RRULE:"), ";") {
			key, value, ok := strings.Cut(part, "=")
			if ok && key == "COUNT" {
				if count, err := strconv.Atoi(value); err == nil && count > 0 {
					ruleLimit = min(ruleLimit, count)
				}
			}
		}
		allowance += min(ruleLimit, calrecur.SeriesReadBudget-allowance)
	}
	if err := spendLinkBudget(ctx, budget, allowance); err != nil {
		return 0, err
	}
	return allowance, nil
}
