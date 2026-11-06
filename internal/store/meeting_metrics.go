package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

func (s *Store) GetMeetingMetricsContext(
	ctx context.Context, scope MeetingQueryScope,
) (*meetingcontent.Metrics, error) {
	candidateScope := scope
	candidateScope.After, candidateScope.Before = nil, nil
	statement, err := s.buildMeetingScopeStatement(candidateScope)
	if err != nil {
		return nil, err
	}
	result := &meetingcontent.Metrics{
		SchemaVersion:   meetingResultSchemaVersion,
		DurationByBasis: []meetingcontent.BasisTotals{},
		Months:          []meetingcontent.MonthTotals{},
		Scope:           meetingScopeProvenance(scope),
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var readErr error
		result.ArchiveUID, readErr = archiveUIDFromMeetingSnapshot(ctx, tx)
		if readErr != nil {
			return readErr
		}
		if readErr = s.validateMeetingScopePopulation(ctx, tx, scope); readErr != nil {
			return readErr
		}
		rows, queryErr := tx.QueryContext(ctx, statement.cte+`
			SELECT sm.message_id, sm.duration_seconds, `+meetingCountLinkedSQL+`
			FROM scoped_meetings sm JOIN messages m ON m.id = sm.message_id`, statement.args...)
		if queryErr != nil {
			return fmt.Errorf("read meeting count candidates: %w", queryErr)
		}
		type candidate struct {
			id       int64
			duration sql.NullFloat64
			linked   bool
		}
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.id, &item.duration, &item.linked); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan meeting count candidate: %w", err)
			}
			candidates = append(candidates, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate meeting count candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close meeting count candidates: %w", err)
		}
		slices.SortFunc(candidates, func(a, b candidate) int {
			if a.duration.Valid != b.duration.Valid {
				if a.duration.Valid {
					return -1
				}
				return 1
			}
			if order := cmp.Compare(b.duration.Float64, a.duration.Float64); order != 0 {
				return order
			}
			return cmp.Compare(a.id, b.id)
		})
		taken := make(map[string]bool)
		kept := make([]int64, 0, len(candidates))
		reader := reboundReader{loggedReader: tx, dialect: s.dialect}
		for _, item := range candidates {
			var occurrences []linkOccurrence
			if item.linked {
				budget := calrecur.RequestReadBudget
				var err error
				occurrences, err = s.meetingMemberOccurrences(ctx, reader, item.id, &budget)
				if unresolvedMeetingCount(err) {
					occurrences = nil
				} else if err != nil {
					return fmt.Errorf("resolve meeting count member %d: %w", item.id, err)
				}
			}
			if len(occurrences) > 0 && !slices.ContainsFunc(occurrences, func(o linkOccurrence) bool { return !taken[o.Identity.countKey()] }) {
				continue
			}
			kept = append(kept, item.id)
			for _, occurrence := range occurrences {
				taken[occurrence.Identity.countKey()] = true
			}
		}
		if len(kept) != len(candidates) || scope.After != nil || scope.Before != nil {
			countScope := scope
			countScope.MessageIDs = &kept
			statement, readErr = s.buildMeetingScopeStatement(countScope)
			if readErr != nil {
				return readErr
			}
		}
		var average sql.NullFloat64
		var first, last nullableTimestamp
		firstExpression, lastExpression := `NULL`, `NULL`
		if s.IsPostgreSQL() {
			firstExpression, lastExpression = `MIN(occurred_at)`, `MAX(occurred_at)`
		}
		readErr = tx.QueryRowContext(ctx, statement.cte+`
			SELECT COUNT(*), COUNT(duration_seconds), COUNT(*) - COUNT(duration_seconds),
				COALESCE(SUM(duration_seconds), 0), AVG(duration_seconds),
				`+firstExpression+`, `+lastExpression+`,
				COALESCE(SUM(CASE WHEN occurred_at IS NULL THEN 1 ELSE 0 END), 0)
			FROM scoped_meetings`, statement.args...).Scan(
			&result.Totals.MeetingCount,
			&result.Totals.KnownDurationCount,
			&result.Totals.UnknownDurationCount,
			&result.Totals.TotalKnownSeconds,
			&average,
			&first,
			&last,
			&result.UndatedCount,
		)
		if readErr != nil {
			return fmt.Errorf("read meeting duration totals: %w", readErr)
		}
		result.Totals.AverageKnownSeconds = nullableFloatPointer(average)
		result.FirstMeetingAt = nullableTimePointer(first)
		result.LastMeetingAt = nullableTimePointer(last)
		if !s.IsPostgreSQL() {
			first, last, readErr = readSQLiteMeetingExtrema(ctx, tx, statement)
			if readErr != nil {
				return readErr
			}
			result.FirstMeetingAt = nullableTimePointer(first)
			result.LastMeetingAt = nullableTimePointer(last)
		}

		rows, queryErr = tx.QueryContext(ctx, statement.cte+`
			SELECT duration_basis, COUNT(*), COALESCE(SUM(duration_seconds), 0)
			FROM scoped_meetings
			WHERE duration_seconds IS NOT NULL
			GROUP BY duration_basis
			ORDER BY CASE duration_basis
				WHEN 'provider' THEN 1
				WHEN 'scheduled' THEN 2
				WHEN 'transcript_span' THEN 3
				ELSE 4 END, duration_basis`, statement.args...)
		if queryErr != nil {
			return fmt.Errorf("read meeting duration bases: %w", queryErr)
		}
		for rows.Next() {
			var item meetingcontent.BasisTotals
			if scanErr := rows.Scan(&item.Basis, &item.Count, &item.TotalSeconds); scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("scan meeting duration basis: %w", scanErr)
			}
			result.DurationByBasis = append(result.DurationByBasis, item)
		}
		if iterationErr := rows.Err(); iterationErr != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate meeting duration bases: %w", iterationErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return fmt.Errorf("close meeting duration bases: %w", closeErr)
		}

		monthExpression := `strftime('%Y-%m', occurred_at)`
		if s.IsPostgreSQL() {
			monthExpression = `TO_CHAR(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM')`
		}
		rows, queryErr = tx.QueryContext(ctx, statement.cte+`
			SELECT `+monthExpression+`, COUNT(*), COUNT(duration_seconds),
				COUNT(*) - COUNT(duration_seconds), COALESCE(SUM(duration_seconds), 0),
				AVG(duration_seconds)
			FROM scoped_meetings
			WHERE occurred_at IS NOT NULL
			GROUP BY `+monthExpression+`
			ORDER BY `+monthExpression, statement.args...)
		if queryErr != nil {
			return fmt.Errorf("read meeting months: %w", queryErr)
		}
		for rows.Next() {
			var item meetingcontent.MonthTotals
			var monthAverage sql.NullFloat64
			if scanErr := rows.Scan(
				&item.Month,
				&item.Totals.MeetingCount,
				&item.Totals.KnownDurationCount,
				&item.Totals.UnknownDurationCount,
				&item.Totals.TotalKnownSeconds,
				&monthAverage,
			); scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("scan meeting month: %w", scanErr)
			}
			item.Totals.AverageKnownSeconds = nullableFloatPointer(monthAverage)
			result.Months = append(result.Months, item)
		}
		if iterationErr := rows.Err(); iterationErr != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate meeting months: %w", iterationErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return fmt.Errorf("close meeting months: %w", closeErr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get meeting metrics: %w", err)
	}
	return result, nil
}

func readSQLiteMeetingExtrema(
	ctx context.Context, tx *loggedTx, statement meetingScopeStatement,
) (nullableTimestamp, nullableTimestamp, error) {
	rows, err := tx.QueryContext(ctx, statement.cte+`, meeting_extrema AS (
		SELECT MIN(julianday(occurred_at)) AS first_bucket,
			MAX(julianday(occurred_at)) AS last_bucket
		FROM scoped_meetings
		WHERE occurred_at IS NOT NULL AND julianday(occurred_at) IS NOT NULL
	)
	SELECT scoped_meetings.occurred_at
	FROM scoped_meetings
	CROSS JOIN meeting_extrema
	WHERE julianday(scoped_meetings.occurred_at) = meeting_extrema.first_bucket
	   OR julianday(scoped_meetings.occurred_at) = meeting_extrema.last_bucket`,
		statement.args...)
	if err != nil {
		return nullableTimestamp{}, nullableTimestamp{}, fmt.Errorf("read SQLite meeting extrema: %w", err)
	}
	var first, last nullableTimestamp
	for rows.Next() {
		var candidate nullableTimestamp
		if scanErr := rows.Scan(&candidate); scanErr != nil {
			_ = rows.Close()
			return nullableTimestamp{}, nullableTimestamp{}, fmt.Errorf("scan SQLite meeting extrema: %w", scanErr)
		}
		if !candidate.Valid {
			continue
		}
		if !first.Valid || candidate.Time.Before(first.Time) {
			first = candidate
		}
		if !last.Valid || candidate.Time.After(last.Time) {
			last = candidate
		}
	}
	if iterationErr := rows.Err(); iterationErr != nil {
		_ = rows.Close()
		return nullableTimestamp{}, nullableTimestamp{}, fmt.Errorf("iterate SQLite meeting extrema: %w", iterationErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return nullableTimestamp{}, nullableTimestamp{}, fmt.Errorf("close SQLite meeting extrema: %w", closeErr)
	}
	return first, last, nil
}
