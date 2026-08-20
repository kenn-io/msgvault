package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	ActivityDefaultLimit = 100
	ActivityMaxLimit     = 500

	CadenceOK      = "ok"
	CadenceOverdue = "overdue"
)

var ErrInvalidActivityRequest = errors.New("invalid activity request")

type ActivityRef struct {
	Ref              string            `json:"ref"`
	MessageID        int64             `json:"message_id"`
	Kind             ActivityRefKind   `json:"kind"`
	Channel          ActivityChannel   `json:"channel"`
	OccurredAt       time.Time         `json:"occurred_at"`
	LocalDate        string            `json:"local_date"`
	Timezone         string            `json:"timezone"`
	UTCOffsetMinutes int               `json:"utc_offset_minutes"`
	DateOrigin       string            `json:"date_origin"`
	DatePrecision    string            `json:"date_precision"`
	Direction        ActivityDirection `json:"direction"`
	SourceID         int64             `json:"source_id"`
	ConversationID   *int64            `json:"conversation_id,omitempty"`
	Role             ActivityRole      `json:"role"`
	Evidence         ActivityEvidence  `json:"evidence"`
}

type PersonDaysRequest struct {
	PersonID int64
	From     string
	To       string
	Limit    int
	Offset   int
}

type PersonDay struct {
	LocalDate   string `json:"local_date"`
	EventCount  int64  `json:"event_count"`
	DirectCount int64  `json:"direct_count"`
	EntryCount  int64  `json:"entry_count"`
}

type PersonDaysPage struct {
	PersonID   int64       `json:"person_id"`
	Days       []PersonDay `json:"days"`
	TotalCount int64       `json:"total_count"`
}

type PersonDayRequest struct {
	PersonID    int64
	LocalDate   string
	Limit       int
	Offset      int
	EntryLimit  int
	EntryOffset int
}

type PersonDayPage struct {
	PersonID           int64            `json:"person_id"`
	LocalDate          string           `json:"local_date"`
	Activity           []ActivityRef    `json:"activity"`
	Entries            []DailyNoteEntry `json:"entries"`
	ActivityTotalCount int64            `json:"activity_total_count"`
	EntryTotalCount    int64            `json:"entry_total_count"`
}

type DayRequest struct {
	LocalDate              string
	Limit                  int
	Offset                 int
	EntryLimit             int
	EntryOffset            int
	ActivityLimitPerPerson int
}

type DayPerson struct {
	PersonID          int64         `json:"person_id"`
	VCardUID          string        `json:"vcard_uid"`
	DisplayName       *string       `json:"display_name,omitempty"`
	EventCount        int64         `json:"event_count"`
	DirectCount       int64         `json:"direct_count"`
	LastAt            time.Time     `json:"last_at"`
	Activity          []ActivityRef `json:"activity"`
	ActivityTruncated bool          `json:"activity_truncated"`
}

type DayPage struct {
	LocalDate        string           `json:"local_date"`
	Persons          []DayPerson      `json:"persons"`
	Entries          []DailyNoteEntry `json:"entries"`
	PersonTotalCount int64            `json:"person_total_count"`
	EntryTotalCount  int64            `json:"entry_total_count"`
}

const activityRefEventColumns = `
	ae.ref_kind, ae.message_id, ae.channel, ae.occurred_at, ae.local_date,
	ae.timezone, ae.utc_offset_minutes, ae.date_origin, ae.date_precision,
	ae.direction, ae.source_id, ae.conversation_id`

func normalizeActivityPage(limit, offset int) (int, error) {
	if offset < 0 {
		return 0, ErrInvalidActivityRequest
	}
	if limit <= 0 {
		limit = ActivityDefaultLimit
	}
	if limit > ActivityMaxLimit {
		limit = ActivityMaxLimit
	}
	return limit, nil
}

func requireActivityDate(value string) error {
	if !IsValidLocalDate(value) {
		return ErrInvalidActivityRequest
	}
	return nil
}

func requirePersonTxContext(ctx context.Context, tx *loggedTx, personID int64) error {
	if personID <= 0 {
		return ErrInvalidActivityRequest
	}
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM persons WHERE id = ?`, personID).
		Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("check person existence: %w", err)
	}
	return nil
}

func (s *Store) PersonDaysContext(
	ctx context.Context,
	request PersonDaysRequest,
) (*PersonDaysPage, error) {
	limit, err := normalizeActivityPage(request.Limit, request.Offset)
	if err != nil || request.PersonID <= 0 {
		return nil, ErrInvalidActivityRequest
	}
	if request.From != "" {
		if err := requireActivityDate(request.From); err != nil {
			return nil, err
		}
	}
	if request.To != "" {
		if err := requireActivityDate(request.To); err != nil {
			return nil, err
		}
	}
	if request.From != "" && request.To != "" && request.From > request.To {
		return nil, ErrInvalidActivityRequest
	}

	page := &PersonDaysPage{PersonID: request.PersonID, Days: []PersonDay{}}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := requirePersonTxContext(ctx, tx, request.PersonID); err != nil {
			return err
		}
		eventWhere := []string{"aep.person_id = ?"}
		eventArgs := []any{request.PersonID}
		entryWhere := []string{"target.person_id = ?"}
		entryArgs := []any{request.PersonID}
		if request.From != "" {
			eventWhere = append(eventWhere, "aep.local_date >= ?")
			eventArgs = append(eventArgs, request.From)
			entryWhere = append(entryWhere, "e.local_date >= ?")
			entryArgs = append(entryArgs, request.From)
		}
		if request.To != "" {
			eventWhere = append(eventWhere, "aep.local_date <= ?")
			eventArgs = append(eventArgs, request.To)
			entryWhere = append(entryWhere, "e.local_date <= ?")
			entryArgs = append(entryArgs, request.To)
		}
		query := `
			WITH event_links AS (
				SELECT aep.local_date, aep.message_id,
				       MAX(CASE WHEN aep.evidence = 'direct' THEN 1 ELSE 0 END) AS is_direct
				FROM activity_event_persons aep
				WHERE ` + strings.Join(eventWhere, " AND ") + `
				GROUP BY aep.local_date, aep.message_id
			),
			event_days AS (
				SELECT local_date,
				       COUNT(*) AS event_count,
				       SUM(is_direct) AS direct_count
				FROM event_links
				GROUP BY local_date
			),
			entry_days AS (
				SELECT e.local_date, COUNT(*) AS entry_count
				FROM daily_note_entries e
				JOIN daily_note_entry_persons target ON target.entry_id = e.id
				WHERE ` + strings.Join(entryWhere, " AND ") + `
				GROUP BY e.local_date
			),
			days AS (
				SELECT local_date FROM event_days
				UNION
				SELECT local_date FROM entry_days
			),
			page AS (
				SELECT d.local_date,
				       COALESCE(ed.event_count, 0) AS event_count,
				       COALESCE(ed.direct_count, 0) AS direct_count,
				       COALESCE(nd.entry_count, 0) AS entry_count,
				       COUNT(*) OVER () AS total_count
				FROM days d
				LEFT JOIN event_days ed ON ed.local_date = d.local_date
				LEFT JOIN entry_days nd ON nd.local_date = d.local_date
				ORDER BY d.local_date DESC
				LIMIT ? OFFSET ?
			)
			SELECT local_date, event_count, direct_count, entry_count, total_count, 0
			FROM page
			UNION ALL
			SELECT NULL, 0, 0, 0, (SELECT COUNT(*) FROM days), 1
			WHERE NOT EXISTS (SELECT 1 FROM page)
			ORDER BY 6, 1 DESC`
		args := make([]any, 0, len(eventArgs)+len(entryArgs)+2)
		args = append(args, eventArgs...)
		args = append(args, entryArgs...)
		args = append(args, limit, request.Offset)
		rows, queryErr := tx.QueryContext(ctx, query, args...)
		if queryErr != nil {
			return fmt.Errorf("list person days: %w", queryErr)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var day PersonDay
			var localDate sql.NullString
			var sentinel int
			if scanErr := rows.Scan(
				&localDate, &day.EventCount, &day.DirectCount, &day.EntryCount,
				&page.TotalCount, &sentinel,
			); scanErr != nil {
				return fmt.Errorf("scan person day: %w", scanErr)
			}
			if sentinel == 0 {
				day.LocalDate = localDate.String
				page.Days = append(page.Days, day)
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return fmt.Errorf("iterate person days: %w", rowsErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func (s *Store) PersonDayContext(
	ctx context.Context,
	request PersonDayRequest,
) (*PersonDayPage, error) {
	activityLimit, err := normalizeActivityPage(request.Limit, request.Offset)
	if err != nil {
		return nil, err
	}
	entryLimit, err := normalizeActivityPage(request.EntryLimit, request.EntryOffset)
	if err != nil || request.PersonID <= 0 {
		return nil, ErrInvalidActivityRequest
	}
	if err := requireActivityDate(request.LocalDate); err != nil {
		return nil, err
	}
	page := &PersonDayPage{
		PersonID: request.PersonID, LocalDate: request.LocalDate,
		Activity: []ActivityRef{}, Entries: []DailyNoteEntry{},
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := requirePersonTxContext(ctx, tx, request.PersonID); err != nil {
			return err
		}
		activity, total, err := listPersonDayActivityTx(
			ctx, tx, request.PersonID, request.LocalDate,
			activityLimit, request.Offset,
		)
		if err != nil {
			return err
		}
		page.Activity = activity
		page.ActivityTotalCount = total
		entries, entryTotal, err := listDailyNotePageTx(
			ctx, tx, request.LocalDate, &request.PersonID,
			entryLimit, request.EntryOffset,
		)
		if err != nil {
			return err
		}
		page.Entries = entries
		page.EntryTotalCount = entryTotal
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func listPersonDayActivityTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	localDate string,
	limit int,
	offset int,
) ([]ActivityRef, int64, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH links AS (
			SELECT aep.message_id,
			       MIN(aep.role) AS role,
			       CASE WHEN MAX(CASE WHEN aep.evidence = 'direct' THEN 1 ELSE 0 END) = 1
			            THEN 'direct' ELSE 'co_presence' END AS evidence
			FROM activity_event_persons aep
			WHERE aep.person_id = ? AND aep.local_date = ?
			GROUP BY aep.message_id
		),
		base AS (
			SELECT `+activityRefEventColumns+`, links.role, links.evidence
			FROM links
			JOIN activity_events ae ON ae.message_id = links.message_id
		),
		page AS (
			SELECT base.*, COUNT(*) OVER () AS total_count
			FROM base
			ORDER BY occurred_at DESC, message_id DESC
			LIMIT ? OFFSET ?
		)
		SELECT page.*, 0 AS sentinel FROM page
		UNION ALL
		SELECT NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
		       NULL, NULL, NULL, NULL, NULL, (SELECT COUNT(*) FROM base), 1
		WHERE NOT EXISTS (SELECT 1 FROM page)
		ORDER BY sentinel, occurred_at DESC, message_id DESC
	`, personID, localDate, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list person day activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	refs := make([]ActivityRef, 0)
	var total int64
	for rows.Next() {
		var rowTotal int64
		var sentinel int
		ref, present, scanErr := scanActivityRefRow(
			rows, nil, &rowTotal, &sentinel,
		)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		total = rowTotal
		if sentinel == 0 && present {
			refs = append(refs, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate person day activity: %w", err)
	}
	return refs, total, nil
}

type activityRefRow struct {
	kind, channel, localDate, timezone   sql.NullString
	dateOrigin, datePrecision, direction sql.NullString
	role, evidence                       sql.NullString
	messageID, sourceID, conversationID  sql.NullInt64
	occurredAt                           nullableTimestamp
	offset                               sql.NullInt64
}

func (row *activityRefRow) destinations() []any {
	return []any{
		&row.kind, &row.messageID, &row.channel, &row.occurredAt, &row.localDate,
		&row.timezone, &row.offset, &row.dateOrigin, &row.datePrecision,
		&row.direction, &row.sourceID, &row.conversationID, &row.role, &row.evidence,
	}
}

func (row *activityRefRow) build() (ActivityRef, bool) {
	if !row.messageID.Valid {
		return ActivityRef{}, false
	}
	return ActivityRef{
		Ref:              row.kind.String + ":" + strconv.FormatInt(row.messageID.Int64, 10),
		MessageID:        row.messageID.Int64,
		Kind:             ActivityRefKind(row.kind.String),
		Channel:          ActivityChannel(row.channel.String),
		OccurredAt:       row.occurredAt.Time.UTC(),
		LocalDate:        row.localDate.String,
		Timezone:         row.timezone.String,
		UTCOffsetMinutes: int(row.offset.Int64),
		DateOrigin:       row.dateOrigin.String,
		DatePrecision:    row.datePrecision.String,
		Direction:        ActivityDirection(row.direction.String),
		SourceID:         row.sourceID.Int64,
		ConversationID:   nullInt64Pointer(row.conversationID),
		Role:             ActivityRole(row.role.String),
		Evidence:         ActivityEvidence(row.evidence.String),
	}, true
}

func scanActivityRefRow(
	scanner scanner,
	prefix []any,
	suffix ...any,
) (ActivityRef, bool, error) {
	var raw activityRefRow
	destinations := make([]any, 0, len(prefix)+14+len(suffix))
	destinations = append(destinations, prefix...)
	destinations = append(destinations, raw.destinations()...)
	destinations = append(destinations, suffix...)
	if err := scanner.Scan(destinations...); err != nil {
		return ActivityRef{}, false, fmt.Errorf("scan activity reference: %w", err)
	}
	ref, present := raw.build()
	return ref, present, nil
}

func (s *Store) DayContext(
	ctx context.Context,
	request DayRequest,
) (*DayPage, error) {
	personLimit, err := normalizeActivityPage(request.Limit, request.Offset)
	if err != nil {
		return nil, err
	}
	entryLimit, err := normalizeActivityPage(request.EntryLimit, request.EntryOffset)
	if err != nil {
		return nil, err
	}
	activityLimit, err := normalizeActivityPage(request.ActivityLimitPerPerson, 0)
	if err != nil {
		return nil, err
	}
	if err := requireActivityDate(request.LocalDate); err != nil {
		return nil, err
	}
	page := &DayPage{
		LocalDate: request.LocalDate,
		Persons:   []DayPerson{}, Entries: []DailyNoteEntry{},
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		persons, total, err := listDayPersonsTx(
			ctx, tx, request.LocalDate, personLimit, request.Offset, activityLimit,
		)
		if err != nil {
			return err
		}
		page.Persons = persons
		page.PersonTotalCount = total
		entries, entryTotal, err := listDailyNotePageTx(
			ctx, tx, request.LocalDate, nil, entryLimit, request.EntryOffset,
		)
		if err != nil {
			return err
		}
		page.Entries = entries
		page.EntryTotalCount = entryTotal
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func listDayPersonsTx(
	ctx context.Context,
	tx *loggedTx,
	localDate string,
	limit int,
	offset int,
	activityLimit int,
) ([]DayPerson, int64, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH links AS (
			SELECT aep.person_id, aep.message_id,
			       MIN(aep.role) AS role,
			       CASE WHEN MAX(CASE WHEN aep.evidence = 'direct' THEN 1 ELSE 0 END) = 1
			            THEN 'direct' ELSE 'co_presence' END AS evidence
			FROM activity_event_persons aep
			WHERE aep.local_date = ?
			GROUP BY aep.person_id, aep.message_id
		),
		aggregated AS (
			SELECT l.person_id, p.vcard_uid, p.display_name,
			       COUNT(*) AS event_count,
			       SUM(CASE WHEN l.evidence = 'direct' THEN 1 ELSE 0 END) AS direct_count,
			       MAX(ae.occurred_at) AS last_at
			FROM links l
			JOIN activity_events ae ON ae.message_id = l.message_id
			JOIN persons p ON p.id = l.person_id
			GROUP BY l.person_id, p.vcard_uid, p.display_name
		),
		person_page AS (
			SELECT aggregated.*, COUNT(*) OVER () AS total_count
			FROM aggregated
			ORDER BY last_at DESC, person_id ASC
			LIMIT ? OFFSET ?
		),
		ranked_refs AS (
			SELECT pp.person_id, `+activityRefEventColumns+`, l.role, l.evidence,
			       ROW_NUMBER() OVER (
				       PARTITION BY pp.person_id
				       ORDER BY ae.occurred_at DESC, ae.message_id DESC
			       ) AS ref_rank
			FROM person_page pp
			JOIN links l ON l.person_id = pp.person_id
			JOIN activity_events ae ON ae.message_id = l.message_id
		),
		result AS (
			SELECT pp.person_id, pp.vcard_uid, pp.display_name,
			       pp.event_count, pp.direct_count, pp.last_at, pp.total_count,
			       rr.ref_kind, rr.message_id, rr.channel, rr.occurred_at, rr.local_date,
			       rr.timezone, rr.utc_offset_minutes, rr.date_origin, rr.date_precision,
			       rr.direction, rr.source_id, rr.conversation_id, rr.role, rr.evidence,
			       0 AS sentinel
			FROM person_page pp
			LEFT JOIN ranked_refs rr
			  ON rr.person_id = pp.person_id AND rr.ref_rank <= ?
			UNION ALL
			SELECT NULL, NULL, NULL, 0, 0, NULL, (SELECT COUNT(*) FROM aggregated),
			       NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
			       NULL, NULL, NULL, NULL, NULL, 1
			WHERE NOT EXISTS (SELECT 1 FROM person_page)
		)
		SELECT * FROM result
		ORDER BY sentinel, last_at DESC, person_id ASC, occurred_at DESC, message_id DESC
	`, localDate, limit, offset, activityLimit)
	if err != nil {
		return nil, 0, fmt.Errorf("list day persons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	persons := make([]DayPerson, 0)
	var total int64
	for rows.Next() {
		var (
			personID, eventCount, directCount sql.NullInt64
			vcardUID                          sql.NullString
			displayName                       sql.NullString
			lastAt                            nullableTimestamp
			rowTotal                          int64
			sentinel                          int
		)
		ref, present, err := scanActivityRefRow(rows, []any{
			&personID, &vcardUID, &displayName, &eventCount, &directCount,
			&lastAt, &rowTotal,
		}, &sentinel)
		if err != nil {
			return nil, 0, err
		}
		total = rowTotal
		if sentinel != 0 {
			continue
		}
		if len(persons) == 0 || persons[len(persons)-1].PersonID != personID.Int64 {
			person := DayPerson{
				PersonID: personID.Int64, VCardUID: vcardUID.String,
				EventCount: eventCount.Int64, DirectCount: directCount.Int64,
				LastAt: lastAt.Time, Activity: []ActivityRef{},
			}
			if displayName.Valid {
				person.DisplayName = &displayName.String
			}
			persons = append(persons, person)
		}
		if present {
			persons[len(persons)-1].Activity = append(
				persons[len(persons)-1].Activity, ref,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate day persons: %w", err)
	}
	for index := range persons {
		persons[index].ActivityTruncated =
			int64(len(persons[index].Activity)) < persons[index].EventCount
	}
	return persons, total, nil
}

func listDailyNotePageTx(
	ctx context.Context,
	tx *loggedTx,
	localDate string,
	personID *int64,
	limit int,
	offset int,
) ([]DailyNoteEntry, int64, error) {
	filter := "e.local_date = ?"
	args := []any{localDate}
	if personID != nil {
		filter += ` AND EXISTS (
			SELECT 1 FROM daily_note_entry_persons filter_target
			WHERE filter_target.entry_id = e.id AND filter_target.person_id = ?
		)`
		args = append(args, *personID)
	}
	args = append(args, limit, offset)
	rows, err := queryDailyNoteRowsTx(ctx, tx, `
		WITH base AS (
			SELECT e.id, e.local_date, e.ordinal, e.body, e.author, e.source,
			       e.source_ref, e.created_at, e.updated_at
			FROM daily_note_entries e
			WHERE `+filter+`
		),
		page AS (
			SELECT base.*, COUNT(*) OVER () AS total_count
			FROM base
			ORDER BY ordinal, id
			LIMIT ? OFFSET ?
		)
		SELECT page.id, page.local_date, page.ordinal, page.body, page.author,
		       page.source, page.source_ref, page.created_at, page.updated_at,
		       target.person_id, page.total_count, 0 AS sentinel
		FROM page
		LEFT JOIN daily_note_entry_persons target ON target.entry_id = page.id
		UNION ALL
		SELECT NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
		       (SELECT COUNT(*) FROM base), 1
		WHERE NOT EXISTS (SELECT 1 FROM page)
		ORDER BY sentinel, ordinal, id, person_id
	`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list daily note page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanDailyNotePageRows(rows)
}

func (s *Store) contactStateTxContext(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	now time.Time,
) (ContactState, error) {
	if err := requirePersonTxContext(ctx, tx, personID); err != nil {
		return ContactState{}, err
	}
	state := ContactState{
		PersonID:      personID,
		CadenceStatus: CadenceUnknown,
	}
	currentRevisions, globalStale, err := s.contactStateFreshnessTxContext(
		ctx, tx)
	if err != nil {
		return ContactState{}, err
	}
	var (
		firstAt, lastAt, inboundAt, outboundAt sql.NullTime
		firstID, lastID, inboundID, outboundID sql.NullInt64
		channel, owner                         sql.NullString
		sourceID                               sql.NullInt64
		dirty                                  sql.NullTime
		identity, account                      int64
		computed                               sql.NullTime
		firstKind, lastKind                    sql.NullString
		inboundKind, outboundKind              sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT first_contact_at, first_contact_message_id,
		       last_contact_at, last_contact_message_id, last_contact_channel,
		       last_contact_source_id, last_contact_owner,
		       last_inbound_at, last_inbound_message_id,
		       last_outbound_at, last_outbound_message_id,
		       interaction_count, identity_revision, account_identity_revision,
		       dirty_at, computed_at,
		       (SELECT ref_kind FROM activity_events
		        WHERE message_id = first_contact_message_id),
		       (SELECT ref_kind FROM activity_events
		        WHERE message_id = last_contact_message_id),
		       (SELECT ref_kind FROM activity_events
		        WHERE message_id = last_inbound_message_id),
		       (SELECT ref_kind FROM activity_events
		        WHERE message_id = last_outbound_message_id)
		FROM person_contact_state
		WHERE person_id = ?
	`, personID).Scan(
		&firstAt, &firstID,
		&lastAt, &lastID, &channel, &sourceID, &owner,
		&inboundAt, &inboundID,
		&outboundAt, &outboundID,
		&state.InteractionCount, &identity, &account,
		&dirty, &computed,
		&firstKind, &lastKind, &inboundKind, &outboundKind,
	)
	hasStoredState := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ContactState{}, fmt.Errorf("read contact state: %w", err)
	}
	if hasStoredState {
		state.FirstContactAt = utcNullTimePointer(firstAt)
		state.LastContactAt = utcNullTimePointer(lastAt)
		state.LastInboundAt = utcNullTimePointer(inboundAt)
		state.LastOutboundAt = utcNullTimePointer(outboundAt)
		state.FirstContactRef = activityStoredRef(firstKind, firstID)
		state.LastContactRef = activityStoredRef(lastKind, lastID)
		state.LastInboundRef = activityStoredRef(inboundKind, inboundID)
		state.LastOutboundRef = activityStoredRef(outboundKind, outboundID)
		if channel.Valid {
			state.LastContactChannel = ActivityChannel(channel.String)
		}
		state.LastContactSourceID = nullInt64Pointer(sourceID)
		if owner.Valid {
			state.LastContactOwner = owner.String
		}
		if computed.Valid {
			state.ComputedAt = computed.Time.UTC()
		}
		state.Stale = dirty.Valid ||
			identity != currentRevisions.IdentityRevision ||
			account != currentRevisions.AccountIdentityRevision ||
			globalStale
	} else {
		var hasDirect int
		err := tx.QueryRowContext(ctx, `
			SELECT 1
			WHERE EXISTS (
				SELECT 1
				FROM activity_event_persons
				WHERE person_id = ? AND evidence = 'direct'
			)
		`, personID).Scan(&hasDirect)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ContactState{}, fmt.Errorf("check direct contact evidence: %w", err)
		}
		state.Stale = err == nil || globalStale
	}

	var inferred string
	err = tx.QueryRowContext(ctx, `
		SELECT ae.channel
		FROM activity_events ae
		WHERE EXISTS (
			SELECT 1
			FROM activity_event_persons aep
			WHERE aep.message_id = ae.message_id
			  AND aep.person_id = ?
			  AND aep.evidence = 'direct'
		)
		GROUP BY ae.channel
		ORDER BY COUNT(*) DESC,
		         CASE ae.channel
		             WHEN 'email' THEN 1
		             WHEN 'chat' THEN 2
		             WHEN 'meeting' THEN 3
		             WHEN 'other' THEN 4
		             ELSE 5
		         END ASC
		LIMIT 1
	`, personID).Scan(&inferred)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ContactState{}, fmt.Errorf("infer contact channel: %w", err)
	}
	if err == nil {
		state.InferredChannel = ActivityChannel(inferred)
	}

	values, err := listCurrentContactFrequencyTx(ctx, tx, personID)
	if err != nil {
		return ContactState{}, err
	}
	deriveContactCadence(&state, values, now)
	return state, nil
}

// contactStateFreshnessTxContext returns the current identity epoch and the
// conservative global freshness barrier for contact projections. A pending
// queue row can introduce a new activity/person binding before an evidence row
// or contact-state row exists, so a missing row cannot be called fresh while
// any projection work remains. The reconciled epoch is also global: a pass
// that has not reached the current identity revisions may still change every
// contact's owner-relative evidence. Active classification-policy transitions
// (timezone, direct-counterpart limit) are part of the barrier too: their
// replay drains the queue between batches, so an empty queue mid-replay would
// otherwise expose partially reclassified counts as fresh.
func (s *Store) contactStateFreshnessTxContext(
	ctx context.Context,
	tx *loggedTx,
) (ContactRevisions, bool, error) {
	identity, err := s.currentIdentityRevisionTx(tx)
	if err != nil {
		return ContactRevisions{}, false,
			fmt.Errorf("read contact identity revision: %w", err)
	}
	account, err := readAccountIdentityRevision(tx)
	if err != nil {
		return ContactRevisions{}, false,
			fmt.Errorf("read contact account identity revision: %w", err)
	}
	revisions := ContactRevisions{
		IdentityRevision:        identity,
		AccountIdentityRevision: account,
	}
	reconciled, err := s.activityEpochReconciledTx(ctx, tx, revisions)
	if err != nil {
		return ContactRevisions{}, false,
			fmt.Errorf("read reconciled contact epoch: %w", err)
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `
		SELECT CASE WHEN EXISTS (
			SELECT 1
			FROM activity_projection_queue
			WHERE revision > processed_revision
		) OR EXISTS (
			SELECT 1
			FROM archive_metadata
			WHERE key IN (?, ?)
		) THEN 1 ELSE 0 END
	`, activityTimezoneActiveKey, activityDirectLimitActiveKey,
	).Scan(&pending); err != nil {
		return ContactRevisions{}, false,
			fmt.Errorf("check pending contact projections: %w", err)
	}
	return revisions, pending != 0 || !reconciled, nil
}

func utcNullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func listCurrentContactFrequencyTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
) ([]contactFrequencyValue, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT d.value_type,
		       v.value_text,
		       CAST(v.value_integer AS TEXT),
		       CAST(v.value_real AS TEXT),
		       CAST(v.value_boolean AS TEXT),
		       v.value_date,
		       CAST(v.value_timestamp AS TEXT),
		       CAST(v.value_json AS TEXT),
		       v.value_record_type,
		       CAST(v.value_record_id AS TEXT)
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.person_id = ?
		  AND d.universal_id = ?
		  AND d.object_type = ?
		  AND v.active_until IS NULL
		  AND v.superseded_at IS NULL
		ORDER BY v.ordinal, v.id
	`,
		personID, AttributeUniversalIDContactFrequency, string(AttributeObjectPerson))
	if err != nil {
		return nil, fmt.Errorf("read contact frequency: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values := make([]contactFrequencyValue, 0)
	for rows.Next() {
		var (
			valueType                                  string
			text, integer, realValue, boolean, date    sql.NullString
			timestamp, jsonValue, recordType, recordID sql.NullString
		)
		if err := rows.Scan(
			&valueType,
			&text, &integer, &realValue, &boolean, &date,
			&timestamp, &jsonValue, &recordType, &recordID,
		); err != nil {
			return nil, fmt.Errorf("scan contact frequency: %w", err)
		}
		value := contactFrequencyValue{}
		populated := 0
		for _, field := range []sql.NullString{
			text, integer, realValue, boolean, date,
			timestamp, jsonValue, recordID,
		} {
			if field.Valid {
				populated++
			}
		}
		if recordType.Valid != recordID.Valid {
			populated++
		}
		if valueType == string(AttributeValueInteger) &&
			populated == 1 &&
			integer.Valid {
			days, parseErr := strconv.ParseInt(integer.String, 10, 64)
			if parseErr == nil {
				value.Days = days
				value.Valid = true
			}
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contact frequency: %w", err)
	}
	return values, nil
}

type contactFrequencyValue struct {
	Days  int64
	Valid bool
}

func deriveContactCadence(
	state *ContactState,
	values []contactFrequencyValue,
	now time.Time,
) {
	state.CadenceStatus = CadenceUnknown
	state.CadenceDueAt = nil
	if state.LastContactAt == nil || len(values) != 1 {
		return
	}
	value := values[0]
	if !value.Valid || value.Days <= 0 {
		return
	}
	days64 := value.Days
	// This bound is deliberately wider than every due date representable by
	// JSON's year range and prevents overflow inside time.Date normalization.
	if days64 > 366*10_000 {
		return
	}
	days := int(days64)
	if int64(days) != days64 {
		return
	}
	last := state.LastContactAt.UTC()
	due := last.AddDate(0, 0, days)
	if !due.After(last) || due.Year() < 0 || due.Year() > 9999 {
		return
	}
	state.CadenceDueAt = &due
	if now.After(due) {
		state.CadenceStatus = CadenceOverdue
	} else {
		state.CadenceStatus = CadenceOK
	}
}
