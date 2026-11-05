package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

func activityFactSubjectClause(subject ActivitySubject) (string, []any, error) {
	if subject.PersonID > 0 && len(subject.ParticipantIDs) == 0 {
		return "f.person_id=?", []any{subject.PersonID}, nil
	}
	if subject.PersonID != 0 || len(subject.ParticipantIDs) == 0 {
		return "", nil, fmt.Errorf("%w: a contact subject is required", ErrInvalidActivityRequest)
	}
	for _, id := range subject.ParticipantIDs {
		if id <= 0 {
			return "", nil, fmt.Errorf("%w: invalid contact participant", ErrInvalidActivityRequest)
		}
	}
	predicate, args := messageExportInClause("f.participant_id", subject.ParticipantIDs)
	return predicate, args, nil
}

func (s *Store) MessageContactAggregateContext(ctx context.Context, q ContactQuery) (ContactAggregate, error) {
	var result ContactAggregate
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		snapshotCtx := context.WithValue(ctx, readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx})
		var err error
		result, err = s.messageContactAggregateContext(snapshotCtx, q)
		return err
	})
	return result, err
}

func (s *Store) messageContactAggregateContext(ctx context.Context, q ContactQuery) (ContactAggregate, error) {
	result := ContactAggregate{Channels: []ActivityChannel{}}
	if q.Now.IsZero() {
		return result, fmt.Errorf("%w: contact clock is required", ErrInvalidActivityRequest)
	}
	if _, _, err := activityFactSubjectClause(q.Subject); err != nil {
		return result, err
	}
	filters, err := s.normalizedActivityFilters(ctx, q.Filters)
	if err != nil {
		return result, err
	}
	filters.IncludeNoise = false
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{Subject: q.Subject, Until: &q.Now}, filters)
	if err != nil {
		return result, err
	}
	loc := q.Location
	if loc == nil {
		loc = time.UTC
	}
	now := q.Now.In(loc)
	monthEnd := now.Format("2006-01")
	firstMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
	monthStart := firstMonth.Format("2006-01")
	gap := `julianday(occurred_at)-julianday(previous_at)>=7`
	if s.IsPostgreSQL() {
		gap = `EXTRACT(EPOCH FROM (occurred_at-previous_at))>=604800`
	}
	// Build civil month boundaries in the caller's zone, including its DST
	// transitions. Comparing instants works identically on both databases.
	months := make([]string, 12)
	boundaries := make([]time.Time, 13)
	for i := range boundaries {
		civil := firstMonth.AddDate(0, i, 0)
		start := time.Date(civil.Year(), civil.Month(), 1, 0, 0, 0, 0, loc)
		// A skipped midnight can normalize into the previous month. The
		// transition ends that zone interval and starts the requested month.
		if start.Year() != civil.Year() || start.Month() != civil.Month() {
			_, start = start.ZoneBounds()
		}
		boundaries[i] = start.UTC()
	}
	for i := range months {
		months[i] = "(?,?,?)"
		if s.IsPostgreSQL() {
			months[i] = "(?,CAST(? AS TIMESTAMPTZ),CAST(? AS TIMESTAMPTZ))"
		}
		args = append(args, firstMonth.AddDate(0, i, 0).Format("2006-01"), boundaries[i], boundaries[i+1])
	}
	initiation := `(previous_at IS NULL OR ` + gap + `)`
	query := cte + `, contact_messages AS (
 SELECT * FROM representatives WHERE ref_kind='message' AND counted=1
 ), contact_months(month,start_at,end_at) AS (VALUES ` + strings.Join(months, ",") + `), exchanges AS (
 SELECT *,LAG(occurred_at) OVER(ORDER BY occurred_at,message_id) AS previous_at FROM contact_messages
 ) SELECT channel,COALESCE(bucket.month,''),MIN(occurred_at),MAX(occurred_at),
 MAX(CASE WHEN direction='inbound' THEN occurred_at END),MAX(CASE WHEN direction='outbound' THEN occurred_at END),COUNT(*),
 SUM(CASE WHEN direction='inbound' AND ` + initiation + ` THEN 1 ELSE 0 END),
 SUM(CASE WHEN direction='outbound' AND ` + initiation + ` THEN 1 ELSE 0 END)
 FROM exchanges LEFT JOIN contact_months bucket ON occurred_at>=bucket.start_at AND occurred_at<bucket.end_at
 GROUP BY channel,bucket.month ORDER BY channel,bucket.month`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return result, fmt.Errorf("aggregate message contact: %w", err)
	}
	channels := map[ActivityChannel]bool{}
	for rows.Next() {
		var channel ActivityChannel
		var month string
		var first, last, in, out nullableTimestamp
		var count, contact, owner int
		if err = rows.Scan(&channel, &month, &first, &last, &in, &out, &count, &contact, &owner); err != nil {
			_ = rows.Close()
			return result, fmt.Errorf("scan message contact: %w", err)
		}
		MergeActivityTime(&result.FirstAt, nullableTimePointer(first), true)
		MergeActivityTime(&result.LastAt, nullableTimePointer(last), false)
		MergeActivityTime(&result.LastInboundAt, nullableTimePointer(in), false)
		MergeActivityTime(&result.LastOutboundAt, nullableTimePointer(out), false)
		result.Count += count
		result.InitiatedByContact += contact
		result.InitiatedByOwner += owner
		channels[channel] = true
		if month >= monthStart && month <= monthEnd {
			yearMonth, parseErr := time.Parse("2006-01", month)
			if parseErr != nil {
				_ = rows.Close()
				return result, fmt.Errorf("read activity month: %w", parseErr)
			}
			index := ActivityMonthIndex(yearMonth, now)
			if index >= 0 && index < 12 {
				result.Monthly[index] += count
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return result, fmt.Errorf("iterate message contact: %w", err)
	}
	for channel := range channels {
		result.Channels = append(result.Channels, channel)
	}
	slices.Sort(result.Channels)
	return result, nil
}

// MergeActivityTime keeps the earliest or latest contact timestamp.
func MergeActivityTime(target **time.Time, value *time.Time, earliest bool) {
	if value == nil {
		return
	}
	if *target == nil || (earliest && value.Before(**target)) || (!earliest && value.After(**target)) {
		instant := *value
		*target = &instant
	}
}

func (s *Store) AttendedTranscriptIDsContext(ctx context.Context, q ContactQuery, limit int) ([]int64, bool, error) {
	if q.Now.IsZero() || limit < 1 || limit > 2000 {
		return nil, false, fmt.Errorf("%w: transcript clock and limit 1..2000 required", ErrInvalidActivityRequest)
	}
	subject, subjectArgs, err := activityFactSubjectClause(q.Subject)
	if err != nil {
		return nil, false, err
	}
	filters, err := s.normalizedActivityFilters(ctx, q.Filters)
	if err != nil {
		return nil, false, err
	}
	filters.IncludeNoise = false
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{Subject: q.Subject, Until: &q.Now}, filters)
	if err != nil {
		return nil, false, err
	}
	args = append(args, subjectArgs...)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(cte+` SELECT r.message_id FROM representatives r WHERE r.message_type='meeting_transcript' AND r.list_id IS NULL
 AND EXISTS(SELECT 1 FROM facts f WHERE f.message_id=r.message_id AND f.excluded='' AND f.role IN ('attendee','organizer') AND `+subject+`)
 ORDER BY r.occurred_at DESC,r.message_id DESC LIMIT ?`), args...)
	if err != nil {
		return nil, false, fmt.Errorf("read attended transcripts: %w", err)
	}
	result := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan attended transcript: %w", err)
		}
		result = append(result, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, false, fmt.Errorf("iterate attended transcripts: %w", err)
	}
	truncated := len(result) > limit
	if truncated {
		result = result[:limit]
	}
	calendarIDs, calendarMore, err := s.calendarAttendedTranscriptIDs(ctx, q, filters, limit)
	if err != nil {
		return nil, false, err
	}
	result = append(result, calendarIDs...)
	slices.Sort(result)
	result = slices.Compact(result)
	truncated = truncated || calendarMore || len(result) > limit
	if len(result) > limit {
		result = result[:limit]
	}
	return result, truncated, nil
}

// CadenceForPersonContext reuses the contact-state cadence calculation while
// taking the caller's scoped last-contact instant instead of stored state.
func (s *Store) CadenceForPersonContext(ctx context.Context, personID int64, lastContactAt *time.Time, now time.Time) (string, *time.Time, error) {
	if personID <= 0 || now.IsZero() {
		return "", nil, fmt.Errorf("%w: cadence requires person and clock", ErrInvalidActivityRequest)
	}
	state := ContactState{LastContactAt: lastContactAt}
	err := s.withTxOptionsContext(ctx, &sql.TxOptions{ReadOnly: true}, func(tx *loggedTx) error {
		values, err := listCurrentContactFrequencyTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		deriveContactCadence(&state, values, now)
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("read contact cadence: %w", err)
	}
	return state.CadenceStatus, state.CadenceDueAt, nil
}

// ActivityMonthIndex returns the oldest-to-newest index in a twelve-month window.
func ActivityMonthIndex(at, now time.Time) int {
	return 11 - ((now.Year()-at.Year())*12 + int(now.Month()-at.Month()))
}
