package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	BeeperDraftOperationEdit   = draftOperationEdit
	BeeperDraftOperationDelete = draftOperationDelete
)

// BeeperDraft binds a local draft ID to one Beeper chat's composer. Text is
// the draft as Beeper last reported it, nil until the first write lands.
// Beeper holds the content, so no archive message backs the draft.
type BeeperDraft struct {
	DraftID     string
	SourceID    int64
	ChatID      string
	Text        *string
	Revision    int64
	DiscardedAt *time.Time
	Pending     *BeeperDraftPending
}

// BeeperDraftPending is a write msgvault started but has not confirmed.
type BeeperDraftPending struct {
	Operation string
	Text      string
}

var (
	ErrBeeperDraftNotFound = errors.New("beeper draft not found")
	ErrBeeperDraftExists   = errors.New("chat already has a managed Beeper draft")
	ErrBeeperDraftRevision = errors.New("beeper draft revision mismatch")
	ErrBeeperDraftPending  = errors.New("beeper draft has a pending operation")
	ErrBeeperDraftState    = errors.New("invalid Beeper draft state")
)

var beeperDrafts = draftTable[BeeperDraft]{
	provider:    "Beeper",
	table:       "beeper_drafts",
	errRevision: ErrBeeperDraftRevision,
	errPending:  ErrBeeperDraftPending,
	errState:    ErrBeeperDraftState,
	load:        loadBeeperDraft,
	state: func(d BeeperDraft) draftState {
		return draftState{revision: d.Revision, discarded: d.DiscardedAt != nil, pending: d.Pending != nil}
	},
	// A Beeper draft has no archive message to link.
	originalArgs: func(BeeperDraft) []any { return []any{nil} },
	withClaim: func(d BeeperDraft, operation string, raw []byte) BeeperDraft {
		d.Pending = &BeeperDraftPending{Operation: operation, Text: string(raw)}
		return d
	},
}

// CreateBeeperDraftContext records a new draft for a chat with its first
// write of text claimed, in one transaction, before Beeper is asked to hold it. A chat has at most
// one live draft; ErrBeeperDraftExists returns that draft.
func (s *Store) CreateBeeperDraftContext(ctx context.Context, sourceID int64, chatID, text string) (BeeperDraft, error) {
	if sourceID <= 0 || strings.TrimSpace(chatID) == "" || text == "" {
		return BeeperDraft{}, errors.New("invalid Beeper draft")
	}
	draftID, err := newIMAPDraftID()
	if err != nil {
		return BeeperDraft{}, err
	}
	var existing string
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT draft_id FROM beeper_drafts
			WHERE source_id = ? AND chat_id = ? AND discarded_at IS NULL
		`, sourceID, chatID).Scan(&existing)
		if err == nil {
			return ErrBeeperDraftExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check Beeper draft for chat: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO beeper_drafts (draft_id, source_id, chat_id, revision, pending_operation, pending_raw, created_at, updated_at)
			VALUES (?, ?, ?, 1, ?, ?, `+s.dialect.Now()+`, `+s.dialect.Now()+`)
		`, draftID, sourceID, chatID, BeeperDraftOperationEdit, []byte(text))
		if err != nil {
			return fmt.Errorf("create Beeper draft: %w", err)
		}
		return nil
	})
	if errors.Is(err, ErrBeeperDraftExists) {
		draft, loadErr := s.GetBeeperDraftContext(ctx, existing)
		if loadErr != nil {
			return BeeperDraft{}, loadErr
		}
		return draft, ErrBeeperDraftExists
	}
	if err != nil {
		return BeeperDraft{}, err
	}
	return BeeperDraft{
		DraftID: draftID, SourceID: sourceID, ChatID: chatID, Revision: 1,
		Pending: &BeeperDraftPending{Operation: BeeperDraftOperationEdit, Text: text},
	}, nil
}

// LiveBeeperDraftContext returns the chat's managed draft that is not
// discarded, or ErrBeeperDraftNotFound.
func (s *Store) LiveBeeperDraftContext(ctx context.Context, sourceID int64, chatID string) (BeeperDraft, error) {
	var draftID string
	err := s.db.QueryRowContext(ctx, `
		SELECT draft_id FROM beeper_drafts
		WHERE source_id = ? AND chat_id = ? AND discarded_at IS NULL
	`, sourceID, chatID).Scan(&draftID)
	if errors.Is(err, sql.ErrNoRows) {
		return BeeperDraft{}, ErrBeeperDraftNotFound
	}
	if err != nil {
		return BeeperDraft{}, fmt.Errorf("find Beeper draft for chat: %w", err)
	}
	return s.GetBeeperDraftContext(ctx, draftID)
}

// GetBeeperDraftContext reads a draft by local ID without contacting Beeper.
func (s *Store) GetBeeperDraftContext(ctx context.Context, draftID string) (BeeperDraft, error) {
	if err := beeperDrafts.validateID(draftID); err != nil {
		return BeeperDraft{}, err
	}
	return loadBeeperDraft(ctx, s.db, "", draftID)
}

func loadBeeperDraft(ctx context.Context, q contextRowQuerier, lockClause string, draftID string) (BeeperDraft, error) {
	var (
		draft       BeeperDraft
		text        sql.NullString
		discardedAt nullableTimestamp
		operation   sql.NullString
		pendingRaw  []byte
	)
	err := q.QueryRowContext(ctx, `
		SELECT draft_id, source_id, chat_id, text, revision, discarded_at, pending_operation, pending_raw
		FROM beeper_drafts
		WHERE draft_id = ?`+lockClause, draftID).Scan(
		&draft.DraftID, &draft.SourceID, &draft.ChatID, &text, &draft.Revision,
		&discardedAt, &operation, &pendingRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return BeeperDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrBeeperDraftNotFound)
	}
	if err != nil {
		return BeeperDraft{}, fmt.Errorf("load Beeper draft %q: %w", draftID, err)
	}
	if text.Valid {
		draft.Text = &text.String
	}
	if discardedAt.Valid {
		t := discardedAt.Time
		draft.DiscardedAt = &t
	}
	if operation.Valid {
		draft.Pending = &BeeperDraftPending{Operation: operation.String, Text: string(pendingRaw)}
	}
	return draft, nil
}

// ClaimBeeperDraftContext records the write about to be sent to Beeper.
func (s *Store) ClaimBeeperDraftContext(ctx context.Context, draftID string, revision int64, operation, text string) (BeeperDraft, error) {
	return beeperDrafts.claim(ctx, s, draftID, revision, operation, []byte(text))
}

// AbortBeeperDraftContext drops a claim whose write Beeper does not show. A
// draft whose first write never landed is discarded, so its chat is free for
// a new draft.
func (s *Store) AbortBeeperDraftContext(ctx context.Context, draftID string, revision int64) (BeeperDraft, error) {
	var active BeeperDraft
	err := beeperDrafts.inTx(ctx, s, draftID, func(tx *loggedTx, draft BeeperDraft) error {
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		active = draft
		active.Pending = nil
		if draft.Text != nil {
			return beeperDrafts.clearPendingTx(ctx, s, tx, draftID, revision)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE beeper_drafts SET discarded_at = `+s.dialect.Now()+`, revision = revision + 1, `+
			beeperDrafts.pendingNullSQL()+`, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ?`, draftID, revision); err != nil {
			return fmt.Errorf("discard unwritten Beeper draft %q: %w", draftID, err)
		}
		active.Revision++
		active.DiscardedAt = ptrTimeNow()
		return nil
	})
	return active, err
}

// FinishBeeperDraftContext commits a claimed write Beeper confirmed. An edit
// stores the text Beeper reported and a delete discards the draft. The first
// write of a new draft keeps revision 1.
func (s *Store) FinishBeeperDraftContext(ctx context.Context, draftID string, revision int64, text string) (BeeperDraft, error) {
	var finished BeeperDraft
	err := beeperDrafts.inTx(ctx, s, draftID, func(tx *loggedTx, draft BeeperDraft) error {
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		if draft.Pending == nil {
			return ErrBeeperDraftState
		}
		finished = draft
		finished.Pending = nil
		set := "discarded_at = " + s.dialect.Now() + ", revision = revision + 1"
		args := []any{}
		if draft.Pending.Operation == BeeperDraftOperationEdit {
			set = "text = ?, revision = revision + CASE WHEN text IS NULL THEN 0 ELSE 1 END"
			args = append(args, text)
			finished.Text = &text
			if draft.Text != nil {
				finished.Revision++
			}
		} else {
			finished.Revision++
			finished.DiscardedAt = ptrTimeNow()
		}
		result, err := tx.ExecContext(ctx, `UPDATE beeper_drafts SET `+set+`, `+beeperDrafts.pendingNullSQL()+
			`, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ?`, append(args, draftID, revision)...)
		if err != nil {
			return fmt.Errorf("finish Beeper draft %q: %w", draftID, err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return ErrBeeperDraftState
		}
		return nil
	})
	if err != nil {
		return BeeperDraft{}, err
	}
	return finished, nil
}
