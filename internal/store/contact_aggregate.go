package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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
	result := ContactAggregate{Channels: []ActivityChannel{}}
	if q.Now.IsZero() {
		return result, fmt.Errorf("%w: contact clock is required", ErrInvalidActivityRequest)
	}
	subject, subjectArgs, err := activityFactSubjectClause(q.Subject)
	if err != nil {
		return result, err
	}
	filters, err := normalizedActivityFilters(q.Filters)
	if err != nil {
		return result, err
	}
	filters.IncludeNoise = false
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{Subject: q.Subject}, filters)
	if err != nil {
		return result, err
	}
	listExcluded := "0"
	if filters.Exclusions.ExcludeListMessages {
		listExcluded = `CASE WHEN r.list_id IS NOT NULL THEN 1 ELSE 0 END`
	}
	counted := CountedEventWhere(CountedEventInputs{Evidence: "f.evidence", Role: "f.role", Direction: "r.direction", RefKind: "r.ref_kind", ListExcluded: listExcluded, LargeInbound: "r.large_inbound"})
	args = append(args, q.Now)
	args = append(args, subjectArgs...)
	loc := q.Location
	if loc == nil {
		loc = time.UTC
	}
	now := q.Now.In(loc)
	monthEnd := now.Format("2006-01")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -11, 0).Format("2006-01")
	gap := `julianday(occurred_at)-julianday(previous_at)>=7`
	if s.IsPostgreSQL() {
		gap = `EXTRACT(EPOCH FROM (occurred_at-previous_at))>=604800`
	}
	initiation := `(previous_at IS NULL OR ` + gap + `) AND SUBSTR(local_date,1,7) BETWEEN ? AND ?`
	args = append(args, monthStart, monthEnd, monthStart, monthEnd)
	query := cte + activityBurstGroupsCTE + `, contact_messages AS (
 SELECT r.* FROM representatives r JOIN burst_groups b ON b.source_id=r.source_id AND b.conversation_id=COALESCE(r.conversation_id,0) AND b.local_date=r.local_date
 WHERE b.noise=0 AND r.ref_kind='message' AND r.encrypted=0 AND r.occurred_at<=?
 AND EXISTS(SELECT 1 FROM facts f WHERE f.message_id=r.message_id AND f.excluded='' AND ` + subject + ` AND ` + counted + `)
 ), exchanges AS (
 SELECT *,LAG(occurred_at) OVER(PARTITION BY source_id,conversation_id ORDER BY occurred_at,message_id) AS previous_at FROM contact_messages
 ) SELECT channel,SUBSTR(local_date,1,7),MIN(occurred_at),MAX(occurred_at),
 MAX(CASE WHEN direction='inbound' THEN occurred_at END),MAX(CASE WHEN direction='outbound' THEN occurred_at END),COUNT(*),
 SUM(CASE WHEN direction='inbound' AND ` + initiation + ` THEN 1 ELSE 0 END),
 SUM(CASE WHEN direction='outbound' AND ` + initiation + ` THEN 1 ELSE 0 END)
 FROM exchanges GROUP BY channel,SUBSTR(local_date,1,7) ORDER BY channel,SUBSTR(local_date,1,7)`
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
		mergeActivityTime(&result.FirstAt, nullableTimePointer(first), true)
		mergeActivityTime(&result.LastAt, nullableTimePointer(last), false)
		mergeActivityTime(&result.LastInboundAt, nullableTimePointer(in), false)
		mergeActivityTime(&result.LastOutboundAt, nullableTimePointer(out), false)
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
			index := 11 - ((now.Year()-yearMonth.Year())*12 + int(now.Month()-yearMonth.Month()))
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

func mergeActivityTime(target **time.Time, value *time.Time, earliest bool) {
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
	filters, err := normalizedActivityFilters(q.Filters)
	if err != nil {
		return nil, false, err
	}
	filters.IncludeNoise = false
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{Subject: q.Subject}, filters)
	if err != nil {
		return nil, false, err
	}
	args = append(args, q.Now)
	args = append(args, subjectArgs...)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(cte+` SELECT r.message_id FROM representatives r WHERE r.message_type='meeting_transcript' AND r.occurred_at<=? AND r.list_id IS NULL
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
