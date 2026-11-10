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
	SourceID int64      `json:"source_id"`
	Name     string     `json:"name"`
	Status   string     `json:"status" enum:"complete,stale,sync_failed,no_baseline"`
	AsOf     *time.Time `json:"as_of"`
}

const (
	CalendarSourceCoverageStatusComplete   = "complete"
	CalendarSourceCoverageStatusStale      = "stale"
	CalendarSourceCoverageStatusSyncFailed = "sync_failed"
	CalendarSourceCoverageStatusNoBaseline = "no_baseline"
)

const calendarCoverageNotice = "Archived calendar data, not live availability. An empty or short result does not prove free time: only registered calendars are archived, and coverage may be stale or incomplete (see sources). Use live free/busy for scheduling."

func calendarCoverageContext(ctx context.Context, s *Store, now time.Time) (CalendarCoverage, error) {
	coverage := CalendarCoverage{Notice: calendarCoverageNotice, Sources: []CalendarSourceCoverage{}}
	// A completed full sync establishes the baseline. Later completed incremental
	// runs refresh it; bounded runs cannot establish or refresh coverage.
	query, args := calendarCoverageQuery(ctx, s.IsPostgreSQL())
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return CalendarCoverage{}, fmt.Errorf("read calendar coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	coverage.Complete = true
	for rows.Next() {
		var c CalendarSourceCoverage
		var hasCursor bool
		var hasFullBaseline bool
		var asof sql.NullTime
		var failed bool
		if err := rows.Scan(&c.SourceID, &c.Name, &hasCursor, &asof, &hasFullBaseline, &failed); err != nil {
			return CalendarCoverage{}, fmt.Errorf("scan calendar coverage: %w", err)
		}
		if hasFullBaseline && asof.Valid {
			at := asof.Time.UTC()
			c.AsOf = &at
		}
		switch {
		case !hasCursor || !hasFullBaseline || !asof.Valid:
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

func calendarCoverageQuery(ctx context.Context, postgres bool) (string, []any) {
	// SQLite can choose a history scan when planner statistics are stale.
	index := ""
	if !postgres {
		index = " INDEXED BY idx_sync_runs_source"
	}
	scoped, args := sourceScopeClause(ctx, "s.id")
	query := `WITH calendar_sources AS MATERIALIZED (
  SELECT s.id,COALESCE(NULLIF(s.display_name,''),s.identifier) AS name,
   COALESCE(s.sync_cursor,'')<>'' AS has_cursor
  FROM sources s
  WHERE 1=1` + scoped + `
   AND NOT EXISTS (SELECT 1 FROM source_settings merged
    WHERE merged.source_id=s.id AND merged.merged_into_source_id IS NOT NULL)
   AND s.source_type='gcal'
 ), calendar_baselines AS MATERIALIZED (
  SELECT s.id,
   (SELECT r.id FROM sync_runs r
    WHERE r.source_id=s.id AND r.sync_type IN ('full','incremental')
     AND r.status='completed' AND COALESCE(r.cursor_after,'')<>''
    ORDER BY r.started_at DESC,r.id DESC LIMIT 1) AS latest_baseline_id,
   (SELECT r.id FROM sync_runs r
    WHERE r.source_id=s.id AND r.sync_type='full'
     AND r.status='completed' AND COALESCE(r.cursor_after,'')<>''
    ORDER BY r.started_at,r.id LIMIT 1) AS full_baseline_id
  FROM calendar_sources s
 )
 SELECT s.id,s.name,s.has_cursor,
  latest.completed_at,
  coverage.full_baseline_id IS NOT NULL,
  CASE WHEN coverage.full_baseline_id IS NOT NULL
    AND coverage.latest_baseline_id IS NOT NULL
    AND (SELECT 1 FROM sync_runs failure
     WHERE failure.source_id=s.id AND failure.status IN ('failed','cancelled')
      AND (failure.started_at>latest.started_at
       OR (failure.started_at=latest.started_at AND failure.id>coverage.latest_baseline_id))
     ORDER BY failure.started_at DESC,failure.id DESC LIMIT 1) IS NOT NULL
   THEN TRUE ELSE FALSE END
 FROM calendar_sources s
 JOIN calendar_baselines coverage ON coverage.id=s.id
 LEFT JOIN sync_runs latest` + index + ` ON latest.source_id=s.id AND latest.id=coverage.latest_baseline_id
 ORDER BY s.id`
	return query, args
}
