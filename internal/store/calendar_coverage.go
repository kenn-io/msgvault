package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// CalendarCoverage describes scoped archive evidence, never live availability.
type CalendarCoverage struct {
	Complete bool                     `json:"complete"`
	Notice   string                   `json:"notice"`
	Sources  []CalendarSourceCoverage `json:"sources"`
}

type CalendarSourceCoverage struct {
	SourceID        int64      `json:"source_id"`
	SourceType      string     `json:"source_type"`
	Status          string     `json:"status" enum:"complete,stale,sync_failed,no_baseline,partial_evidence"`
	AsOf            *time.Time `json:"as_of"`
	LatestRunStatus string     `json:"latest_run_status,omitempty"`
}

const calendarCoverageNotice = "Archived calendar data, not live availability. An empty or short result does not prove free time: only registered calendars are archived, and coverage may be stale or incomplete (see sources). Use live free/busy for scheduling."

func calendarCoverageContext(ctx context.Context, s *Store, r SQLReader, evidence map[int64]bool, unexpandable bool, now time.Time) (CalendarCoverage, error) {
	coverage := CalendarCoverage{Notice: calendarCoverageNotice, Sources: []CalendarSourceCoverage{}}
	scoped, args := sourceScopeClause(ctx, "s.id")
	candidates := "s.source_type IN ('gcal','caldav')"
	if len(evidence) > 0 {
		ids := make([]int64, 0, len(evidence))
		for id := range evidence {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		candidates += " OR s.id IN (" + placeholders(len(ids)) + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	// Both correlated lookups use the existing source/started_at index. A bounded
	// completion cannot replace a baseline, or hide a failure after that baseline.
	query := `SELECT s.id,s.source_type,COALESCE(s.sync_cursor,'')<>'',
  COALESCE(latest.status,''),baseline.completed_at,
  EXISTS(SELECT 1 FROM sync_runs failed WHERE failed.source_id=s.id
   AND failed.status IN ('failed','cancelled')
   AND (failed.started_at>baseline.started_at OR (failed.started_at=baseline.started_at AND failed.id>baseline.id)))
 FROM sources s
 LEFT JOIN sync_runs latest ON latest.id=(SELECT id FROM sync_runs WHERE source_id=s.id ORDER BY started_at DESC,id DESC LIMIT 1)
 LEFT JOIN sync_runs baseline ON baseline.id=(SELECT id FROM sync_runs WHERE source_id=s.id AND status='completed' AND COALESCE(cursor_after,'')<>'' ORDER BY started_at DESC,id DESC LIMIT 1)
 WHERE 1=1` + scoped + `
 AND NOT EXISTS (SELECT 1 FROM source_settings merged
  WHERE merged.source_id=s.id AND merged.merged_into_source_id IS NOT NULL)
 AND (` + candidates + `) ORDER BY s.id`
	rows, err := r.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return CalendarCoverage{}, fmt.Errorf("read calendar coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hasCalendar := false
	coverage.Complete = !unexpandable
	for rows.Next() {
		var c CalendarSourceCoverage
		var hasCursor bool
		var asof sql.NullTime
		var failed bool
		if err := rows.Scan(&c.SourceID, &c.SourceType, &hasCursor, &c.LatestRunStatus, &asof, &failed); err != nil {
			return CalendarCoverage{}, fmt.Errorf("scan calendar coverage: %w", err)
		}
		if c.SourceType != "gcal" && c.SourceType != "caldav" {
			c.Status = "partial_evidence"
		} else {
			hasCalendar = true
			if asof.Valid {
				at := asof.Time.UTC()
				c.AsOf = &at
			}
			switch {
			case !hasCursor || !asof.Valid:
				c.Status = "no_baseline"
			case failed:
				c.Status = "sync_failed"
			case now.Sub(asof.Time) > 24*time.Hour:
				c.Status = "stale"
			default:
				c.Status = "complete"
			}
		}
		if c.Status != "complete" {
			coverage.Complete = false
		}
		coverage.Sources = append(coverage.Sources, c)
	}
	if err := rows.Err(); err != nil {
		return CalendarCoverage{}, fmt.Errorf("iterate calendar coverage: %w", err)
	}
	coverage.Complete = coverage.Complete && hasCalendar
	return coverage, nil
}
