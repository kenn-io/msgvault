package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/filemeta"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/search"
)

const filenamePersonBudget = 64
const filenameProvenanceBudget = 4096

var (
	ErrFilenameScopeTooLarge  = errors.New("filename discovery person scope is too large")
	ErrFilenameRequestInvalid = errors.New("invalid filename discovery request")
)

// FilenameSuggestionScope intersects every supplied authority constraint.
// Arrays are bounded before deduplication; person identities are never changed.
type FilenameSuggestionScope struct {
	SourceIDs     []int64
	AccountScopes []search.AccountScope
	After, Before *time.Time
	MIMEFamilies  []string
	Person        *personscope.Scope
}

// FilenameSuggestionRow contains current authority, not projection metadata.
type FilenameSuggestionRow struct {
	File             FileMetadata
	OccurredAt       *time.Time
	MIMEFamily       string
	PersonProvenance *personscope.Provenance
}

// FilenamePersonContext resolves only identity and bounded bindings. It honors
// the caller's snapshot and never hydrates CardDAV or begins a write transaction.
func (s *Store) FilenamePersonContext(ctx context.Context, id int64) (*Person, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrPersonNotFound
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	db := ReadDBContext(ctx, s.db.DB)
	var person Person
	var display sql.NullString
	err = db.QueryRowContext(ctx, s.dialect.Rebind(`SELECT id,vcard_uid,display_name,revision,created_at,updated_at FROM persons WHERE id=?`), id).Scan(&person.ID, &person.VCardUID, &display, &person.Revision, &person.CreatedAt, &person.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPersonNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read filename person: %w", err)
	}
	if display.Valid {
		person.DisplayName = &display.String
	}
	person.ParticipantIDs = []int64{}
	rows, err := db.QueryContext(ctx, s.dialect.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=? ORDER BY participant_id LIMIT 65`), id)
	if err != nil {
		return nil, fmt.Errorf("read filename person bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var participant int64
		if err := rows.Scan(&participant); err != nil {
			return nil, fmt.Errorf("scan filename person binding: %w", err)
		}
		person.ParticipantIDs = append(person.ParticipantIDs, participant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filename person bindings: %w", err)
	}
	if len(person.ParticipantIDs) > filenamePersonBudget {
		return nil, ErrFilenameScopeTooLarge
	}
	return &person, nil
}

// ValidateFilenameSuggestionScope checks request constraints without SQL.
func ValidateFilenameSuggestionScope(scope FilenameSuggestionScope) error {
	return validateFilenameSuggestionScope(nil, scope)
}

func validateFilenameSuggestionScope(ids []int64, scope FilenameSuggestionScope) error {
	if len(ids) > filenameCandidateBudget || len(scope.SourceIDs) > 64 || len(scope.MIMEFamilies) > 8 {
		return ErrFilenameRequestInvalid
	}
	for _, group := range [][]int64{ids, scope.SourceIDs} {
		for _, id := range group {
			if id <= 0 {
				return ErrFilenameRequestInvalid
			}
		}
	}
	for _, family := range scope.MIMEFamilies {
		switch family {
		case "image", "pdf", "audio", "video", "text", "document", "archive", "other":
		default:
			return ErrFilenameRequestInvalid
		}
	}
	if err := search.ValidateAccountScopes(scope.AccountScopes); err != nil {
		return fmt.Errorf("%w: %w", ErrFilenameRequestInvalid, err)
	}
	if scope.After != nil && scope.Before != nil && !scope.After.Before(*scope.Before) {
		return fmt.Errorf("%w: after must precede before", ErrFilenameRequestInvalid)
	}
	if scope.Person != nil {
		if len(scope.Person.ParticipantIDs) > filenamePersonBudget {
			return ErrFilenameScopeTooLarge
		}
		if err := personscope.Validate(*scope.Person); err != nil {
			return fmt.Errorf("%w: %w", ErrFilenameRequestInvalid, err)
		}
	}
	return nil
}

// FilenameSuggestionRowsContext rechecks at most 500 candidate attachment IDs
// against one archive snapshot, then admits all or none of bounded provenance.
func (s *Store) FilenameSuggestionRowsContext(ctx context.Context, ids []int64, scope FilenameSuggestionScope) ([]FilenameSuggestionRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFilenameSuggestionScope(ids, scope); err != nil {
		return nil, err
	}
	if len(ids) == 0 || scope.Person != nil && len(scope.Person.ParticipantIDs) == 0 {
		return []FilenameSuggestionRow{}, nil
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	scope = normalizeFilenameSuggestionScope(scope)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	conditions, scopeArgs := s.filenameScopeConditions(scope)
	args = append(args, scopeArgs...)
	mime := filemeta.MIMEFamilySQL("a.mime_type")
	roots := `FROM attachments a JOIN messages m ON m.id=a.message_id JOIN conversations c ON c.id=m.conversation_id`
	if s.IsPostgreSQL() {
		values := make([]string, len(ids))
		for i := range values {
			values[i] = "(CAST(? AS BIGINT))"
		}
		roots = `FROM (VALUES ` + strings.Join(values, ",") + `) filename_candidates(id)
		JOIN LATERAL (SELECT a.* FROM attachments a WHERE a.id=filename_candidates.id OFFSET 0) a ON TRUE
		` + filenamePostgresMessageRoots
	} else {
		// IDs bind at the PostgreSQL primary-key roots above.
		conditions = append([]string{"a.id IN (" + placeholders(len(ids)) + ")"}, conditions...)
	}
	query := `SELECT a.id,a.message_id,m.conversation_id,m.source_id,COALESCE(m.source_message_id,''),
  COALESCE(m.message_type,''),COALESCE(c.conversation_type,''),COALESCE(a.filename,''),COALESCE(a.mime_type,''),COALESCE(a.size,0),
  COALESCE(a.content_hash,''),COALESCE(a.storage_path,''),a.attachment_role,a.role_source,COALESCE(a.source_part_key,''),COALESCE(a.content_id,''),m.sent_at,` + mime + `
  ` + roots + `
  WHERE ` + strings.Join(conditions, " AND ")
	rows, err := ReadDBContext(ctx, s.db.DB).QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read filename suggestion authority: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []FilenameSuggestionRow{}
	var messageIDs []int64
	for rows.Next() {
		var row FilenameSuggestionRow
		var occurred sql.NullTime
		file := &row.File
		if err := rows.Scan(&file.ID, &file.MessageID, &file.ConversationID, &file.SourceID, &file.SourceMessageID, &file.MessageType, &file.ConversationType, &file.Filename, &file.MimeType, &file.Size, &file.ContentHash, &file.StoragePath, &file.AttachmentRole, &file.RoleSource, &file.SourcePartKey, &file.ContentID, &occurred, &row.MIMEFamily); err != nil {
			return nil, fmt.Errorf("scan filename suggestion authority: %w", err)
		}
		if occurred.Valid {
			row.OccurredAt = &occurred.Time
		}
		normalizeFileMetadataStorage(file)
		result = append(result, row)
		messageIDs = append(messageIDs, file.MessageID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filename suggestion authority: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close filename suggestion authority: %w", err)
	}
	slices.SortFunc(result, func(a, b FilenameSuggestionRow) int {
		if a.File.ID < b.File.ID {
			return -1
		}
		if a.File.ID > b.File.ID {
			return 1
		}
		return 0
	})
	if scope.Person != nil && len(result) > 0 {
		provenance, err := s.filenamePersonProvenanceContext(ctx, messageIDs, *scope.Person)
		if err != nil {
			return nil, err
		}
		for i := range result {
			result[i].PersonProvenance = provenance[result[i].File.MessageID]
			if result[i].PersonProvenance == nil {
				return nil, fmt.Errorf("filename person provenance missing for message %d", result[i].File.MessageID)
			}
		}
	}
	return result, nil
}

// filenamePostgresMessageRoots joins m and c to attachment alias a by primary
// key. OFFSET 0 keeps PostgreSQL from flattening them into archive-wide joins.
const filenamePostgresMessageRoots = `JOIN LATERAL (SELECT id,conversation_id,source_id,source_message_id,message_type,sent_at,account_address,account_path,sender_id,is_from_me,deleted_at FROM messages WHERE id=a.message_id OFFSET 0) m ON TRUE
		JOIN LATERAL (SELECT id,conversation_type FROM conversations WHERE id=m.conversation_id OFFSET 0) c ON TRUE`

// filenameAttachmentScopeExists tests one correlated attachment ID against
// filenameScopeConditions. Both backends start from the attachment primary
// key so a tiny or unanalyzed archive cannot plan message or conversation scans.
func (s *Store) filenameAttachmentScopeExists(attachmentID string, conditions []string) string {
	roots := `attachments a CROSS JOIN messages m CROSS JOIN conversations c
		WHERE a.id=` + attachmentID + ` AND m.id=a.message_id AND c.id=m.conversation_id`
	if s.IsPostgreSQL() {
		roots = `attachments a ` + filenamePostgresMessageRoots + `
		WHERE a.id=` + attachmentID
	}
	return `EXISTS (SELECT 1 FROM ` + roots + ` AND ` + strings.Join(conditions, " AND ") + ` LIMIT 1 OFFSET 0)`
}

// normalizeFilenameSuggestionScope sorts and deduplicates person bindings
// without mutating the caller's scope.
func normalizeFilenameSuggestionScope(scope FilenameSuggestionScope) FilenameSuggestionScope {
	if scope.Person != nil {
		person := *scope.Person
		person.ParticipantIDs = slices.Clone(person.ParticipantIDs)
		slices.Sort(person.ParticipantIDs)
		person.ParticipantIDs = slices.Compact(person.ParticipantIDs)
		scope.Person = &person
	}
	return scope
}

// filenameScopeFilters reports whether a request narrows beyond live messages.
func filenameScopeFilters(scope FilenameSuggestionScope) bool {
	return len(scope.SourceIDs) > 0 || len(scope.AccountScopes) > 0 || len(scope.MIMEFamilies) > 0 ||
		scope.After != nil || scope.Before != nil || scope.Person != nil
}

// filenameScopeConditions renders live-message and request constraints over
// aliases a (attachments), m (messages) and c (conversations). Candidate
// admission and authoritative rows share it so the two cannot drift.
func (s *Store) filenameScopeConditions(scope FilenameSuggestionScope) ([]string, []any) {
	conditions := []string{LiveMessagesWhere("m", false)}
	var args []any
	if len(scope.SourceIDs) > 0 {
		conditions = append(conditions, "m.source_id IN ("+placeholders(len(scope.SourceIDs))+")")
		for _, id := range scope.SourceIDs {
			args = append(args, id)
		}
	}
	accountConditions, accountArgs := search.AccountScopeConditions(scope.AccountScopes, "m")
	conditions = append(conditions, accountConditions...)
	args = append(args, accountArgs...)
	dateColumn, dateValue := "m.sent_at", "?"
	if !s.IsPostgreSQL() {
		dateColumn, dateValue = "julianday(m.sent_at)", "julianday(?)"
	}
	if scope.After != nil {
		conditions = append(conditions, dateColumn+" >= "+dateValue)
		args = append(args, scope.After.UTC())
	}
	if scope.Before != nil {
		conditions = append(conditions, dateColumn+" < "+dateValue)
		args = append(args, scope.Before.UTC())
	}
	if len(scope.MIMEFamilies) > 0 {
		conditions = append(conditions, "("+filemeta.MIMEFamilySQL("a.mime_type")+") IN ("+placeholders(len(scope.MIMEFamilies))+")")
		for _, family := range scope.MIMEFamilies {
			args = append(args, family)
		}
	}
	if scope.Person != nil {
		predicate, personArgs := personscope.BoundedMessagePredicate(*scope.Person, "m", "c")
		conditions = append(conditions, "("+predicate+")")
		args = append(args, personArgs...)
	}
	return conditions, args
}

// The finite cross product emits each (message, participant, role) once. EXISTS
// probes keep envelope aliases and roster duplicates out of the work/output cap.
func (s *Store) filenamePersonProvenanceContext(ctx context.Context, messageIDs []int64, scope personscope.Scope) (map[int64]*personscope.Provenance, error) {
	messageIDs = slices.Clone(messageIDs)
	slices.Sort(messageIDs)
	messageIDs = slices.Compact(messageIDs)
	messageValues := make([]string, len(messageIDs))
	personValues := make([]string, len(scope.ParticipantIDs))
	var args []any
	for i, id := range messageIDs {
		messageValues[i] = "(CAST(? AS BIGINT))"
		args = append(args, id)
	}
	for i, id := range scope.ParticipantIDs {
		personValues[i] = "(CAST(? AS BIGINT))"
		args = append(args, id)
	}
	evidence := `(m.is_from_me=TRUE OR m.sender_id IS NOT NULL OR EXISTS(SELECT 1 FROM message_recipients known_from WHERE known_from.message_id=m.id AND LOWER(known_from.recipient_type)='from' LIMIT 1 OFFSET 0))`
	roster := `LOWER(COALESCE(c.conversation_type,'')) IN ('group_chat','channel')`
	if scope.IncludeUnclassifiedRosterRows {
		roster = `(LOWER(COALESCE(c.conversation_type,''))<>'direct_chat' OR NOT ` + evidence + `)`
	}
	recipient := `EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=p.participant_id AND LOWER(mr.recipient_type)=r.role LIMIT 1 OFFSET 0)`
	// PostgreSQL can otherwise turn correlated EXISTS into hashed, whole-table
	// subplans. OFFSET 0 keeps native point probes correlated. Lateral PK roots
	// also prevent flattening the bounded message population into archive joins.
	roots := `FROM selected_messages sm JOIN messages m ON m.id=sm.message_id JOIN conversations c ON c.id=m.conversation_id`
	if s.IsPostgreSQL() {
		roots = `FROM selected_messages sm
		JOIN LATERAL (SELECT id,conversation_id,sender_id,is_from_me FROM messages WHERE id=sm.message_id OFFSET 0) m ON TRUE
		JOIN LATERAL (SELECT id,conversation_type FROM conversations WHERE id=m.conversation_id OFFSET 0) c ON TRUE`
	}
	query := `WITH selected_messages(message_id) AS (VALUES ` + strings.Join(messageValues, ",") + `),
 person_ids(participant_id) AS (VALUES ` + strings.Join(personValues, ",") + `),
 filename_roles(role) AS (VALUES ('from'),('to'),('cc'),('bcc'),('conversation_member')),
 filename_edges AS (
 SELECT m.id AS message_id,p.participant_id,r.role
 ` + roots + `
 CROSS JOIN person_ids p CROSS JOIN filename_roles r
 WHERE (r.role IN ('from','to','cc','bcc') AND ` + recipient + `)
 OR (r.role='from' AND m.sender_id=p.participant_id)
 OR (r.role='to' AND LOWER(COALESCE(c.conversation_type,''))='direct_chat' AND ` + evidence + `
   AND EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id AND cp.participant_id=p.participant_id LIMIT 1 OFFSET 0)
   AND NOT EXISTS(SELECT 1 FROM person_ids sender_person WHERE sender_person.participant_id=m.sender_id OR (
     m.sender_id IS NULL AND EXISTS(SELECT 1 FROM message_recipients mr_from WHERE mr_from.message_id=m.id AND mr_from.participant_id=sender_person.participant_id AND LOWER(mr_from.recipient_type)='from' LIMIT 1 OFFSET 0))))
 OR (r.role='conversation_member' AND ` + roster + ` AND EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id AND cp.participant_id=p.participant_id LIMIT 1 OFFSET 0))
 ) SELECT message_id,participant_id,role FROM filename_edges LIMIT 4097`
	rows, err := ReadDBContext(ctx, s.db.DB).QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read filename person provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type edgeSet map[personscope.Role]bool
	edges := map[int64]map[int64]edgeSet{}
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count++
		if count > filenameProvenanceBudget {
			return nil, ErrFilenameScopeTooLarge
		}
		var message, participant int64
		var role personscope.Role
		if err := rows.Scan(&message, &participant, &role); err != nil {
			return nil, fmt.Errorf("scan filename person provenance: %w", err)
		}
		if edges[message] == nil {
			edges[message] = map[int64]edgeSet{}
		}
		if edges[message][participant] == nil {
			edges[message][participant] = edgeSet{}
		}
		edges[message][participant][role] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filename person provenance: %w", err)
	}
	result := map[int64]*personscope.Provenance{}
	for message, byPerson := range edges {
		provenance := &personscope.Provenance{}
		for participant := range byPerson {
			provenance.ParticipantIDs = append(provenance.ParticipantIDs, participant)
		}
		slices.Sort(provenance.ParticipantIDs)
		for _, role := range []personscope.Role{personscope.RoleFrom, personscope.RoleTo, personscope.RoleCC, personscope.RoleBCC, personscope.RoleConversationMember} {
			for _, participant := range provenance.ParticipantIDs {
				if byPerson[participant][role] {
					provenance.Roles = append(provenance.Roles, role)
					break
				}
			}
		}
		if slices.Contains(provenance.Roles, personscope.RoleFrom) {
			provenance.Directions = append(provenance.Directions, personscope.FromPerson)
		}
		if slices.Contains(provenance.Roles, personscope.RoleTo) || slices.Contains(provenance.Roles, personscope.RoleCC) || slices.Contains(provenance.Roles, personscope.RoleBCC) {
			provenance.Directions = append(provenance.Directions, personscope.ToPerson)
		}
		if slices.Contains(provenance.Roles, personscope.RoleConversationMember) {
			provenance.Directions = append(provenance.Directions, personscope.Group)
		}
		result[message] = provenance
	}
	return result, nil
}
