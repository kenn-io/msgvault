package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

const meetingResultSchemaVersion = 1
const meetingSenderRole = "from"

var (
	ErrMeetingNotFound              = errors.New("meeting not found")
	ErrNotMeeting                   = errors.New("message is not a meeting")
	ErrMeetingProjectionUnavailable = errors.New("meeting projection unavailable")
	ErrMeetingScopeChanged          = errors.New("meeting scope changed")
	ErrMeetingInvalidCursor         = errors.New("invalid meeting cursor")
)

// MeetingSelectionError reports a bounded set of internal archive IDs that
// failed one atomic meeting selection check. Err is one of the meeting
// sentinels above and remains available through errors.Is.
type MeetingSelectionError struct {
	Err        error   `json:"-"`
	MessageIDs []int64 `json:"message_ids"`
}

func (e *MeetingSelectionError) Error() string {
	return fmt.Sprintf("%v: message IDs %v", e.Err, e.MessageIDs)
}

func (e *MeetingSelectionError) Unwrap() error { return e.Err }

type MeetingQueryScope struct {
	MessageIDs     *[]int64
	SourceIDs      []int64
	ParticipantIDs []int64
	Person         *personscope.Scope
	Domains        []string
	After          *time.Time
	Before         *time.Time
	Deletion       string
	Authority      string
}

type meetingScopeStatement struct {
	cte  string
	args []any
}

type meetingContextRow struct {
	meeting  meetingcontent.MeetingRef
	content  meetingcontent.Content
	metadata sql.NullString
}

// GetMeetingContextContext atomically validates and loads the requested
// archived meetings. Compact projections serve the normal path; transcript
// opt-in performs one bounded raw primary-key read at a time in the same read
// snapshot.
func (s *Store) GetMeetingContextContext(
	ctx context.Context, ids []int64, options meetingcontent.PacketOptions,
) (*meetingcontent.PacketResult, error) {
	ids = normalizedSignedIDs(ids)
	var archiveUID string
	entries := make([]meetingcontent.Entry, 0, len(ids))
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		archiveUID, err = archiveUIDFromMeetingSnapshot(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.validateMeetingContextIDs(ctx, tx, ids); err != nil {
			return err
		}
		for _, id := range ids {
			row, loadErr := s.loadMeetingContextRow(ctx, tx, id, options.IncludeTranscript)
			if loadErr != nil {
				return loadErr
			}
			participants, participantErr := loadMeetingContextParticipants(ctx, tx, id)
			if participantErr != nil {
				return participantErr
			}
			entries = append(entries, meetingcontent.Entry{
				Meeting: row.meeting, Participants: participants, Content: row.content,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get meeting context: %w", err)
	}
	return meetingcontent.Render(archiveUID, entries, options)
}

func (s *Store) validateMeetingContextIDs(ctx context.Context, tx *loggedTx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	clause, args, err := s.meetingIDsMembership(`m.id`, ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.message_type, m.deleted_at, d.message_id IS NOT NULL
		FROM messages m
		LEFT JOIN meeting_details d ON d.message_id = m.id
		WHERE `+clause, args...)
	if err != nil {
		return fmt.Errorf("validate meeting context IDs: %w", err)
	}
	type state struct {
		messageType   string
		deleted       bool
		hasProjection bool
	}
	found := make(map[int64]state, len(ids))
	for rows.Next() {
		var id int64
		var messageType string
		var deletedAt nullableTimestamp
		var hasProjection bool
		if scanErr := rows.Scan(&id, &messageType, &deletedAt, &hasProjection); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("scan meeting context validation: %w", scanErr)
		}
		found[id] = state{messageType: messageType, deleted: deletedAt.Valid, hasProjection: hasProjection}
	}
	if iterationErr := rows.Err(); iterationErr != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate meeting context validation: %w", iterationErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return fmt.Errorf("close meeting context validation: %w", closeErr)
	}

	missing := make([]int64, 0)
	notMeeting := make([]int64, 0)
	unavailable := make([]int64, 0)
	for _, id := range ids {
		item, ok := found[id]
		switch {
		case !ok || item.deleted:
			missing = append(missing, id)
		case item.messageType != "meeting_transcript":
			notMeeting = append(notMeeting, id)
		case !item.hasProjection:
			unavailable = append(unavailable, id)
		}
	}
	if len(missing) > 0 {
		return newMeetingSelectionError(ErrMeetingNotFound, missing)
	}
	if len(notMeeting) > 0 {
		return newMeetingSelectionError(ErrNotMeeting, notMeeting)
	}
	if len(unavailable) > 0 {
		return newMeetingSelectionError(ErrMeetingProjectionUnavailable, unavailable)
	}
	return nil
}

func (s *Store) validateMeetingScopePopulation(
	ctx context.Context, tx *loggedTx, scope MeetingQueryScope,
) error {
	if scope.Authority == "" {
		return nil
	}
	if scope.MessageIDs == nil {
		return newMeetingSelectionError(ErrMeetingScopeChanged, nil)
	}
	ids := normalizedSignedIDs(*scope.MessageIDs)
	if len(ids) == 0 {
		return nil
	}
	clause, args, err := s.meetingIDsMembership(`m.id`, ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.message_type, m.deleted_at
		FROM messages m WHERE `+clause, args...)
	if err != nil {
		return fmt.Errorf("validate meeting scope population: %w", err)
	}
	valid := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		var messageType string
		var deletedAt nullableTimestamp
		if scanErr := rows.Scan(&id, &messageType, &deletedAt); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("scan meeting scope population: %w", scanErr)
		}
		valid[id] = messageType == "meeting_transcript" && !deletedAt.Valid
	}
	if iterationErr := rows.Err(); iterationErr != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate meeting scope population: %w", iterationErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return fmt.Errorf("close meeting scope population: %w", closeErr)
	}
	changed := make([]int64, 0)
	for _, id := range ids {
		if !valid[id] {
			changed = append(changed, id)
		}
	}
	if len(changed) > 0 {
		return newMeetingSelectionError(ErrMeetingScopeChanged, changed)
	}
	return nil
}

func (s *Store) loadMeetingContextRow(
	ctx context.Context, tx *loggedTx, id int64, includeTranscript bool,
) (meetingContextRow, error) {
	var row meetingContextRow
	var occurredAt nullableTimestamp
	var contentJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT m.id, m.conversation_id, m.source_id, s.source_type, s.identifier,
			COALESCE(m.source_message_id, ''),
			COALESCE(NULLIF(m.subject, ''), NULLIF(c.title, ''), ''),
			COALESCE(m.sent_at, m.received_at, m.internal_date),
			d.content_json, m.metadata
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		JOIN conversations c ON c.id = m.conversation_id
		JOIN meeting_details d ON d.message_id = m.id
		WHERE m.id = ?`, id).Scan(
		&row.meeting.MessageID,
		&row.meeting.ConversationID,
		&row.meeting.SourceID,
		&row.meeting.SourceType,
		&row.meeting.SourceIdentifier,
		&row.meeting.SourceMessageID,
		&row.meeting.Title,
		&occurredAt,
		&contentJSON,
		&row.metadata,
	)
	if err != nil {
		return meetingContextRow{}, fmt.Errorf("load meeting context %d: %w", id, err)
	}
	row.meeting.OccurredAt = nullableTimePointer(occurredAt)
	row.meeting.ArchivePath = fmt.Sprintf("/api/v1/messages/%d", id)
	if err := json.Unmarshal([]byte(contentJSON), &row.content); err != nil {
		return meetingContextRow{}, newMeetingSelectionError(ErrMeetingProjectionUnavailable, []int64{id})
	}
	if includeTranscript {
		row.content.Transcript, err = s.loadMeetingTranscript(ctx, tx, id, row.metadata)
		if err != nil {
			return meetingContextRow{}, err
		}
	}
	return row, nil
}

func (s *Store) loadMeetingTranscript(
	ctx context.Context, tx *loggedTx, id int64, metadata sql.NullString,
) (meetingcontent.Transcript, error) {
	var compressed []byte
	var format string
	var compression sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT raw_data, raw_format, compression
		FROM message_raw WHERE message_id = ?`, id).Scan(&compressed, &format, &compression)
	if errors.Is(err, sql.ErrNoRows) {
		return meetingcontent.Decode("", nil, []byte(metadata.String)).Transcript, nil
	}
	if err != nil {
		return meetingcontent.Transcript{}, fmt.Errorf("load meeting transcript %d: %w", id, err)
	}
	raw, _, err := decodeMessageRawBounded(compressed, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		reason := "invalid_raw"
		if errors.Is(err, errMeetingRawTooLarge) {
			reason = "raw_too_large"
		}
		return meetingcontent.Transcript{State: meetingcontent.StateUnavailable, Reason: reason}, nil
	}
	return meetingcontent.Decode(format, raw, []byte(metadata.String)).Transcript, nil
}

func loadMeetingContextParticipants(
	ctx context.Context, tx *loggedTx, id int64,
) ([]meetingcontent.Participant, error) {
	participants := make([]meetingcontent.Participant, 0)
	seen := make(map[string]struct{})
	seenRoleID := make(map[string]struct{})
	rows, err := tx.QueryContext(ctx, `
		SELECT mr.participant_id,
			COALESCE(NULLIF(mr.display_name, ''), NULLIF(p.display_name, ''), ''),
			COALESCE(NULLIF(mr.email_address, ''), NULLIF(p.email_address, ''), ''),
			LOWER(mr.recipient_type)
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ?
		ORDER BY mr.id`, id)
	if err != nil {
		return nil, fmt.Errorf("load meeting recipients %d: %w", id, err)
	}
	for rows.Next() {
		var participant meetingcontent.Participant
		var participantID int64
		if scanErr := rows.Scan(&participantID, &participant.Name, &participant.Email, &participant.Role); scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan meeting recipient %d: %w", id, scanErr)
		}
		participant.ParticipantID = new(participantID)
		seenRoleID[meetingParticipantRoleIDKey(participant.Role, participantID)] = struct{}{}
		key := meetingParticipantKey(participant)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		participants = append(participants, participant)
	}
	if iterationErr := rows.Err(); iterationErr != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate meeting recipients %d: %w", id, iterationErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return nil, fmt.Errorf("close meeting recipients %d: %w", id, closeErr)
	}

	var sender meetingcontent.Participant
	var senderID int64
	err = tx.QueryRowContext(ctx, `
		SELECT p.id, COALESCE(p.display_name, ''), COALESCE(p.email_address, '')
		FROM messages m
		JOIN participants p ON p.id = m.sender_id
		WHERE m.id = ?`, id).Scan(&senderID, &sender.Name, &sender.Email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load meeting sender %d: %w", id, err)
	}
	if err == nil {
		sender.ParticipantID = new(senderID)
		sender.Role = meetingSenderRole
		key := meetingParticipantKey(sender)
		_, archived := seenRoleID[meetingParticipantRoleIDKey(sender.Role, senderID)]
		if _, exists := seen[key]; !exists && !archived {
			participants = append(participants, sender)
		}
	}
	return participants, nil
}

func meetingParticipantRoleIDKey(role string, participantID int64) string {
	return fmt.Sprintf("%s\x00%d", strings.ToLower(role), participantID)
}

func meetingParticipantKey(participant meetingcontent.Participant) string {
	id := int64(0)
	if participant.ParticipantID != nil {
		id = *participant.ParticipantID
	}
	return fmt.Sprintf("%d\x00%s\x00%s", id, strings.ToLower(participant.Role), strings.ToLower(participant.Email))
}

func (s *Store) meetingIDsMembership(column string, ids []int64) (string, []any, error) {
	if len(ids) <= 500 {
		return column + ` IN (` + meetingPlaceholders(len(ids)) + `)`, appendInt64Args(nil, ids), nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", nil, fmt.Errorf("encode meeting IDs: %w", err)
	}
	if s.IsPostgreSQL() {
		return column + ` IN (
			SELECT CAST(value AS BIGINT)
			FROM jsonb_array_elements_text(CAST(? AS jsonb)) AS meeting_id(value)
		)`, []any{string(encoded)}, nil
	}
	return column + ` IN (SELECT CAST(value AS INTEGER) FROM json_each(?))`, []any{string(encoded)}, nil
}

func newMeetingSelectionError(kind error, ids []int64) *MeetingSelectionError {
	const maxReportedMeetingIDs = 100
	ids = normalizedSignedIDs(ids)
	if len(ids) > maxReportedMeetingIDs {
		ids = ids[:maxReportedMeetingIDs]
	}
	return &MeetingSelectionError{Err: kind, MessageIDs: ids}
}

func (s *Store) buildMeetingScopeStatement(scope MeetingQueryScope) (meetingScopeStatement, error) {
	conditions := []string{
		`m.message_type = 'meeting_transcript'`,
		`m.deleted_at IS NULL`,
	}
	args := make([]any, 0)
	if scope.MessageIDs != nil {
		ids := normalizedSignedIDs(*scope.MessageIDs)
		if len(ids) == 0 {
			conditions = append(conditions, `FALSE`)
		} else {
			clause, idArgs, err := s.meetingIDsMembership(`m.id`, ids)
			if err != nil {
				return meetingScopeStatement{}, err
			}
			conditions = append(conditions, clause)
			args = append(args, idArgs...)
		}
	}
	if ids := normalizedSignedIDs(scope.SourceIDs); len(ids) > 0 {
		conditions = append(conditions, `m.source_id IN (`+meetingPlaceholders(len(ids))+`)`)
		args = appendInt64Args(args, ids)
	}
	if ids := normalizedSignedIDs(scope.ParticipantIDs); len(ids) > 0 {
		placeholders := meetingPlaceholders(len(ids))
		conditions = append(conditions, `(m.sender_id IN (`+placeholders+`) OR EXISTS (
			SELECT 1 FROM message_recipients exact_recipient
			WHERE exact_recipient.message_id = m.id
			  AND exact_recipient.participant_id IN (`+placeholders+`)
		) OR EXISTS (
			SELECT 1 FROM conversation_participants exact_roster
			WHERE exact_roster.conversation_id = m.conversation_id
			  AND exact_roster.participant_id IN (`+placeholders+`)
		))`)
		for range 3 {
			args = appendInt64Args(args, ids)
		}
	}
	if scope.Person != nil {
		if err := personscope.Validate(*scope.Person); err != nil {
			return meetingScopeStatement{}, fmt.Errorf("invalid meeting person scope: %w", err)
		}
		predicate, personArgs := personscope.MessagePredicate(*scope.Person, "m", "c")
		conditions = append(conditions, `(`+predicate+`)`)
		args = append(args, personArgs...)
	}
	if domains := normalizedMeetingDomains(scope.Domains); len(domains) > 0 {
		placeholders := meetingPlaceholders(len(domains))
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM participants domain_participant
			WHERE LOWER(TRIM(COALESCE(domain_participant.domain, ''))) IN (`+placeholders+`)
			  AND (domain_participant.id = m.sender_id OR EXISTS (
				SELECT 1 FROM message_recipients domain_recipient
				WHERE domain_recipient.message_id = m.id
				  AND domain_recipient.participant_id = domain_participant.id
			  ) OR EXISTS (
				SELECT 1 FROM conversation_participants domain_roster
				WHERE domain_roster.conversation_id = m.conversation_id
				  AND domain_roster.participant_id = domain_participant.id
			  ))
		)`)
		for _, domain := range domains {
			args = append(args, domain)
		}
	}
	occurredAt := `COALESCE(m.sent_at, m.received_at, m.internal_date)`
	occurredKey := occurredAt
	if !s.IsPostgreSQL() {
		occurredKey = sqliteutil.TimestampKeyFunction + `(` + occurredAt + `)`
	}
	if scope.After != nil {
		if s.IsPostgreSQL() {
			conditions = append(conditions, occurredAt+` >= ?`)
			args = append(args, scope.After.UTC())
		} else {
			conditions = append(conditions, occurredKey+` >= ?`)
			args = append(args, sqliteutil.TimestampKey(*scope.After))
		}
	}
	if scope.Before != nil {
		if s.IsPostgreSQL() {
			conditions = append(conditions, occurredAt+` < ?`)
			args = append(args, scope.Before.UTC())
		} else {
			conditions = append(conditions, occurredKey+` < ?`)
			args = append(args, sqliteutil.TimestampKey(*scope.Before))
		}
	}
	switch strings.ToLower(strings.TrimSpace(scope.Deletion)) {
	case "", "any":
	case "active":
		conditions = append(conditions, `m.deleted_from_source_at IS NULL`)
	case "deleted":
		conditions = append(conditions, `m.deleted_from_source_at IS NOT NULL`)
	default:
		return meetingScopeStatement{}, fmt.Errorf("invalid meeting deletion scope %q", scope.Deletion)
	}

	return meetingScopeStatement{
		cte: `WITH scoped_meetings AS (
			SELECT m.id AS message_id, m.conversation_id, m.source_id,
				s.source_type, s.identifier AS source_identifier,
				COALESCE(m.source_message_id, '') AS source_message_id,
				COALESCE(NULLIF(m.subject, ''), NULLIF(c.title, ''), '') AS title,
				` + occurredAt + ` AS occurred_at,
				` + occurredKey + ` AS occurred_key,
				d.duration_seconds, d.duration_basis,
				COALESCE(d.action_coverage, 'unavailable') AS action_coverage
			FROM messages m
			JOIN sources s ON s.id = m.source_id
			JOIN conversations c ON c.id = m.conversation_id
			LEFT JOIN meeting_details d ON d.message_id = m.id
			WHERE ` + strings.Join(conditions, ` AND `) + `
		)`,
		args: args,
	}, nil
}

func normalizedSignedIDs(values []int64) []int64 {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func normalizedMeetingDomains(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func appendInt64Args(args []any, values []int64) []any {
	for _, value := range values {
		args = append(args, value)
	}
	return args
}

func meetingPlaceholders(count int) string {
	if count < 1 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func meetingScopeProvenance(scope MeetingQueryScope) meetingcontent.ScopeProvenance {
	kind := "direct"
	if scope.Authority != "" {
		kind = "explore"
	}
	return meetingcontent.ScopeProvenance{Kind: kind}
}

func archiveUIDFromMeetingSnapshot(ctx context.Context, tx *loggedTx) (string, error) {
	var uid string
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, archiveUIDKey,
	).Scan(&uid); err != nil {
		return "", fmt.Errorf("read archive UID: %w", err)
	}
	return uid, nil
}

func nullableTimePointer(value nullableTimestamp) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func nullableFloatPointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	result := value.Float64
	return &result
}
