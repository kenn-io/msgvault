package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/sourcescope"
)

var (
	ErrMailKindIneligible = errors.New("message is not eligible for mail kind classification")
	ErrMailKindStale      = errors.New("mail kind message evidence changed")
)

const mailKindLabelPrefix = "mv:kind/"

const mailKindScanStatePrefix = "mail_kind_scan:"

type mailKindScanState struct {
	Version         int   `json:"version"`
	InitialComplete bool  `json:"initial_complete"`
	InitialStarted  bool  `json:"initial_started"`
	HighWatermark   int64 `json:"high_watermark"`
	InitialCursor   int64 `json:"initial_cursor"`
}

type mailKindQuerier interface {
	contextStatementQuerier
	QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error)
}

// ListMailKindCandidatesContext reads metadata only. Body hydration is always
// a separate primary-key read, bounded to one explicit batch.
func (s *Store) ListMailKindCandidatesContext(ctx context.Context, limit int, ids []int64) ([]int64, error) {
	if limit < 1 || limit > 100 || len(ids) > limit {
		return nil, errors.New("mail kind batch must contain between 1 and 100 messages")
	}
	for _, id := range ids {
		if id < 1 {
			return nil, errors.New("mail kind message IDs must be positive")
		}
	}
	scope, args := sourceScopeClause(ctx, "m.source_id")
	query := `SELECT m.id FROM messages m WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND COALESCE(m.message_type, '') IN ('', 'email') AND COALESCE(m.is_from_me, FALSE) = FALSE` + scope + `
		AND NOT EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = m.id AND l.source_id IS NULL AND l.label_type = 'auto' AND l.name LIKE 'mv:kind/%')`
	if len(ids) > 0 {
		ids = slices.Clone(ids)
		slices.Sort(ids)
		ids = slices.Compact(ids)
		query += ` AND m.id IN (` + strings.TrimRight(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

// NextMailKindCandidatesContext advances a bounded automatic scan. It walks
// the archive's existing candidate IDs once, newest first, then only considers
// IDs created after that initial high-water mark. Unchanged undecided messages
// therefore do not trigger another provider request on each invocation.
func (s *Store) NextMailKindCandidatesContext(ctx context.Context, fingerprint string, limit int) ([]int64, error) {
	if limit < 1 || limit > 100 || len(fingerprint) != 64 {
		return nil, errors.New("mail kind automatic batch is invalid")
	}

	key, err := mailKindScanStateKey(ctx, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("build mail kind scan state key: %w", err)
	}
	var result []int64
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '')`), key,
		); err != nil {
			return fmt.Errorf("seed mail kind scan state: %w", err)
		}

		// Lock this scope's cursor before selecting a page so concurrent runs
		// cannot repeatedly send the same automatic candidates to the provider.
		var encoded string
		if err := tx.QueryRowContext(ctx,
			`UPDATE archive_metadata SET value = value WHERE key = ? RETURNING value`, key,
		).Scan(&encoded); err != nil {
			return fmt.Errorf("lock mail kind scan state: %w", err)
		}

		var state mailKindScanState
		if encoded == "" || json.Unmarshal([]byte(encoded), &state) != nil ||
			state.Version != 2 || state.HighWatermark < 0 || state.InitialCursor < 0 ||
			(state.InitialComplete && state.InitialCursor != 0) ||
			(!state.InitialComplete && state.InitialCursor > state.HighWatermark) {
			scope, args := sourceScopeClause(ctx, "m.source_id")
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(m.id), 0) FROM messages m WHERE 1=1`+scope, args...).Scan(&state.HighWatermark); err != nil {
				return fmt.Errorf("read mail kind scan high-water mark: %w", err)
			}
			state = mailKindScanState{Version: 2, HighWatermark: state.HighWatermark, InitialCursor: state.HighWatermark}
		}

		if !state.InitialComplete {
			cursorClause := "m.id <= ?"
			if state.InitialStarted {
				cursorClause = "m.id < ?"
			}
			result, err = s.mailKindCandidatePageContext(ctx, tx, limit, cursorClause, state.InitialCursor, "DESC")
			if err != nil {
				return err
			}
			if len(result) > 0 {
				state.InitialStarted = true
				state.InitialCursor = result[len(result)-1]
			} else {
				state.InitialComplete = true
				state.InitialCursor = 0
			}
		}

		if state.InitialComplete && len(result) == 0 {
			result, err = s.mailKindCandidatePageContext(ctx, tx, limit, "m.id > ?", state.HighWatermark, "ASC")
			if err != nil {
				return err
			}
			if len(result) > 0 {
				state.HighWatermark = result[len(result)-1]
			}
		}

		stateBytes, err := json.Marshal(state, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("encode mail kind scan state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE archive_metadata SET value = ? WHERE key = ?`, string(stateBytes), key); err != nil {
			return fmt.Errorf("save mail kind scan state: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) mailKindCandidatePageContext(ctx context.Context, q mailKindQuerier, limit int, cursorClause string, cursor int64, order string) ([]int64, error) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append(args, cursor, limit)
	rows, err := q.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND COALESCE(m.message_type, '') IN ('', 'email') AND COALESCE(m.is_from_me, FALSE) = FALSE`+scope+`
		AND NOT EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = m.id AND l.source_id IS NULL AND l.label_type = 'auto' AND l.name LIKE 'mv:kind/%')
		AND `+cursorClause+` ORDER BY m.id `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func mailKindScanStateKey(ctx context.Context, fingerprint string) (string, error) {
	sourceIDs := sourcescope.IDs(ctx)
	if sourceIDs != nil {
		sourceIDs = slices.Clone(sourceIDs)
		slices.Sort(sourceIDs)
		sourceIDs = slices.Compact(sourceIDs)
	}
	scope, err := json.Marshal(sourceIDs, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(append([]byte(fingerprint), 0), scope...))
	return mailKindScanStatePrefix + hex.EncodeToString(sum[:]), nil
}

// MailKindInputContext returns local evidence plus an opaque snapshot token.
// The token is checked again under the message write lock before labeling.
func (s *Store) MailKindInputContext(ctx context.Context, id int64) (mailkind.Input, string, error) {
	return s.mailKindInputWith(ctx, s.db, id)
}

func (s *Store) mailKindInputWith(ctx context.Context, q mailKindQuerier, id int64) (mailkind.Input, string, error) {
	var in mailkind.Input
	var sourceID int64
	var sourceMessageID string
	var sentAt nullableTimestamp
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	err := q.QueryRowContext(ctx, `SELECT m.source_id, COALESCE(m.source_message_id, ''),
		COALESCE(m.message_type, ''), COALESCE(m.subject, ''), COALESCE(m.snippet, ''),
		COALESCE(m.list_id, ''), COALESCE(m.is_from_me, FALSE),
		COALESCE(m.sent_at, m.received_at, m.internal_date), COALESCE(p.email_address, ''), COALESCE(p.phone_number, '')
		FROM messages m LEFT JOIN participants p ON p.id = COALESCE(m.sender_id,
		(SELECT mr.participant_id FROM message_recipients mr WHERE mr.message_id = m.id AND mr.recipient_type = 'from' ORDER BY mr.id LIMIT 1))
		WHERE m.id = ? AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`+scope,
		append([]any{id}, scopeArgs...)...).Scan(&sourceID, &sourceMessageID, &in.MessageType, &in.Subject, &in.Snippet,
		&in.ListID, &in.IsFromMe, &sentAt, &in.SenderEmail, &in.SenderPhone)
	if errors.Is(err, sql.ErrNoRows) {
		return in, "", ErrMessageNotFound
	}
	if err != nil {
		return in, "", err
	}
	if sentAt.Valid {
		in.SentAt = sentAt.Time
	}
	var textLength, htmlLength int64
	err = q.QueryRowContext(ctx, `SELECT COALESCE(LENGTH(body_text), 0), COALESCE(LENGTH(body_html), 0),
		CASE WHEN LENGTH(body_text) <= 1048576 THEN COALESCE(body_text, '') ELSE '' END,
		CASE WHEN LENGTH(body_html) <= 1048576 THEN COALESCE(body_html, '') ELSE '' END
		FROM message_bodies WHERE message_id = ?`, id).Scan(&textLength, &htmlLength, &in.BodyText, &in.BodyHTML)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return in, "", err
	}
	if textLength > 1<<20 || htmlLength > 1<<20 {
		return in, "", ErrMailKindIneligible
	}
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(mime_type, '') FROM attachments WHERE message_id = ? ORDER BY id LIMIT 1001`, id)
	if err != nil {
		return in, "", err
	}
	defer func() { _ = rows.Close() }()
	in.AttachmentMIMETypes = []string{}
	for rows.Next() {
		var mediaType string
		if err := rows.Scan(&mediaType); err != nil {
			return in, "", err
		}
		in.AttachmentMIMETypes = append(in.AttachmentMIMETypes, mediaType)
	}
	if err := rows.Err(); err != nil {
		return in, "", err
	}
	encoded, err := json.Marshal(struct {
		Input               mailkind.Input
		SourceID, MessageID int64
		SourceMessageID     string
	}{in, sourceID, id, sourceMessageID}, json.Deterministic(true))
	if err != nil {
		return in, "", err
	}
	sum := sha256.Sum256(encoded)
	return in, hex.EncodeToString(sum[:]), nil
}

// ApplyMailKindLabelContext changes local labels only. It rechecks consent,
// message scope, current evidence and deterministic coverage before writing.
func (s *Store) ApplyMailKindLabelContext(ctx context.Context, fingerprint string, id int64, token, kind string, mutationGate func(context.Context) (func(), error)) (bool, error) {
	if !mailkind.KindAllowed(kind) || len(token) != 64 {
		return false, errors.New("invalid mail kind decision")
	}
	mu := &s.withoutSyncScope().mailKindConsentMu
	mu.RLock()
	defer mu.RUnlock()
	active, err := s.HasMailKindConsentContext(ctx, fingerprint)
	if err != nil {
		return false, err
	}
	if !active {
		return false, mailkind.ErrConsentRevoked
	}
	if mutationGate != nil {
		release, err := mutationGate(ctx)
		if err != nil {
			return false, err
		}
		defer release()
	}
	changed := false
	err = s.withMessageAttributionTxContext(ctx, id, func(tx *loggedTx) error {
		if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
			return err
		}
		in, current, err := s.mailKindInputWith(ctx, tx, id)
		if err != nil {
			return err
		}
		if current != token {
			return ErrMailKindStale
		}
		if _, eligible := mailkind.BuildPacket(in); !eligible {
			return ErrMailKindIneligible
		}
		// SQLite already reserves the writer. PostgreSQL needs a global
		// transaction lock because NULL-source label names have no unique key.
		if s.dialect.DriverName() == postgresDriverName {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('msgvault.mail-kind.labels', 0))`); err != nil {
				return err
			}
		}
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
			WHERE ml.message_id = ? AND l.source_id IS NULL AND l.label_type = 'auto' AND l.name LIKE 'mv:kind/%' LIMIT 1`, id).Scan(&exists)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var labelID int64
		name := mailKindLabelPrefix + kind
		err = tx.QueryRowContext(ctx, `SELECT id FROM labels WHERE source_id IS NULL AND label_type = 'auto' AND name = ? ORDER BY id LIMIT 1`, name).Scan(&labelID)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `INSERT INTO labels (source_id, name, label_type) VALUES (NULL, ?, 'auto') RETURNING id`, name).Scan(&labelID)
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO message_labels (message_id, label_id) VALUES (?, ?) ON CONFLICT (message_id, label_id) DO NOTHING`, id, labelID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = count > 0
		return s.bumpDerivedDataRevision(tx, changed)
	})
	return changed, err
}

// MailKindLabelContext accepts only a local auto label from the closed enum.
// A provider tag or a user label with a similar name is not model evidence.
func (s *Store) MailKindLabelContext(ctx context.Context, id int64) (string, error) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	rows, err := s.db.QueryContext(ctx, `SELECT l.name FROM messages m
		JOIN message_labels ml ON ml.message_id = m.id JOIN labels l ON l.id = ml.label_id
		WHERE m.id = ? AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND COALESCE(m.message_type, '') IN ('', 'email')
		AND l.source_id IS NULL AND l.label_type = 'auto' AND l.name LIKE 'mv:kind/%'`+scope,
		append([]any{id}, args...)...)
	if err != nil {
		return "", fmt.Errorf("read local mail kind: %w", err)
	}
	defer func() { _ = rows.Close() }()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		names = append(names, name)
	}
	return mailkind.KindFromLocalLabelNames(names), rows.Err()
}
