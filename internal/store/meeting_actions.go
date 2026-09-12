package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

const (
	meetingActionsDefaultLimit = 50
	meetingActionsMaxLimit     = 200
	meetingActionsCursorBytes  = 2 << 10
	meetingActionsCursorV1     = 1
)

type MeetingActionsQuery struct {
	Scope         MeetingQueryScope
	AssigneeEmail string
	Status        meetingcontent.Status
	Query         string
	Limit         int
	Cursor        string
}

type MeetingCursorError struct {
	Reason string `json:"reason,omitempty"`
}

func (e *MeetingCursorError) Error() string {
	if e.Reason == "" {
		return ErrMeetingInvalidCursor.Error()
	}
	return ErrMeetingInvalidCursor.Error() + ": " + e.Reason
}

func (e *MeetingCursorError) Unwrap() error { return ErrMeetingInvalidCursor }

type meetingActionsCursor struct {
	Version    int        `json:"version"`
	ArchiveUID string     `json:"archive_uid"`
	FilterHash string     `json:"filter_hash"`
	OccurredAt *time.Time `json:"occurred_at"`
	MessageID  *int64     `json:"message_id"`
	Ordinal    *int       `json:"ordinal"`
}

type meetingActionsFilter struct {
	MessageIDs     *[]int64           `json:"message_ids"`
	SourceIDs      []int64            `json:"source_ids"`
	ParticipantIDs []int64            `json:"participant_ids"`
	Person         *personscope.Scope `json:"person"`
	Domains        []string           `json:"domains"`
	After          string             `json:"after,omitempty"`
	Before         string             `json:"before,omitempty"`
	Deletion       string             `json:"deletion"`
	Authority      string             `json:"authority"`
	AssigneeEmail  string             `json:"assignee_email"`
	Status         string             `json:"status"`
	Query          string             `json:"query"`
}

func (s *Store) ListMeetingActionsContext(
	ctx context.Context, query MeetingActionsQuery,
) (*meetingcontent.ActionsPage, error) {
	limit, err := normalizedMeetingActionsLimit(query.Limit)
	if err != nil {
		return nil, err
	}
	if err := validateMeetingActionStatus(query.Status); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(query.Query) > 256 {
		return nil, errors.New("meeting action query exceeds 256 runes")
	}
	statement, err := s.buildMeetingScopeStatement(query.Scope)
	if err != nil {
		return nil, err
	}
	filterHash, err := meetingActionsFilterHash(query)
	if err != nil {
		return nil, fmt.Errorf("hash meeting action filters: %w", err)
	}
	var cursor *meetingActionsCursor
	if query.Cursor != "" {
		decoded, decodeErr := decodeMeetingActionsCursor(query.Cursor)
		if decodeErr != nil {
			return nil, decodeErr
		}
		cursor = &decoded
	}
	filters, filterArgs := s.meetingActionFilters(query)
	result := &meetingcontent.ActionsPage{
		SchemaVersion: meetingResultSchemaVersion,
		Rows:          []meetingcontent.ActionRow{},
		Scope:         meetingScopeProvenance(query.Scope),
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var readErr error
		result.ArchiveUID, readErr = archiveUIDFromMeetingSnapshot(ctx, tx)
		if readErr != nil {
			return readErr
		}
		if cursor != nil && (cursor.ArchiveUID != result.ArchiveUID || cursor.FilterHash != filterHash) {
			return &MeetingCursorError{Reason: "archive or filters changed"}
		}
		if readErr = s.validateMeetingScopePopulation(ctx, tx, query.Scope); readErr != nil {
			return readErr
		}

		totalArgs := append(slices.Clone(statement.args), filterArgs...)
		readErr = tx.QueryRowContext(ctx, statement.cte+`
			SELECT COUNT(*)
			FROM scoped_meetings sm
			JOIN meeting_action_items a ON a.message_id = sm.message_id`+filters,
			totalArgs...).Scan(&result.TotalCount)
		if readErr != nil {
			return fmt.Errorf("count meeting actions: %w", readErr)
		}

		readErr = tx.QueryRowContext(ctx, statement.cte+`
			SELECT COUNT(*),
				COALESCE(SUM(CASE WHEN action_coverage = 'available' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN action_coverage = 'partial' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN action_coverage = 'unsupported' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN action_coverage NOT IN ('available', 'partial', 'unsupported') THEN 1 ELSE 0 END), 0)
			FROM scoped_meetings`, statement.args...).Scan(
			&result.Coverage.MeetingCount,
			&result.Coverage.Available,
			&result.Coverage.Partial,
			&result.Coverage.Unsupported,
			&result.Coverage.Unavailable,
		)
		if readErr != nil {
			return fmt.Errorf("read meeting action coverage: %w", readErr)
		}

		positionSQL, positionArgs := s.meetingActionPosition(cursor)
		rowArgs := append(slices.Clone(statement.args), filterArgs...)
		rowArgs = append(rowArgs, positionArgs...)
		rowArgs = append(rowArgs, limit+1)

		rows, queryErr := tx.QueryContext(ctx, statement.cte+`
			SELECT sm.message_id, sm.conversation_id, sm.source_id,
				sm.source_type, sm.source_identifier, sm.source_message_id,
				sm.title, sm.occurred_at,
				a.ordinal, a.source_id, a.title, a.description,
				a.assignee_name, a.assignee_email, a.status, a.source_status,
				a.due_date, a.origin, a.locator
			FROM scoped_meetings sm
			JOIN meeting_action_items a ON a.message_id = sm.message_id`+
			filters+positionSQL+`
			ORDER BY CASE WHEN sm.occurred_key IS NULL THEN 1 ELSE 0 END,
				sm.occurred_key DESC, sm.message_id DESC, a.ordinal ASC
			LIMIT ?`, rowArgs...)
		if queryErr != nil {
			return fmt.Errorf("list meeting actions: %w", queryErr)
		}
		for rows.Next() {
			var row meetingcontent.ActionRow
			var occurredAt nullableTimestamp
			if scanErr := rows.Scan(
				&row.Meeting.MessageID,
				&row.Meeting.ConversationID,
				&row.Meeting.SourceID,
				&row.Meeting.SourceType,
				&row.Meeting.SourceIdentifier,
				&row.Meeting.SourceMessageID,
				&row.Meeting.Title,
				&occurredAt,
				&row.Action.Ordinal,
				&row.Action.SourceID,
				&row.Action.Title,
				&row.Action.Description,
				&row.Action.AssigneeName,
				&row.Action.AssigneeEmail,
				&row.Action.Status,
				&row.Action.SourceStatus,
				&row.Action.DueDate,
				&row.Action.Origin,
				&row.Action.Locator,
			); scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("scan meeting action: %w", scanErr)
			}
			row.Meeting.OccurredAt = nullableTimePointer(occurredAt)
			row.Meeting.ArchivePath = fmt.Sprintf("/api/v1/messages/%d", row.Meeting.MessageID)
			result.Rows = append(result.Rows, row)
		}
		if iterationErr := rows.Err(); iterationErr != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate meeting actions: %w", iterationErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return fmt.Errorf("close meeting actions: %w", closeErr)
		}
		if len(result.Rows) > limit {
			result.Rows = result.Rows[:limit]
			last := result.Rows[len(result.Rows)-1]
			result.NextCursor, readErr = encodeMeetingActionsCursor(meetingActionsCursor{
				Version:    meetingActionsCursorV1,
				ArchiveUID: result.ArchiveUID,
				FilterHash: filterHash,
				OccurredAt: last.Meeting.OccurredAt,
				MessageID:  new(last.Meeting.MessageID),
				Ordinal:    new(last.Action.Ordinal),
			})
			if readErr != nil {
				return readErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list meeting actions: %w", err)
	}
	return result, nil
}

func normalizedMeetingActionsLimit(value int) (int, error) {
	if value == 0 {
		return meetingActionsDefaultLimit, nil
	}
	if value < 1 || value > meetingActionsMaxLimit {
		return 0, fmt.Errorf("meeting action limit must be between 1 and %d", meetingActionsMaxLimit)
	}
	return value, nil
}

func validateMeetingActionStatus(status meetingcontent.Status) error {
	switch status {
	case "", meetingcontent.StatusPending, meetingcontent.StatusCompleted,
		meetingcontent.StatusCancelled, meetingcontent.StatusUnknown:
		return nil
	default:
		return fmt.Errorf("invalid meeting action status %q", status)
	}
}

func (s *Store) meetingActionFilters(query MeetingActionsQuery) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if email := strings.ToLower(strings.TrimSpace(query.AssigneeEmail)); email != "" {
		conditions = append(conditions, s.dialect.UnicodeLowerExpression(`a.assignee_email`)+` = ?`)
		args = append(args, email)
	}
	if query.Status != "" {
		conditions = append(conditions, `a.status = ?`)
		args = append(args, string(query.Status))
	}
	if query.Query != "" {
		pattern := "%" + escapeLike(strings.ToLower(query.Query)) + "%"
		conditions = append(conditions, `(`+
			s.dialect.UnicodeLowerExpression(`a.title`)+` LIKE ? ESCAPE '\' OR `+
			s.dialect.UnicodeLowerExpression(`a.description`)+` LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	if len(conditions) == 0 {
		return "", args
	}
	return ` WHERE ` + strings.Join(conditions, ` AND `), args
}

func (s *Store) meetingActionPosition(cursor *meetingActionsCursor) (string, []any) {
	if cursor == nil {
		return "", nil
	}
	messageID, ordinal := *cursor.MessageID, *cursor.Ordinal
	if cursor.OccurredAt == nil {
		return ` AND sm.occurred_key IS NULL AND
			(sm.message_id < ? OR (sm.message_id = ? AND a.ordinal > ?))`,
			[]any{messageID, messageID, ordinal}
	}
	if s.IsPostgreSQL() {
		position := cursor.OccurredAt.UTC()
		return ` AND (sm.occurred_key IS NULL OR sm.occurred_at < ? OR
			(sm.occurred_at = ? AND (sm.message_id < ? OR
				(sm.message_id = ? AND a.ordinal > ?))))`,
			[]any{position, position, messageID, messageID, ordinal}
	}
	position := sqliteutil.TimestampKey(*cursor.OccurredAt)
	return ` AND (sm.occurred_key IS NULL OR sm.occurred_key < ? OR
		(sm.occurred_key = ? AND (sm.message_id < ? OR
			(sm.message_id = ? AND a.ordinal > ?))))`,
		[]any{position, position, messageID, messageID, ordinal}
}

func meetingActionsFilterHash(query MeetingActionsQuery) (string, error) {
	filter := meetingActionsFilter{
		SourceIDs:      normalizedSignedIDs(query.Scope.SourceIDs),
		ParticipantIDs: normalizedSignedIDs(query.Scope.ParticipantIDs),
		Domains:        normalizedMeetingDomains(query.Scope.Domains),
		Deletion:       normalizedMeetingDeletion(query.Scope.Deletion),
		Authority:      query.Scope.Authority,
		AssigneeEmail:  strings.ToLower(strings.TrimSpace(query.AssigneeEmail)),
		Status:         string(query.Status),
		Query:          strings.ToLower(query.Query),
	}
	if query.Scope.MessageIDs != nil {
		values := normalizedSignedIDs(*query.Scope.MessageIDs)
		if values == nil {
			values = []int64{}
		}
		filter.MessageIDs = &values
	}
	if query.Scope.Person != nil {
		person := *query.Scope.Person
		person.ParticipantIDs = normalizedSignedIDs(person.ParticipantIDs)
		person.Directions = slices.Clone(person.Directions)
		slices.Sort(person.Directions)
		person.Directions = slices.Compact(person.Directions)
		filter.Person = &person
	}
	if query.Scope.After != nil {
		filter.After = query.Scope.After.UTC().Format(time.RFC3339Nano)
	}
	if query.Scope.Before != nil {
		filter.Before = query.Scope.Before.UTC().Format(time.RFC3339Nano)
	}
	encoded, err := json.Marshal(filter)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func normalizedMeetingDeletion(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "any"
	}
	return value
}

func encodeMeetingActionsCursor(cursor meetingActionsCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode meeting action cursor: %w", err)
	}
	if len(encoded) > meetingActionsCursorBytes {
		return "", &MeetingCursorError{Reason: "encoded position is too large"}
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeMeetingActionsCursor(value string) (meetingActionsCursor, error) {
	if len(value) > base64.RawURLEncoding.EncodedLen(meetingActionsCursorBytes) {
		return meetingActionsCursor{}, &MeetingCursorError{Reason: "cursor is too large"}
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > meetingActionsCursorBytes {
		return meetingActionsCursor{}, &MeetingCursorError{Reason: "cursor is not valid base64url JSON"}
	}
	var cursor meetingActionsCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return meetingActionsCursor{}, &MeetingCursorError{Reason: "cursor has an invalid shape"}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return meetingActionsCursor{}, &MeetingCursorError{Reason: "cursor has trailing data"}
	}
	if cursor.Version != meetingActionsCursorV1 || !validMeetingFilterHash(cursor.ArchiveUID) ||
		!validMeetingFilterHash(cursor.FilterHash) || cursor.MessageID == nil ||
		cursor.Ordinal == nil || *cursor.Ordinal < 0 ||
		(cursor.OccurredAt != nil && cursor.OccurredAt.IsZero()) {
		return meetingActionsCursor{}, &MeetingCursorError{Reason: "cursor has an invalid position"}
	}
	if cursor.OccurredAt != nil {
		occurredAt := cursor.OccurredAt.UTC()
		cursor.OccurredAt = &occurredAt
	}
	return cursor, nil
}

func validMeetingFilterHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
