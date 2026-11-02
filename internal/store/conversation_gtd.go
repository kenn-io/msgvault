package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
)

// ErrConversationGTDRevision rejects a decision based on superseded user state.
var ErrConversationGTDRevision = errors.New("conversation classification changed")

// ConversationGTD is msgvault user state, independent of native labels.
// Revision zero describes an existing archived conversation not yet classified.
type ConversationGTD struct {
	ConversationID   int64          `json:"conversation_id"`
	SourceID         int64          `json:"source_id"`
	SourceIdentifier string         `json:"source_identifier"`
	ChatID           string         `json:"chat_id"`
	Title            string         `json:"title"`
	Category         string         `json:"category"`
	NeedsAnswer      bool           `json:"needs_answer"`
	Revision         int64          `json:"revision"`
	ObservedActivity string         `json:"observed_activity_at"`
	ContextStamp     string         `json:"reviewed_context_stamp"`
	ReviewedHead     jsontext.Value `json:"reviewed_head,omitzero"`
	ArchiveHold      bool           `json:"archive_hold"`
}

// ConversationGTDChange replaces a local decision, optionally guarded by its
// current revision. Clear resets the decision but never acknowledges a hold.
type ConversationGTDChange struct {
	Category         string
	NeedsAnswer      *bool
	Clear            bool
	ExpectedRevision *int64
	ObservedActivity string
	ContextStamp     string
	ReviewedHead     jsontext.Value
}

const conversationGTDSelect = `SELECT c.id,c.source_id,s.identifier,COALESCE(c.source_conversation_id,''),COALESCE(c.title,''),
 COALESCE(g.category,''),COALESCE(g.needs_answer,FALSE),COALESCE(g.revision,0),
 COALESCE(g.observed_activity_at,''),COALESCE(g.reviewed_context_stamp,''),COALESCE(g.reviewed_head,''),COALESCE(g.archive_hold,FALSE)
 FROM conversations c JOIN sources s ON s.id=c.source_id LEFT JOIN conversation_gtd g ON g.conversation_id=c.id
 WHERE s.source_type='beeper'`

func scanConversationGTD(row scanner) (ConversationGTD, error) {
	var g ConversationGTD
	var head string
	err := row.Scan(&g.ConversationID, &g.SourceID, &g.SourceIdentifier, &g.ChatID, &g.Title, &g.Category, &g.NeedsAnswer, &g.Revision, &g.ObservedActivity, &g.ContextStamp, &head, &g.ArchiveHold)
	if head != "" {
		g.ReviewedHead = jsontext.Value(head)
	}
	return g, err
}

// GetConversationGTDContext includes archived Beeper identities without a decision.
func (s *Store) GetConversationGTDContext(ctx context.Context, id int64) (ConversationGTD, error) {
	scope, args := sourceScopeClause(ctx, "c.source_id")
	g, err := scanConversationGTD(s.db.QueryRowContext(ctx, s.Rebind(conversationGTDSelect+` AND c.id=?`+scope), append([]any{id}, args...)...))
	if err != nil {
		return g, fmt.Errorf("load Beeper classification: %w", err)
	}
	return g, nil
}

// ListConversationGTDContext pages local identities independently of native Inbox.
func (s *Store) ListConversationGTDContext(ctx context.Context, sourceID, after int64, limit int, needsAnswer bool) ([]ConversationGTD, error) {
	if limit < 1 || limit > 201 || after < 0 {
		return nil, errors.New("invalid conversation page")
	}
	scope, args := sourceScopeClause(ctx, "c.source_id")
	query := conversationGTDSelect + ` AND c.source_id=? AND c.id>?` + scope
	if needsAnswer {
		query += ` AND g.needs_answer=TRUE`
	}
	query += ` ORDER BY c.id LIMIT ?`
	values := append([]any{sourceID, after}, args...)
	values = append(values, limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), values...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []ConversationGTD{}
	for rows.Next() {
		g, err := scanConversationGTD(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

// BeeperConversationCeilingContext bounds one traversal of local identities.
func (s *Store) BeeperConversationCeilingContext(ctx context.Context, sourceID int64) (int64, error) {
	scope, args := sourceScopeClause(ctx, "c.source_id")
	var ceiling int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(MAX(c.id),0) FROM conversations c JOIN sources s ON s.id=c.source_id WHERE s.source_type='beeper' AND c.source_id=?`+scope), append([]any{sourceID}, args...)...).Scan(&ceiling)
	return ceiling, err
}

// SetConversationGTDContext changes only msgvault user state, never a provider.
func (s *Store) SetConversationGTDContext(ctx context.Context, id int64, change ConversationGTDChange) (ConversationGTD, error) {
	if !change.Clear && change.Category != "todo" && change.Category != "waiting" && change.Category != "delegated" && change.Category != "reference" {
		return ConversationGTD{}, errors.New("invalid GTD category")
	}
	if change.ObservedActivity != "" {
		if _, err := time.Parse(time.RFC3339Nano, change.ObservedActivity); err != nil {
			return ConversationGTD{}, errors.New("invalid observed activity")
		}
	}
	if len(change.ReviewedHead) > 0 && !change.ReviewedHead.IsValid() {
		return ConversationGTD{}, errors.New("invalid reviewed head")
	}
	var result ConversationGTD
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		scope, args := sourceScopeClause(ctx, "c.source_id")
		current, err := scanConversationGTD(tx.QueryRowContext(ctx, s.Rebind(conversationGTDSelect+` AND c.id=?`+scope), append([]any{id}, args...)...))
		if err != nil {
			return fmt.Errorf("resolve GTD conversation: %w", err)
		}
		if err := s.requireSyncSource(current.SourceID); err != nil {
			return err
		}
		if change.ExpectedRevision != nil && current.Revision != *change.ExpectedRevision {
			return ErrConversationGTDRevision
		}
		if change.Clear && current.Revision == 0 {
			result = current
			return nil
		}
		category := change.Category
		answer := category == "todo"
		if current.Category != "" {
			answer = current.NeedsAnswer
		}
		observed, stamp, head := change.ObservedActivity, change.ContextStamp, string(change.ReviewedHead)
		if change.NeedsAnswer != nil {
			answer = *change.NeedsAnswer
		}
		if change.Clear {
			category = ""
			answer = false
			observed = ""
			stamp = ""
			head = ""
		}
		// The conflict WHERE is the optimistic user-state guard on both backends.
		var revision int64
		err = tx.QueryRowContext(ctx, s.Rebind(`INSERT INTO conversation_gtd(conversation_id,category,needs_answer,observed_activity_at,reviewed_context_stamp,reviewed_head,revision,archive_hold)
   VALUES(?,NULLIF(?,''),?,NULLIF(?,''),NULLIF(?,''),NULLIF(?,''),1,FALSE)
   ON CONFLICT(conversation_id) DO UPDATE SET category=excluded.category,needs_answer=excluded.needs_answer,
   observed_activity_at=excluded.observed_activity_at,reviewed_context_stamp=excluded.reviewed_context_stamp,reviewed_head=excluded.reviewed_head,
   revision=conversation_gtd.revision+1,updated_at=CURRENT_TIMESTAMP WHERE conversation_gtd.revision=? RETURNING revision`), id, category, answer, observed, stamp, head, current.Revision).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConversationGTDRevision
		}
		if err != nil {
			return err
		}
		result = current
		result.Category = category
		result.NeedsAnswer = answer
		result.ObservedActivity = observed
		result.ContextStamp = stamp
		result.ReviewedHead = nil
		if head != "" {
			result.ReviewedHead = jsontext.Value(head)
		}
		result.Revision = revision
		return nil
	})
	return result, err
}

// SetConversationGTDHoldContext changes only current uncertainty, never review
// metadata or revision. Classification/reset must not acknowledge this hold.
func (s *Store) SetConversationGTDHoldContext(ctx context.Context, id, revision int64, hold bool) error {
	scope, args := sourceScopeClause(ctx, "c.source_id")
	var changed int64
	values := append([]any{hold, id, revision, hold}, args...)
	err := s.db.QueryRowContext(ctx, s.Rebind(`UPDATE conversation_gtd SET archive_hold=?,updated_at=CURRENT_TIMESTAMP
 WHERE conversation_id=? AND revision=? AND (?=FALSE OR archive_hold=FALSE) AND EXISTS(SELECT 1 FROM conversations c JOIN sources s ON s.id=c.source_id
 WHERE c.id=conversation_gtd.conversation_id AND s.source_type='beeper'`+scope+`) RETURNING conversation_id`), values...).Scan(&changed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConversationGTDRevision
	}
	return err
}

// BeeperConversationIDsContext maps only exact archived provider identities.
func (s *Store) BeeperConversationIDsContext(ctx context.Context, sourceID int64, chatIDs []string) (map[string]int64, error) {
	result := map[string]int64{}
	if len(chatIDs) == 0 {
		return result, nil
	}
	scope, scopeArgs := sourceScopeClause(ctx, "c.source_id")
	err := queryInChunksContext(ctx, s.db, chatIDs, append([]any{sourceID}, scopeArgs...),
		`SELECT c.source_conversation_id,c.id FROM conversations c JOIN sources s ON s.id=c.source_id WHERE s.source_type='beeper' AND c.source_id=?`+scope+` AND c.source_conversation_id IN (%s)`,
		func(rows *loggedRows) error {
			var id int64
			var chat string
			if err := rows.Scan(&chat, &id); err != nil {
				return err
			}
			result[chat] = id
			return nil
		})
	return result, err
}

// mergeConversationGTDContext keeps user state on the canonical conversation.
// It runs inside the existing source-merge transaction and source locks. Old
// review evidence cannot authorize a write through a different source identity.
func mergeConversationGTDContext(ctx context.Context, tx *loggedTx, from, into int64) error {
	var category sql.NullString
	var answer, hold bool
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT category, needs_answer, revision, archive_hold FROM conversation_gtd WHERE conversation_id = ?`, from).Scan(&category, &answer, &revision, &hold)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read merged conversation classification: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_gtd(conversation_id, category, needs_answer, revision, archive_hold)
 VALUES (?, ?, ?, ?, ?)
 ON CONFLICT(conversation_id) DO UPDATE SET
 category = COALESCE(conversation_gtd.category, excluded.category),
 needs_answer = conversation_gtd.needs_answer OR excluded.needs_answer,
 archive_hold = conversation_gtd.archive_hold OR excluded.archive_hold,
 revision = CASE WHEN conversation_gtd.revision >= excluded.revision THEN conversation_gtd.revision + 1 ELSE excluded.revision END,
 observed_activity_at = NULL, reviewed_context_stamp = NULL, reviewed_head = NULL, updated_at = CURRENT_TIMESTAMP`, into, category, answer, revision+1, hold)
	if err != nil {
		return fmt.Errorf("move merged conversation classification: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_gtd WHERE conversation_id = ?`, from); err != nil {
		return fmt.Errorf("remove retired conversation classification: %w", err)
	}
	return nil
}

// Planning folds categories for every input sharing a destination before
// preview or transfer, under the existing source-merge transaction and locks.
func validateConversationGTDMergeContext(ctx context.Context, tx *loggedTx, from, into int64, planned map[int64]string) error {
	var incoming, canonical sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT category FROM conversation_gtd WHERE conversation_id = ?),
 (SELECT category FROM conversation_gtd WHERE conversation_id = ?)`, from, into).Scan(&incoming, &canonical); err != nil {
		return fmt.Errorf("check merged conversation classification: %w", err)
	}
	category := canonical.String
	if previous, ok := planned[into]; ok {
		category = previous
	}
	if incoming.String != "" && category != "" && incoming.String != category {
		return invalidSourceMerge("conversation classifications conflict; resolve them before merging")
	}
	if category == "" {
		category = incoming.String
	}
	planned[into] = category
	return nil
}
