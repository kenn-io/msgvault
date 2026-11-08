package notionmeetings

import "time"

// MeetingFilter follows the meeting-query filter schema, which differs from data-source filters.
type MeetingFilter struct {
	Operator string           `json:"operator,omitempty"`
	Filters  []*MeetingFilter `json:"filters,omitempty"`
	Property string           `json:"property,omitempty"`
	Filter   *dateComparison  `json:"filter,omitempty"`
}
type dateComparison struct {
	Operator string    `json:"operator"`
	Value    exactDate `json:"value"`
}
type exactDate struct {
	Type  string     `json:"type"`
	Value minuteDate `json:"value"`
}
type minuteDate struct {
	Type      string `json:"type"`
	StartDate string `json:"start_date"`
	StartTime string `json:"start_time"`
	TimeZone  string `json:"time_zone"`
}
type queryWindow struct {
	From time.Time `json:"from,omitzero"`
	To   time.Time `json:"to,omitzero"`
}

func (w queryWindow) filter() *MeetingFilter {
	var filters []*MeetingFilter
	for _, bound := range []struct {
		at time.Time
		op string
	}{{w.From, "date_is_on_or_after"}, {w.To, "date_is_before"}} {
		if bound.at.IsZero() {
			continue
		}
		utc := bound.at.UTC()
		filters = append(filters, &MeetingFilter{Property: "created_time", Filter: &dateComparison{Operator: bound.op, Value: exactDate{Type: "exact", Value: minuteDate{Type: "datetime", StartDate: utc.Format("2006-01-02"), StartTime: utc.Format("15:04"), TimeZone: "UTC"}}}})
	}
	if len(filters) == 0 {
		return nil
	}
	return &MeetingFilter{Operator: "and", Filters: filters}
}

// split retains an open historical tail and uses only documented minute precision.
func (w queryWindow) split(results []MeetingNote) ([]queryWindow, bool) {
	var pivot time.Time
	if w.From.IsZero() || w.To.IsZero() {
		for _, m := range results {
			at, err := time.Parse(time.RFC3339, m.CreatedTime)
			if err == nil && (pivot.IsZero() || at.Before(pivot)) {
				pivot = at
			}
		}
		if pivot.IsZero() {
			return nil, false
		}
		pivot = pivot.UTC().Truncate(24 * time.Hour)
		if !w.From.IsZero() && !pivot.After(w.From) {
			pivot = w.From.Add(time.Minute)
		}
		if !w.To.IsZero() && !pivot.Before(w.To) {
			pivot = w.To.Add(-time.Minute)
		}
	} else {
		span := w.To.Sub(w.From)
		if span <= time.Minute {
			return nil, false
		}
		pivot = w.From.Add(span / 2).UTC().Truncate(time.Minute)
		if span > 24*time.Hour {
			day := pivot.Truncate(24 * time.Hour)
			if day.After(w.From) {
				pivot = day
			}
		}
	}
	if (!w.From.IsZero() && !pivot.After(w.From)) || (!w.To.IsZero() && !pivot.Before(w.To)) {
		return nil, false
	}
	return []queryWindow{{From: w.From, To: pivot}, {From: pivot, To: w.To}}, true
}
