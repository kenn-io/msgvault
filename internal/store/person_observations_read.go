package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

const observationColumns = `id, person_id, sequence, generation_id, level, origin, kind,
 content, content_key, subject_ref, subject_person_id, directness, sensitive, confidence_bp,
 valid_from, valid_until, occurs_on, status, status_reason, superseded_by_id, owner_state,
 pinned, support_flag, stability, stability_source, target_kind, target_key, mention_count,
 first_seen_at, last_seen_at, premise_changed_at, program_fingerprint, created_at, updated_at`

func scanObservation(row scanner) (personobs.Observation, error) {
	var o personobs.Observation
	var generation, subject, successor sql.NullInt64
	var from, until, first, last, changed nullableTimestamp
	var created, updated requiredTimestamp
	var occurs, targetKind, targetKey sql.NullString
	err := row.Scan(&o.ID, &o.PersonID, &o.Sequence, &generation, &o.Level, &o.Origin, &o.Kind,
		&o.Content, &o.ContentKey, &o.SubjectRef, &subject, &o.Directness, &o.Sensitive, &o.ConfidenceBP,
		&from, &until, &occurs, &o.Status, &o.StatusReason, &successor, &o.OwnerState, &o.Pinned,
		&o.SupportFlag, &o.Stability, &o.StabilitySource, &targetKind, &targetKey, &o.MentionCount,
		&first, &last, &changed, &o.ProgramFingerprint, &created, &updated)
	if err != nil {
		return o, err
	}
	o.GenerationID = personFactNullInt64(generation)
	o.SubjectPersonID = personFactNullInt64(subject)
	o.SupersededByID = personFactNullInt64(successor)
	o.ValidFrom = optionalTimestamp(from)
	o.ValidUntil = optionalTimestamp(until)
	o.FirstSeenAt = optionalTimestamp(first)
	o.LastSeenAt = optionalTimestamp(last)
	o.PremiseChangedAt = optionalTimestamp(changed)
	o.CreatedAt = created.Time
	o.UpdatedAt = updated.Time
	o.TargetKind = targetKind.String
	o.TargetKey = targetKey.String
	if occurs.Valid {
		parts := strings.Split(occurs.String, "-")
		values := make([]int, 3)
		if len(parts) < 1 || len(parts) > 3 {
			return o, fmt.Errorf("invalid observation date %q", occurs.String)
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil {
				return o, fmt.Errorf("parse observation date: %w", err)
			}
			values[i] = n
		}
		o.OccursOn = &personfacts.PartialDateValue{Year: values[0], Month: values[1], Day: values[2]}
	}
	return o, nil
}

func (s *Store) queryObservationRows(ctx context.Context, q contextRowsQuerier, predicate, order string, args ...any) ([]personobs.Observation, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+observationColumns+` FROM person_observations WHERE `+predicate+` ORDER BY `+order, args...)
	if err != nil {
		return nil, fmt.Errorf("query person observations: %w", err)
	}
	items := make([]personobs.Observation, 0)
	for rows.Next() {
		item, err := scanObservation(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan person observation: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate person observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.hydrateObservationRows(ctx, q, items); err != nil {
		return nil, err
	}
	return items, nil
}

// hydrateObservationRows computes support from the shared generation chronology.
// All row sets close before the next query, including on single-connection SQLite.
func (s *Store) hydrateObservationRows(ctx context.Context, q contextRowsQuerier, items []personobs.Observation) error {
	for i := range items {
		citations, err := s.observationCitations(ctx, q, items[i].ID)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(citations))
		for _, c := range citations {
			if c.Exclusion == personobs.ExclusionReasonNone {
				ids = append(ids, c.EvidenceID)
			}
		}
		supported, err := s.latestEvidenceSupportedTx(ctx, q, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if supported[id] {
				items[i].SupportedCount++
			}
		}
		rows, err := q.QueryContext(ctx, `SELECT category FROM person_observation_categories WHERE observation_id=? ORDER BY category`, items[i].ID)
		if err != nil {
			return fmt.Errorf("read observation categories: %w", err)
		}
		for rows.Next() {
			var category personobs.Category
			if err := rows.Scan(&category); err != nil {
				_ = rows.Close()
				return err
			}
			items[i].Categories = append(items[i].Categories, category)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) observationCitations(ctx context.Context, q contextRowsQuerier, id int64) ([]personobs.Citation, error) {
	rows, err := q.QueryContext(ctx, `SELECT evidence_id,quote_start,quote_end,quote_sha256,exclusion_reason,message_id,conversation_id,conversation_kind,authored_by_person,author_is_owner,event_time
  FROM person_observation_evidence WHERE observation_id=? ORDER BY evidence_id`, id)
	if err != nil {
		return nil, fmt.Errorf("read observation citations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]personobs.Citation, 0)
	for rows.Next() {
		var c personobs.Citation
		var event requiredTimestamp
		if err := rows.Scan(&c.EvidenceID, &c.QuoteStart, &c.QuoteEnd, &c.QuoteSHA256, &c.Exclusion, &c.Leaf.MessageID, &c.Leaf.ConversationID, &c.Leaf.ConversationKind, &c.Leaf.AuthoredByPerson, &c.Leaf.AuthorIsOwner, &event); err != nil {
			return nil, fmt.Errorf("scan observation citation: %w", err)
		}
		c.Leaf.EventTime = event.Time
		out = append(out, c)
	}
	return out, rows.Err()
}

func appendObservationFilter[T ~string](predicate *string, args *[]any, column string, values []T) {
	if len(values) == 0 {
		return
	}
	*predicate += ` AND ` + column + ` IN (` + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + `)`
	for _, value := range values {
		*args = append(*args, string(value))
	}
}
func observationLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	return min(limit, 500)
}

// ListPersonObservations lists owner-visible rows in display order. Sensitivity
// is opt-in even when the caller supplies an explicit lifecycle filter.
func (s *Store) ListPersonObservations(ctx context.Context, personID int64, f personobs.Filter) (personobs.Page, error) {
	page := personobs.Page{Items: []personobs.Observation{}}
	if f.Offset < 0 {
		return page, errors.New("observation offset must be non-negative")
	}
	predicate := `person_id=?`
	args := []any{personID}
	appendObservationFilter(&predicate, &args, "status", f.Statuses)
	appendObservationFilter(&predicate, &args, "level", f.Levels)
	appendObservationFilter(&predicate, &args, "kind", f.Kinds)
	if !f.IncludeSensitive {
		predicate += ` AND sensitive=FALSE`
	}
	if f.AsOf != nil {
		predicate += ` AND (valid_from IS NULL OR valid_from<=?) AND (valid_until IS NULL OR valid_until>=?)`
		args = append(args, personFactPortableTime(*f.AsOf), personFactPortableTime(*f.AsOf))
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_observations WHERE `+predicate, args...).Scan(&page.Total); err != nil {
			return fmt.Errorf("count person observations: %w", err)
		}
		listArgs := append(append([]any(nil), args...), observationLimit(f.Limit), f.Offset)
		var err error
		page.Items, err = s.queryObservationRows(ctx, tx, predicate, `pinned DESC,last_seen_at DESC NULLS LAST,id DESC LIMIT ? OFFSET ?`, listArgs...)
		return err
	})
	return page, err
}
func (s *Store) ListPersonObservationsAfterSequence(ctx context.Context, personID int64, level personobs.Level, after int64, limit int) ([]personobs.Observation, error) {
	var items []personobs.Observation
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		items, err = s.queryObservationRows(ctx, tx, `person_id=? AND level=? AND sequence>?`, `sequence ASC LIMIT ?`, personID, level, after, observationLimit(limit))
		return err
	})
	return items, err
}
func (s *Store) ListObservationsNeedingRecheck(ctx context.Context, personID int64, level personobs.Level, limit int) ([]personobs.Observation, error) {
	var items []personobs.Observation
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		items, err = s.queryObservationRows(ctx, tx, `person_id=? AND level=? AND premise_changed_at IS NOT NULL`, `sequence ASC LIMIT ?`, personID, level, observationLimit(limit))
		return err
	})
	return items, err
}
func (s *Store) GetPersonObservation(ctx context.Context, id int64) (personobs.Detail, error) {
	var detail personobs.Detail
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		items, err := s.queryObservationRows(ctx, tx, `id=?`, `id`, id)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return sql.ErrNoRows
		}
		detail.Observation = items[0]
		detail.Evidence, err = s.observationCitations(ctx, tx, id)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT premise_id,role FROM person_observation_premises WHERE derived_id=? ORDER BY ordinal`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p personobs.PremiseInput
			if err := rows.Scan(&p.ID, &p.Role); err != nil {
				_ = rows.Close()
				return err
			}
			detail.Premises = append(detail.Premises, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT derived_id FROM person_observation_premises WHERE premise_id=? ORDER BY derived_id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var dependent int64
			if err := rows.Scan(&dependent); err != nil {
				_ = rows.Close()
				return err
			}
			detail.Dependents = append(detail.Dependents, dependent)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT id,observation_id,event,actor,detail,created_at FROM person_observation_events WHERE observation_id=? ORDER BY id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var event personobs.Event
			var created requiredTimestamp
			if err := rows.Scan(&event.ID, &event.ObservationID, &event.Event, &event.Actor, &event.Detail, &created); err != nil {
				_ = rows.Close()
				return err
			}
			event.CreatedAt = created.Time
			detail.Events = append(detail.Events, event)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		return rows.Close()
	})
	if err != nil {
		return personobs.Detail{}, fmt.Errorf("get person observation %d: %w", id, err)
	}
	return detail, nil
}

// PersonObservationLayout reads only the covering layout index; no statement
// content, evidence, or message body is needed for these counts.
func (s *Store) PersonObservationLayout(ctx context.Context, personID int64) (personobs.Layout, error) {
	layout := personobs.Layout{ByLevel: map[personobs.Level]int{}, ByStatus: map[personobs.Status]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT level,status,pinned,sensitive,premise_changed_at,COUNT(*) FROM person_observations WHERE person_id=? GROUP BY level,status,pinned,sensitive,premise_changed_at`, personID)
	if err != nil {
		return layout, fmt.Errorf("read person observation layout: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var level personobs.Level
		var status personobs.Status
		var pinned, sensitive bool
		var recheck nullableTimestamp
		var count int
		if err := rows.Scan(&level, &status, &pinned, &sensitive, &recheck, &count); err != nil {
			return layout, err
		}
		layout.Total += count
		layout.ByLevel[level] += count
		layout.ByStatus[status] += count
		if pinned {
			layout.Pinned += count
		}
		if sensitive {
			layout.Sensitive += count
		}
		if recheck.Valid {
			layout.NeedsRecheck += count
		}
	}
	return layout, rows.Err()
}
