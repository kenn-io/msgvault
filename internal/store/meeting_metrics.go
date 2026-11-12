package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

type meetingMetricMember struct {
	id       int64
	at       time.Time
	duration sql.NullFloat64
	basis    string
}

type meetingMetricUnit struct {
	at       time.Time
	month    string
	counted  bool
	duration *float64
	basis    meetingcontent.DurationBasis
	memberID int64
}

func (s *Store) GetMeetingMetricsContext(ctx context.Context, scope MeetingQueryScope) (*meetingcontent.Metrics, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	snapshot, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	if !ok {
		return nil, errors.New("meeting metrics read snapshot unavailable")
	}
	uid, err := archiveUIDFromMeetingSnapshot(ctx, snapshot.tx)
	if err != nil {
		return nil, err
	}
	if err = s.validateMeetingScopePopulation(ctx, snapshot.tx, scope); err != nil {
		return nil, err
	}
	unbounded := scope
	unbounded.After, unbounded.Before = nil, nil
	statement, err := s.buildMeetingScopeStatementFor(unbounded, scope.MessageIDs == nil)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, statement.cte+` SELECT sm.message_id, COALESCE(e.start_at, sm.occurred_at), sm.duration_seconds, sm.duration_basis, e.end_at FROM scoped_meetings sm LEFT JOIN calendar_events e ON e.message_id=sm.message_id`, statement.args...)
	if err != nil {
		return nil, fmt.Errorf("select meeting metrics members: %w", err)
	}
	members := map[int64]meetingMetricMember{}
	for rows.Next() {
		var member meetingMetricMember
		var at, end nullableTimestamp
		var basis sql.NullString
		if err := rows.Scan(&member.id, &at, &member.duration, &basis, &end); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if at.Valid {
			member.at = at.Time
		}
		member.basis = basis.String
		if !member.duration.Valid && at.Valid && end.Valid && !end.Time.Before(at.Time) {
			member.duration = sql.NullFloat64{Float64: end.Time.Sub(at.Time).Seconds(), Valid: true}
			member.basis = string(meetingcontent.DurationScheduled)
		}
		members[member.id] = member
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, err
	}
	owners, err := s.meetingMetricOwners(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	occurrences, _, err := s.MeetingOccurrencesContext(ctx, now, owners)
	if err != nil {
		return nil, err
	}
	units := map[string]*meetingMetricUnit{}
	seen := map[int64]bool{}
	for _, row := range occurrences {
		member, selected := members[row.MessageID]
		if !selected {
			continue
		}
		seen[row.MessageID] = true
		if row.OccurredAt.After(now) {
			continue
		}
		unit := units[row.OccurrenceKey]
		if unit == nil {
			unit = &meetingMetricUnit{at: row.OccurredAt}
			if len(row.StartDate) >= 7 {
				unit.month = row.StartDate[:7]
			}
			units[row.OccurrenceKey] = unit
		}
		unit.counted = unit.counted || row.Counted
		if member.duration.Valid {
			unit.choose(member.duration.Float64, meetingcontent.DurationBasis(member.basis), member.id)
		}
		if row.ScheduledSeconds != nil {
			unit.choose(*row.ScheduledSeconds, meetingcontent.DurationScheduled, member.id)
		}
	}
	for id, member := range members {
		if seen[id] {
			continue
		}
		if member.at.After(now) {
			continue
		}
		unit := &meetingMetricUnit{at: member.at, counted: true}
		if member.duration.Valid {
			unit.choose(member.duration.Float64, meetingcontent.DurationBasis(member.basis), id)
		}
		units[fmt.Sprintf("message:%d", id)] = unit
	}
	result := &meetingcontent.Metrics{SchemaVersion: meetingResultSchemaVersion, ArchiveUID: uid, Scope: meetingScopeProvenance(scope), DurationByBasis: []meetingcontent.BasisTotals{}, Months: []meetingcontent.MonthTotals{}}
	bases := map[meetingcontent.DurationBasis]meetingcontent.BasisTotals{}
	months := map[string]meetingcontent.DurationTotals{}
	for _, unit := range units {
		if !unit.counted {
			continue
		}
		if scope.After != nil && (unit.at.IsZero() || unit.at.Before(*scope.After)) {
			continue
		}
		if scope.Before != nil && (unit.at.IsZero() || !unit.at.Before(*scope.Before)) {
			continue
		}
		addMeetingMetric(&result.Totals, unit.duration)
		if unit.at.IsZero() {
			result.UndatedCount++
		} else {
			at := unit.at.UTC()
			if result.FirstMeetingAt == nil || at.Before(*result.FirstMeetingAt) {
				result.FirstMeetingAt = &at
			}
			if result.LastMeetingAt == nil || at.After(*result.LastMeetingAt) {
				result.LastMeetingAt = &at
			}
			month := unit.month
			if month == "" {
				month = at.Format("2006-01")
			}
			totals := months[month]
			addMeetingMetric(&totals, unit.duration)
			months[month] = totals
		}
		if unit.duration != nil {
			b := bases[unit.basis]
			b.Basis = unit.basis
			b.Count++
			b.TotalSeconds += *unit.duration
			bases[unit.basis] = b
		}
	}
	finishMeetingMetric(&result.Totals)
	for month, totals := range months {
		finishMeetingMetric(&totals)
		result.Months = append(result.Months, meetingcontent.MonthTotals{Month: month, Totals: totals})
	}
	slices.SortFunc(result.Months, func(a, b meetingcontent.MonthTotals) int { return strings.Compare(a.Month, b.Month) })
	for _, b := range bases {
		result.DurationByBasis = append(result.DurationByBasis, b)
	}
	slices.SortFunc(result.DurationByBasis, func(a, b meetingcontent.BasisTotals) int { return metricBasisRank(a.Basis) - metricBasisRank(b.Basis) })
	return result, nil
}

func (u *meetingMetricUnit) choose(seconds float64, basis meetingcontent.DurationBasis, id int64) {
	if u.duration == nil || metricBasisRank(basis) < metricBasisRank(u.basis) || metricBasisRank(basis) == metricBasisRank(u.basis) && id < u.memberID {
		u.duration = &seconds
		u.basis = basis
		u.memberID = id
	}
}
func metricBasisRank(b meetingcontent.DurationBasis) int {
	switch b {
	case meetingcontent.DurationProvider:
		return 0
	case meetingcontent.DurationScheduled:
		return 1
	case meetingcontent.DurationTranscriptSpan:
		return 2
	default:
		return 3
	}
}
func addMeetingMetric(t *meetingcontent.DurationTotals, duration *float64) {
	t.MeetingCount++
	if duration == nil {
		t.UnknownDurationCount++
	} else {
		t.KnownDurationCount++
		t.TotalKnownSeconds += *duration
	}
}
func finishMeetingMetric(t *meetingcontent.DurationTotals) {
	if t.KnownDurationCount > 0 {
		average := t.TotalKnownSeconds / float64(t.KnownDurationCount)
		t.AverageKnownSeconds = &average
	}
}
func errorsJoinRows(rows *loggedRows) error {
	err := rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *Store) meetingMetricOwners(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT LOWER(TRIM(address)) FROM account_identities`)
	if err != nil {
		return nil, err
	}
	owners := map[string]bool{}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			_ = rows.Close()
			return nil, err
		}
		owners[email] = true
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, err
	}
	clusters, err := s.ParticipantClusters(ctx)
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id, email_address FROM participants WHERE email_address IS NOT NULL UNION ALL SELECT participant_id, identifier_value FROM participant_identifiers WHERE identifier_type='email'`)
	if err != nil {
		return nil, err
	}
	type address struct {
		id    int64
		email string
	}
	var addresses []address
	for rows.Next() {
		var a address
		if err := rows.Scan(&a.id, &a.email); err != nil {
			_ = rows.Close()
			return nil, err
		}
		a.email = strings.ToLower(strings.TrimSpace(a.email))
		addresses = append(addresses, a)
	}
	if err := errorsJoinRows(rows); err != nil {
		return nil, err
	}
	canonical := func(id int64) int64 {
		if c, ok := clusters[id]; ok {
			return c
		}
		return id
	}
	ownerClusters := map[int64]bool{}
	for _, a := range addresses {
		if owners[a.email] {
			ownerClusters[canonical(a.id)] = true
		}
	}
	for _, a := range addresses {
		if ownerClusters[canonical(a.id)] {
			owners[a.email] = true
		}
	}
	return owners, nil
}
