package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CalendarCoverage describes scoped archive evidence, never live availability.
type CalendarCoverage struct {
	Complete bool                     `json:"complete"`
	Notice   string                   `json:"notice"`
	Sources  []CalendarSourceCoverage `json:"sources"`
}

type CalendarSourceCoverage struct {
	SourceID   int64      `json:"source_id"`
	SourceType string     `json:"source_type"`
	Name       string     `json:"name"`
	Status     string     `json:"status" enum:"complete,stale,sync_failed,no_baseline"`
	AsOf       *time.Time `json:"as_of"`
}

const (
	CalendarSourceCoverageStatusComplete   = "complete"
	CalendarSourceCoverageStatusStale      = "stale"
	CalendarSourceCoverageStatusSyncFailed = "sync_failed"
	CalendarSourceCoverageStatusNoBaseline = "no_baseline"
)

const calendarCoverageNotice = "Archived calendar data, not live availability. An empty or short result does not prove free time: only registered calendars are archived, and coverage may be stale or incomplete (see sources). Use live free/busy for scheduling."

func calendarCoverageContext(ctx context.Context, s *Store, unexpandable bool, now time.Time) (CalendarCoverage, error) {
	coverage := CalendarCoverage{Notice: calendarCoverageNotice, Sources: []CalendarSourceCoverage{}}
	scoped, args := sourceScopeClause(ctx, "s.id")
	// A completed full sync establishes the baseline. Later completed incremental
	// runs refresh it; bounded runs cannot establish or refresh coverage.
	query := `SELECT s.id,s.source_type,COALESCE(NULLIF(s.display_name,''),s.identifier),COALESCE(s.sync_cursor,'')<>'',
  baseline.completed_at,
  EXISTS(SELECT 1 FROM sync_runs failed WHERE failed.source_id=s.id
   AND failed.status IN ('failed','cancelled')
   AND (failed.started_at>baseline.started_at OR (failed.started_at=baseline.started_at AND failed.id>baseline.id)))
 FROM sources s
 LEFT JOIN sync_runs full_run ON full_run.id=(SELECT id FROM sync_runs WHERE source_id=s.id AND sync_type='full' AND status='completed' AND COALESCE(cursor_after,'')<>'' ORDER BY started_at DESC,id DESC LIMIT 1)
 LEFT JOIN sync_runs baseline ON baseline.id=(SELECT id FROM sync_runs WHERE source_id=s.id AND sync_type IN ('full','incremental') AND status='completed' AND COALESCE(cursor_after,'')<>''
  AND full_run.id IS NOT NULL ORDER BY started_at DESC,id DESC LIMIT 1)
 WHERE 1=1` + scoped + `
 AND NOT EXISTS (SELECT 1 FROM source_settings merged
  WHERE merged.source_id=s.id AND merged.merged_into_source_id IS NOT NULL)
 AND s.source_type = 'gcal' ORDER BY s.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return CalendarCoverage{}, fmt.Errorf("read calendar coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	coverage.Complete = !unexpandable
	for rows.Next() {
		var c CalendarSourceCoverage
		var hasCursor bool
		var asof sql.NullTime
		var failed bool
		if err := rows.Scan(&c.SourceID, &c.SourceType, &c.Name, &hasCursor, &asof, &failed); err != nil {
			return CalendarCoverage{}, fmt.Errorf("scan calendar coverage: %w", err)
		}
		if asof.Valid {
			at := asof.Time.UTC()
			c.AsOf = &at
		}
		switch {
		case !hasCursor || !asof.Valid:
			c.Status = CalendarSourceCoverageStatusNoBaseline
		case failed:
			c.Status = CalendarSourceCoverageStatusSyncFailed
		case now.Sub(asof.Time) > 24*time.Hour:
			c.Status = CalendarSourceCoverageStatusStale
		default:
			c.Status = CalendarSourceCoverageStatusComplete
		}
		if c.Status != CalendarSourceCoverageStatusComplete {
			coverage.Complete = false
		}
		coverage.Sources = append(coverage.Sources, c)
	}
	if err := rows.Err(); err != nil {
		return CalendarCoverage{}, fmt.Errorf("iterate calendar coverage: %w", err)
	}
	coverage.Complete = coverage.Complete && len(coverage.Sources) > 0
	return coverage, nil
}
