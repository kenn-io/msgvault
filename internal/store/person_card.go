package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/sqliteutil"
)

// PersonSource identifies a source observed on a person's currently bound
// participants. TotalCount includes sources omitted by the requested limit.
type PersonSource struct {
	SourceID       int64     `json:"source_id"`
	LastActivityAt time.Time `json:"last_activity_at"`
	TotalCount     int       `json:"total_count"`
}

// ResolvePersonByEmailContext returns durable profiles with a current email
// contact point. Six IDs suffice for five ambiguity candidates and a sentinel
// indicating that more candidates exist. Observations alone never resolve to a
// durable profile through this read.
func (s *Store) ResolvePersonByEmailContext(ctx context.Context, email string) ([]int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT person_id FROM person_contact_points
		WHERE address_kind = 'email' AND normalized_value = ?
		  AND active_until IS NULL AND superseded_at IS NULL
		GROUP BY person_id
		ORDER BY person_id
		LIMIT 6`), email)
	if err != nil {
		return nil, fmt.Errorf("resolve person by email: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, 6)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan person email match: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person email matches: %w", err)
	}
	return ids, nil
}

// ListPersonSourcesContext lists at most ten distinct sources, newest activity
// first and then by source ID. Nonpositive limits default to ten. Observation
// creation time supplies activity time when the importer did not record one.
func (s *Store) ListPersonSourcesContext(ctx context.Context, personID int64, limit int) ([]PersonSource, error) {
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	activity := `COALESCE(o.observed_at, o.created_at)`
	activityKey := activity
	if !s.IsPostgreSQL() {
		activityKey = sqliteutil.TimestampKeyFunction + `(` + activity + `)`
	}
	// Ranking keeps the original timestamp for scanning while using exact
	// chronological keys on SQLite, including offsets and subsecond precision.
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		WITH observations AS (
			SELECT o.source_id, `+activity+` AS last_activity_at,
				`+activityKey+` AS activity_key,
				ROW_NUMBER() OVER (
					PARTITION BY o.source_id ORDER BY `+activityKey+` DESC, o.id DESC
				) AS source_rank
			FROM person_participants pp
			JOIN participant_contact_observations o ON o.participant_id = pp.participant_id
			WHERE pp.person_id = ? AND o.source_id IS NOT NULL
			  AND o.active_until IS NULL AND o.superseded_at IS NULL
		)
		SELECT source_id, last_activity_at, COUNT(*) OVER () AS total_count
		FROM observations WHERE source_rank = 1
		ORDER BY activity_key DESC, source_id ASC
		LIMIT ?`), personID, limit)
	if err != nil {
		return nil, fmt.Errorf("list person sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sources := make([]PersonSource, 0, limit)
	for rows.Next() {
		var source PersonSource
		var lastActivity nullableTimestamp
		if err := rows.Scan(&source.SourceID, &lastActivity, &source.TotalCount); err != nil {
			return nil, fmt.Errorf("scan person source: %w", err)
		}
		source.LastActivityAt = lastActivity.Time.UTC()
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person sources: %w", err)
	}
	return sources, nil
}
