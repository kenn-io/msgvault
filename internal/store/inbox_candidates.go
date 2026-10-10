package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxCandidateCursor struct {
	Version    int                         `json:"version"`
	Source     inboxcontrol.SourceIdentity `json:"source"`
	Scope      inboxcontrol.Scope          `json:"scope"`
	Revision   string                      `json:"revision"`
	ObservedAt int64                       `json:"observed_at"`
	ItemID     int64                       `json:"item_id"`
	TargetKey  string                      `json:"target_key"`
}

// InboxCandidates reads one repeatable database snapshot. The cursor is an
// opaque pagination position, never an authorization credential. API admission
// and exact Store source binding both apply independently of cursor contents.
func (s *Store) InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error) {
	if source.Validate() != nil || limit < 1 || limit > 100 || len(cursor) > 16384 {
		return nil, inboxcontrol.ErrInvalid
	}
	if (source.SourceType == "beeper" && scope != inboxcontrol.ScopeChat) || (source.SourceType != "beeper" && scope != inboxcontrol.ScopeMessage) {
		return nil, inboxcontrol.ErrInvalid
	}
	var position inboxCandidateCursor
	if cursor != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(encoded, &position, json.RejectUnknownMembers(true)) != nil || position.Version != 1 || position.Source.Validate() != nil || position.ObservedAt <= 0 || position.ItemID <= 0 || len(position.Revision) != 64 || len(position.TargetKey) != 64 {
			return nil, inboxcontrol.ErrInvalid
		}
		if position.Source != source || position.Scope != scope {
			return nil, inboxcontrol.ErrConflict
		}
	}
	page := &inboxcontrol.CandidatePage{ProviderIngestion: inboxcontrol.UnknownProviderIngestion(), Source: source, Scope: scope, Candidates: []inboxcontrol.Candidate{}}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxCandidateSource(ctx, tx, source); err != nil {
			return err
		}
		revision, unknown, err := inboxCandidateRevision(ctx, tx, source, scope)
		if err != nil {
			return err
		}
		page.ArchiveRevision, page.Unavailable = revision, unknown
		if cursor != "" && position.Revision != revision {
			return inboxcontrol.ErrPlanChanged
		}
		table, providerID, title, snippet, active := inboxCandidateRecordSQL(scope)
		contextMessage := "0"
		if scope == inboxcontrol.ScopeChat {
			contextMessage = `COALESCE((SELECT m.id FROM messages m
   WHERE m.source_id=p.source_id AND m.conversation_id=p.item_id
   AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
   ORDER BY COALESCE(m.sent_at,m.received_at,m.internal_date) DESC NULLS LAST,m.id DESC LIMIT 1),0)`
		}
		query := fmt.Sprintf(`SELECT p.state_json,p.observed_at,p.item_id,p.target_key,COALESCE(r.%s,''),COALESCE(r.%s,''),%s
   FROM inbox_provider_states p JOIN %s r ON r.id=p.item_id AND r.source_id=p.source_id AND r.%s=p.provider_id
   WHERE p.source_id=? AND p.scope=? AND p.is_inbox=TRUE AND %s
   AND %s`, title, snippet, contextMessage, table, providerID, active, inboxCandidateMembershipSQL(source))
		args := []any{source.SourceID, scope}
		if cursor != "" {
			query += ` AND (p.observed_at<? OR (p.observed_at=? AND p.item_id<?) OR (p.observed_at=? AND p.item_id=? AND p.target_key<?))`
			args = append(args, position.ObservedAt, position.ObservedAt, position.ItemID, position.ObservedAt, position.ItemID, position.TargetKey)
		}
		query += ` ORDER BY p.observed_at DESC,p.item_id DESC,p.target_key DESC LIMIT ?`
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("query inbox candidates: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var last inboxCandidateCursor
		for rows.Next() {
			if len(page.Candidates) == limit {
				encoded, err := json.Marshal(last)
				if err != nil {
					return fmt.Errorf("encode inbox candidate cursor: %w", err)
				}
				page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
				break
			}
			var candidate inboxcontrol.Candidate
			var encoded string
			var observed, itemID int64
			var key string
			if err := rows.Scan(&encoded, &observed, &itemID, &key, &candidate.Title, &candidate.Snippet, &candidate.ContextMessageID); err != nil {
				return fmt.Errorf("scan inbox candidate: %w", err)
			}
			if json.Unmarshal([]byte(encoded), &candidate.State) != nil {
				return inboxcontrol.ErrUnavailable
			}
			target := candidate.State.Target
			if target.Validate() != nil || target.SourceID != source.SourceID || target.SourceType != source.SourceType || target.SourceIdentifier != source.SourceIdentifier || target.AccountID != source.AccountID || target.Scope != scope || target.ItemID != itemID || candidate.State.Inbox == nil || !*candidate.State.Inbox {
				return inboxcontrol.ErrUnavailable
			}
			candidate.Available = candidate.State.Read != nil && (source.SourceType != "beeper" || candidate.State.MarkedUnread != nil)
			if !candidate.Available {
				page.Unavailable = true
			}
			page.Candidates = append(page.Candidates, candidate)
			last = inboxCandidateCursor{Version: 1, Source: source, Scope: scope, Revision: revision, ObservedAt: observed, ItemID: itemID, TargetKey: key}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func validateInboxCandidateSource(ctx context.Context, tx *loggedTx, source inboxcontrol.SourceIdentity) error {
	var kind, identifier string
	err := tx.QueryRowContext(ctx, `SELECT source_type,identifier FROM sources WHERE id=?`, source.SourceID).Scan(&kind, &identifier)
	if err == sql.ErrNoRows {
		return inboxcontrol.ErrDenied
	}
	if err != nil {
		return fmt.Errorf("resolve inbox candidate source: %w", err)
	}
	if kind == "" {
		kind = sourceTypeGmail
	}
	account := identifier
	if kind == sourceTypeIMAP {
		parsed, err := url.Parse(identifier)
		if err != nil || parsed.User == nil || parsed.User.Username() == "" {
			return inboxcontrol.ErrUnavailable
		}
		account = parsed.User.Username()
	}
	if kind != source.SourceType || identifier != source.SourceIdentifier || account != source.AccountID {
		return inboxcontrol.ErrDenied
	}
	return nil
}

func inboxCandidateRecordSQL(scope inboxcontrol.Scope) (table, providerID, title, snippet, active string) {
	if scope == inboxcontrol.ScopeChat {
		return "conversations", "source_conversation_id", "title", "last_message_preview", "TRUE"
	}
	return "messages", "source_message_id", "subject", "snippet", "r.deleted_at IS NULL AND r.deleted_from_source_at IS NULL"
}
func inboxCandidateMembershipSQL(source inboxcontrol.SourceIdentity) string {
	if source.SourceType != sourceTypeIMAP {
		return "TRUE"
	}
	return `EXISTS (SELECT 1 FROM imap_message_memberships member WHERE member.source_id=p.source_id
  AND member.message_id=p.item_id AND member.mailbox=p.mailbox AND member.uidvalidity=p.uidvalidity AND member.uid=p.uid)`
}

func inboxCandidateRevision(ctx context.Context, tx *loggedTx, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope) (string, bool, error) {
	table, providerID, _, _, active := inboxCandidateRecordSQL(scope)
	var generation, total, missing, unknown, messages, maxMessage int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM inbox_source_revisions WHERE source_id=?),0)`, source.SourceID).Scan(&generation); err != nil {
		return "", false, fmt.Errorf("read inbox archive revision: %w", err)
	}
	query := fmt.Sprintf(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN NOT EXISTS (SELECT 1 FROM inbox_provider_states p
  WHERE p.source_id=r.source_id AND p.scope=? AND p.item_id=r.id AND p.provider_id=r.%s AND p.is_inbox IS NOT NULL AND %s)
  THEN 1 ELSE 0 END),0) FROM %s r WHERE r.source_id=? AND %s`, providerID, inboxCandidateMembershipSQL(source), table, active)
	if err := tx.QueryRowContext(ctx, query, scope, source.SourceID).Scan(&total, &missing); err != nil {
		return "", false, fmt.Errorf("read inbox observation coverage: %w", err)
	}
	unknownMarkers := "p.provider_read IS NULL"
	if source.SourceType == "beeper" {
		unknownMarkers += " OR p.marked_unread IS NULL"
	}
	unknownQuery := fmt.Sprintf(`SELECT COUNT(*) FROM inbox_provider_states p JOIN %s r
  ON r.id=p.item_id AND r.source_id=p.source_id AND r.%s=p.provider_id
  WHERE p.source_id=? AND p.scope=? AND %s AND %s
  AND (p.is_inbox IS NULL OR (p.is_inbox=TRUE AND (%s)))`, table, providerID, active, inboxCandidateMembershipSQL(source), unknownMarkers)
	if err := tx.QueryRowContext(ctx, unknownQuery, source.SourceID, scope).Scan(&unknown); err != nil {
		return "", false, fmt.Errorf("read unknown inbox observations: %w", err)
	}
	// Incoming messages in an existing chat also invalidate pagination, even if
	// the chat observation has not been refreshed by the importer yet.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),0) FROM messages WHERE source_id=? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, source.SourceID).Scan(&messages, &maxMessage); err != nil {
		return "", false, fmt.Errorf("read inbox incoming watermark: %w", err)
	}
	encoded, err := json.Marshal(struct {
		Source                                                    inboxcontrol.SourceIdentity
		Scope                                                     inboxcontrol.Scope
		Generation, Total, Missing, Unknown, Messages, MaxMessage int64
	}{source, scope, generation, total, missing, unknown, messages, maxMessage})
	if err != nil {
		return "", false, fmt.Errorf("encode inbox archive revision: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), missing > 0 || unknown > 0, nil
}

func bumpInboxSourceRevisionTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO inbox_source_revisions(source_id,revision) VALUES (?,1)
  ON CONFLICT(source_id) DO UPDATE SET revision=inbox_source_revisions.revision+1`, sourceID)
	if err != nil {
		return fmt.Errorf("advance inbox archive revision: %w", err)
	}
	return nil
}
