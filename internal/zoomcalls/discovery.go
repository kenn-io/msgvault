package zoomcalls

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"time"
)

func dateWindows(from, to string, now time.Time) ([][2]time.Time, error) {
	a, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return nil, errors.New("zoom --from must be YYYY-MM-DD")
	}
	b, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return nil, errors.New("zoom --to must be YYYY-MM-DD (exclusive)")
	}
	today := now.UTC().Truncate(24 * time.Hour)
	if !a.Before(b) || b.Sub(a) > 31*24*time.Hour || a.Before(today.AddDate(0, -6, 0)) || b.After(today.AddDate(0, 0, 1)) {
		return nil, errors.New("zoom window must be 1–31 days within the past six months")
	}
	var out [][2]time.Time
	for a.Before(b) {
		end := time.Date(a.Year(), a.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		if b.Before(end) {
			end = b
		}
		out = append(out, [2]time.Time{a, end})
		a = end
	}
	return out, nil
}

// Meetings discovers only hosted past meeting instances and stages their full
// rosters before callers write to the archive. Webinars and PAC are skipped.
func (c *Client) Meetings(ctx context.Context, owner, from, to string) ([]MeetingRecord, int, error) {
	if owner == "" {
		return nil, 0, errors.New("zoom owner ID required")
	}
	windows, err := dateWindows(from, to, time.Now())
	if err != nil {
		return nil, 0, err
	}
	var records []MeetingRecord
	seen := map[string]bool{}
	skipped, total := 0, 0
	for _, window := range windows {
		q := url.Values{"from": {window[0].Format(time.DateOnly)}, "to": {window[1].AddDate(0, 0, -1).Format(time.DateOnly)}, "type": {"past"}, "page_size": {"100"}}
		pages, err := c.pages(ctx, "/report/users/"+url.PathEscape(owner)+"/meetings", "meetings", q, 100)
		if err != nil {
			return nil, skipped, err
		}
		for _, raw := range pages {
			var page struct {
				Meetings []struct {
					UUID  string    `json:"uuid"`
					Type  int       `json:"type"`
					Start time.Time `json:"start_time"`
				} `json:"meetings"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return nil, skipped, errors.New("invalid Zoom meeting report")
			}
			for _, m := range page.Meetings {
				if m.Type != 1 && m.Type != 2 && m.Type != 3 && m.Type != 4 && m.Type != 8 {
					skipped++
					continue
				}
				if m.UUID == "" || m.Start.IsZero() {
					return nil, skipped, errors.New("missing Zoom report instance or start time")
				}
				if m.Start.Before(window[0]) || !m.Start.Before(window[1]) {
					continue
				}
				if seen[m.UUID] {
					continue
				}
				seen[m.UUID] = true
				if len(records) >= 100 {
					return nil, skipped, errors.New("zoom meeting limit exceeded")
				}
				roster, err := c.Meeting(ctx, m.UUID)
				if err != nil {
					return nil, skipped, err
				}
				total += len(roster)
				if total > 32<<20 {
					return nil, skipped, errors.New("zoom staging exceeds 32 MiB")
				}
				records = append(records, MeetingRecord{UUID: m.UUID, Roster: roster})
			}
		}
	}
	return records, skipped, nil
}
