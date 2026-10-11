package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxContext reads one authorized archive identity and its bounded text in
// the same snapshot. It neither observes providers nor modifies read state.
func (s *Store) InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	limit := request.MaxBytes
	if limit == 0 {
		limit = inboxcontrol.DefaultContextBytes
	}
	messageID := request.Target.ItemID
	if request.Target.Scope == inboxcontrol.ScopeChat {
		messageID = request.MessageID
	}
	result := &inboxcontrol.Context{Target: request.Target, MessageID: messageID}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		target := request.Target
		source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
		if err := validateInboxCandidateSource(ctx, tx, source); err != nil {
			return err
		}
		if err := validateInboxArchiveIdentity(ctx, tx, target); err != nil {
			return err
		}
		var count int
		query := `SELECT COUNT(*) FROM messages WHERE id=? AND source_id=? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`
		args := []any{messageID, target.SourceID}
		if target.Scope == inboxcontrol.ScopeChat {
			query += ` AND conversation_id=?`
			args = append(args, target.ItemID)
		}
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return fmt.Errorf("resolve inbox context message: %w", err)
		}
		if count != 1 {
			return inboxcontrol.ErrDenied
		}
		if target.SourceType == "imap" {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_message_memberships member
				WHERE member.source_id=? AND member.message_id=? AND member.mailbox=? AND member.uidvalidity=? AND member.uid=?
				AND EXISTS (SELECT 1 FROM imap_folder_state folder WHERE folder.source_id=member.source_id AND folder.mailbox=member.mailbox AND folder.uidvalidity=member.uidvalidity)`,
				target.SourceID, target.ItemID, target.Mailbox, target.UIDValidity, target.UID).Scan(&count); err != nil {
				return fmt.Errorf("resolve inbox context membership: %w", err)
			}
			if count != 1 {
				return inboxcontrol.ErrInvalid
			}
		}
		// SQLite text substr stops at NUL; slicing the blob preserves exact bytes.
		// PostgreSQL converts only a bounded character prefix before byte slicing.
		query = `SELECT CASE WHEN body_text IS NULL THEN NULL WHEN body_text='' THEN '' ELSE substr(CAST(body_text AS BLOB),1,?) END FROM message_bodies WHERE message_id=?`
		args = []any{limit + 1, messageID}
		if s.dialect.DriverName() == postgresDriverName {
			query = `SELECT substring(convert_to(substr(body_text,1,?), 'UTF8') FROM 1 FOR ?) FROM message_bodies WHERE message_id=?`
			args = []any{limit + 1, limit + 1, messageID}
		}
		var text sql.NullString
		err := tx.QueryRowContext(ctx, query, args...).Scan(&text)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !text.Valid) {
			result.Unavailable = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read inbox context body: %w", err)
		}
		result.Text = text.String
		if len(result.Text) > limit {
			result.Truncated = true
			end := limit
			for end > 0 && !utf8.RuneStart(result.Text[end]) {
				end--
			}
			result.Text = result.Text[:end]
		}
		if !utf8.ValidString(result.Text) {
			return inboxcontrol.ErrUnavailable
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
