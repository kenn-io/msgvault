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

// Reserve a frequency-aware candidate bound per rule, plus DTSTART/RDATE and
// a zone margin. This accounts for excluded and duplicate pulls without
// charging weekly or less frequent series once for every elapsed day.
// Unsupported selectors are rejected by ParseSet before reaching this
// calculation.
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
		ruleLimit := linkRuleCandidateBound(rule, days)
		allowance += min(ruleLimit, calrecur.SeriesReadBudget-allowance)
	}
	if err := spendLinkBudget(ctx, budget, allowance); err != nil {
		return 0, err
	}
	return allowance, nil
}

func linkRuleCandidateBound(rule string, days int) int {
	frequency := ""
	interval, count := 1, 0
	byDay, byMonthDay, byMonth, bySetPos := 0, 0, 0, 0
	for part := range strings.SplitSeq(strings.TrimPrefix(rule, "RRULE:"), ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "FREQ":
			frequency = strings.ToUpper(value)
		case "INTERVAL":
			interval, _ = strconv.Atoi(value)
		case "COUNT":
			count, _ = strconv.Atoi(value)
		case "BYDAY":
			byDay = len(strings.Split(value, ","))
		case "BYMONTHDAY":
			byMonthDay = len(strings.Split(value, ","))
		case "BYMONTH":
			byMonth = len(strings.Split(value, ","))
		case "BYSETPOS":
			bySetPos = len(strings.Split(value, ","))
		}
	}
	if interval < 1 {
		interval = 1
	}

	var candidates int
	switch frequency {
	case "DAILY":
		candidates = ceilLinkDiv(days, interval) + 1
	case "WEEKLY":
		candidates = (ceilLinkDiv(days, 7)/interval + 1) * max(1, min(7, byDay))
	case "MONTHLY":
		perMonth := 1
		if byMonthDay > 0 {
			perMonth = min(31, byMonthDay)
		}
		if byDay > 0 {
			perMonth = min(31, 5*min(7, byDay))
		}
		if bySetPos > 0 {
			perMonth = min(perMonth, bySetPos)
		}
		candidates = (ceilLinkDiv(days, 28)/interval + 1) * perMonth
	case "YEARLY":
		perMonth := 1
		if byMonthDay > 0 {
			perMonth = min(31, byMonthDay)
		}
		if byDay > 0 {
			perMonth = min(31, 5*min(7, byDay))
		}
		if bySetPos > 0 {
			perMonth = min(perMonth, bySetPos)
		}
		months := 1
		if byMonth > 0 {
			months = min(12, byMonth)
		}
		candidates = (ceilLinkDiv(days, 365)/interval + 1) * months * perMonth
	default:
		// ParseSet rejects unsupported frequencies. Keep a conservative bound if
		// a future caller constructs a Set without passing it through ParseSet.
		candidates = days
	}
	if count > 0 {
		candidates = min(candidates, count)
	}
	return max(1, candidates)
}

func ceilLinkDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return 1 + (value-1)/divisor
}
