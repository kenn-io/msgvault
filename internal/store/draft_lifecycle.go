package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	draftOperationEdit   = "edit"
	draftOperationDelete = "delete"
)

// draftState is the part of a draft record the shared claim guards on.
type draftState struct {
	revision  int64
	discarded bool
	pending   bool
}

// draftTable describes one provider's managed-draft table and receipt.
type draftTable[D any] struct {
	provider           string // "IMAP" or "Gmail", as existing error text spells it
	table              string
	originalColumns    []string // receipt columns after pending_original_message_id
	replacementColumns []string
	errRevision        error
	errPending         error
	errState           error
	load               func(ctx context.Context, q contextRowQuerier, lockClause, draftID string) (D, error)
	state              func(D) draftState
	originalArgs       func(D) []any // current message ID, then originalColumns values
	withClaim          func(draft D, operation string, raw []byte) D
}

func (t draftTable[D]) validateID(draftID string) error {
	if strings.TrimSpace(draftID) == "" || strings.ContainsAny(draftID, "\x00\r\n") {
		return fmt.Errorf("invalid %s draft ID", t.provider)
	}
	return nil
}

func assignDraftColumns(columns []string, value string) string {
	var b strings.Builder
	for _, column := range columns {
		b.WriteString(column + " = " + value + ", ")
	}
	return b.String()
}

func (t draftTable[D]) pendingNullSQL() string {
	return "pending_operation = NULL, pending_original_message_id = NULL, " +
		assignDraftColumns(t.originalColumns, "NULL") + "pending_raw = NULL, " +
		assignDraftColumns(t.replacementColumns, "NULL") + "pending_code = NULL"
}

func (t draftTable[D]) lockTx(ctx context.Context, s *Store, tx *loggedTx, draftID string) error {
	if lockSQL := s.dialect.RowWriterLockSQL(t.table, "updated_at"); lockSQL != "" {
		// Managed drafts use draft_id instead of the dialect helper's id key.
		lockSQL = strings.Replace(lockSQL, "WHERE id = ?", "WHERE draft_id = ?", 1)
		if _, err := tx.ExecContext(ctx, lockSQL, draftID); err != nil {
			return fmt.Errorf("lock %s draft %q: %w", t.provider, draftID, err)
		}
	}
	return nil
}

// inTx runs fn in one transaction after locking and loading the draft row.
func (t draftTable[D]) inTx(ctx context.Context, s *Store, draftID string, fn func(tx *loggedTx, draft D) error) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := t.lockTx(ctx, s, tx, draftID); err != nil {
			return err
		}
		draft, err := t.load(ctx, tx, s.dialect.SelectForUpdate(), draftID)
		if err != nil {
			return err
		}
		return fn(tx, draft)
	})
}

// inAttributionTx is inTx for publication entries that write messages or
// labels: it opens through the attribution entry, locking the draft's source
// before the draft row.
func (t draftTable[D]) inAttributionTx(ctx context.Context, s *Store, draftID string, fn func(tx *loggedTx, draft D) error) error {
	var sourceID int64
	err := s.db.QueryRowContext(ctx, `SELECT source_id FROM `+t.table+` WHERE draft_id = ?`, draftID).Scan(&sourceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read %s draft %q source: %w", t.provider, draftID, err)
	}
	lock := attributionLock{}
	if sourceID > 0 {
		lock.Sources = []int64{sourceID}
	}
	return s.withAttributionTxContext(ctx, lock, func(tx *loggedTx) error {
		if err := t.lockTx(ctx, s, tx, draftID); err != nil {
			return err
		}
		draft, err := t.load(ctx, tx, s.dialect.SelectForUpdate(), draftID)
		if err != nil {
			return err
		}
		var lockedSourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM `+t.table+` WHERE draft_id = ?`, draftID).Scan(&lockedSourceID); err != nil {
			return fmt.Errorf("read %s draft %q source: %w", t.provider, draftID, err)
		}
		if lockedSourceID != sourceID {
			return fmt.Errorf("%s draft %q moved from source %d to %d", t.provider, draftID, sourceID, lockedSourceID)
		}
		return fn(tx, draft)
	})
}

func (t draftTable[D]) clearPendingTx(ctx context.Context, s *Store, tx *loggedTx, draftID string, revision int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE `+t.table+` SET `+t.pendingNullSQL()+`, updated_at = `+s.dialect.Now()+`
		WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL`, draftID, revision)
	if err != nil {
		return fmt.Errorf("clear %s draft pending evidence %q: %w", t.provider, draftID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return t.errState
	}
	return nil
}

func checkDraftReplacementSourceKeyTx(ctx context.Context, tx *loggedTx, sourceID int64, sourceMessageID string) error {
	var existingMessageID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
	`, sourceID, sourceMessageID).Scan(&existingMessageID); err == nil {
		return fmt.Errorf("replacement source key already belongs to message %d", existingMessageID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check replacement source key: %w", err)
	}
	return nil
}

func (t draftTable[D]) checkOutcomeRequest(revision int64, code string) error {
	if revision <= 0 || strings.TrimSpace(code) == "" {
		return fmt.Errorf("%w: outcome requires positive revision and code", t.errState)
	}
	return nil
}

// claim durably records the original receipt and candidate bytes before a
// provider mutation. The revision remains unchanged.
func (t draftTable[D]) claim(ctx context.Context, s *Store, draftID string, revision int64, operation string, replacementRaw []byte) (D, error) {
	var claimed D
	if err := t.validateID(draftID); err != nil {
		return claimed, err
	}
	if revision <= 0 {
		return claimed, fmt.Errorf("%w: expected positive revision", t.errRevision)
	}
	if operation != draftOperationEdit && operation != draftOperationDelete {
		return claimed, fmt.Errorf("%w: unknown operation %q", t.errState, operation)
	}
	if operation == draftOperationEdit && len(replacementRaw) == 0 {
		return claimed, errors.New("edit candidate must not be empty")
	}
	err := t.inTx(ctx, s, draftID, func(tx *loggedTx, draft D) error {
		st := t.state(draft)
		if st.revision != revision {
			return fmt.Errorf("%w: expected %d, found %d", t.errRevision, revision, st.revision)
		}
		if st.discarded {
			return fmt.Errorf("%w: draft is discarded", t.errState)
		}
		if st.pending {
			return t.errPending
		}
		var raw []byte
		if operation == draftOperationEdit {
			raw = append([]byte(nil), replacementRaw...)
		}
		args := append([]any{operation}, t.originalArgs(draft)...)
		result, err := tx.ExecContext(ctx, `UPDATE `+t.table+` SET pending_operation = ?, pending_original_message_id = ?, `+
			assignDraftColumns(t.originalColumns, "?")+`pending_raw = ?, `+assignDraftColumns(t.replacementColumns, "NULL")+
			`pending_code = NULL, updated_at = `+s.dialect.Now()+`
			WHERE draft_id = ? AND revision = ? AND pending_operation IS NULL`, append(args, raw, draftID, revision)...)
		if err != nil {
			return fmt.Errorf("claim %s draft %q: %w", t.provider, draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check %s draft claim %q: %w", t.provider, draftID, err)
		}
		if affected != 1 {
			return t.errPending
		}
		claimed = t.withClaim(draft, operation, raw)
		return nil
	})
	if err != nil {
		var zero D
		return zero, err
	}
	return claimed, nil
}
