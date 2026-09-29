package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	BeeperDraftOperationCreate = "create"
	BeeperDraftOperationEdit   = "edit"
	BeeperDraftOperationDelete = "delete"

	BeeperDraftPhaseClaimed             = "claimed"
	BeeperDraftPhaseClearDispatched     = "clear_dispatched"
	BeeperDraftPhaseClearConfirmed      = "clear_confirmed"
	BeeperDraftPhaseSetDispatched       = "set_dispatched"
	BeeperDraftPhaseRejected            = "rejected"
	BeeperDraftPhaseRemoteUnknown       = "remote_unknown"
	BeeperDraftPhaseAcceptedLocalFailed = "accepted_local_failed"
)

var (
	ErrBeeperDraftNotFound = errors.New("beeper draft not found")
	ErrBeeperDraftRevision = errors.New("beeper draft revision mismatch")
	ErrBeeperDraftPending  = errors.New("beeper draft has a pending operation")
	ErrBeeperDraftState    = errors.New("invalid Beeper draft state")
)

type BeeperDraftPending struct {
	Operation   string
	Phase       string
	Candidate   string
	OutcomeCode string
}

type BeeperDraft struct {
	DraftID       string
	SourceID      int64
	AccountID     string
	ChatID        string
	Revision      int64
	CommittedText *string
	Pending       *BeeperDraftPending
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func newBeeperDraftID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate Beeper draft ID: %w", err)
	}
	return "beeper-draft-" + hex.EncodeToString(raw[:]), nil
}

func validateBeeperDraftID(id string) error {
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "\x00\r\n") {
		return errors.New("invalid Beeper draft ID")
	}
	return nil
}

func validateBeeperDraftText(text string) error {
	if text == "" || strings.ContainsAny(text, "\x00") {
		return errors.New("beeper draft text must be non-empty")
	}
	return nil
}

// BeginBeeperDraftCreateContext allocates a managed binding and records the
// candidate before the provider request. Confirmed-cleared bindings are reused.
func (s *Store) BeginBeeperDraftCreateContext(ctx context.Context, sourceID int64, accountID, chatID, candidate string) (BeeperDraft, error) {
	if sourceID <= 0 || strings.TrimSpace(accountID) == "" || strings.TrimSpace(chatID) == "" {
		return BeeperDraft{}, errors.New("invalid Beeper draft source or chat")
	}
	if err := validateBeeperDraftText(candidate); err != nil {
		return BeeperDraft{}, err
	}
	var result BeeperDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceType, identifier string
		if err := tx.QueryRowContext(ctx, `SELECT source_type, identifier FROM sources WHERE id = ?`+s.dialect.SelectForUpdate(), sourceID).Scan(&sourceType, &identifier); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("invalid_source")
			}
			return fmt.Errorf("read Beeper draft source: %w", err)
		}
		if sourceType != "beeper" || identifier != accountID {
			return errors.New("invalid_source")
		}
		var draftID string
		err := tx.QueryRowContext(ctx, `SELECT draft_id FROM beeper_drafts WHERE source_id = ? AND chat_id = ?`+s.dialect.SelectForUpdate(), sourceID, chatID).Scan(&draftID)
		if err == nil {
			draft, loadErr := loadBeeperDraft(ctx, tx, s.dialect.SelectForUpdate(), draftID)
			if loadErr != nil {
				return loadErr
			}
			if draft.Pending != nil {
				return ErrBeeperDraftPending
			}
			if draft.CommittedText != nil {
				return errors.New("source_key_conflict")
			}
			result, err = s.updateBeeperDraftPending(ctx, tx, draft, draft.Revision+1, BeeperDraftOperationCreate, candidate)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check Beeper draft binding: %w", err)
		}
		draftID, err = newBeeperDraftID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO beeper_drafts (draft_id, source_id, account_id, chat_id, revision, pending_operation, pending_phase, candidate_text, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?, `+s.dialect.Now()+`, `+s.dialect.Now()+`)`, draftID, sourceID, accountID, chatID, BeeperDraftOperationCreate, BeeperDraftPhaseClaimed, candidate); err != nil {
			return fmt.Errorf("create Beeper draft binding: %w", err)
		}
		result = BeeperDraft{DraftID: draftID, SourceID: sourceID, AccountID: accountID, ChatID: chatID, Revision: 1, Pending: &BeeperDraftPending{Operation: BeeperDraftOperationCreate, Phase: BeeperDraftPhaseClaimed, Candidate: candidate}}
		return nil
	})
	return result, err
}

func (s *Store) GetBeeperDraftContext(ctx context.Context, draftID string) (BeeperDraft, error) {
	if err := validateBeeperDraftID(draftID); err != nil {
		return BeeperDraft{}, err
	}
	return loadBeeperDraft(ctx, s.db, "", draftID)
}

func (s *Store) GetBeeperDraft(draftID string) (BeeperDraft, error) {
	return s.GetBeeperDraftContext(context.Background(), draftID)
}

// GetBeeperDraftForSourceChatContext loads the one durable binding for an
// exact source/chat pair without scanning unrelated draft rows.
func (s *Store) GetBeeperDraftForSourceChatContext(ctx context.Context, sourceID int64, chatID string) (BeeperDraft, error) {
	if sourceID <= 0 || strings.TrimSpace(chatID) == "" || strings.ContainsAny(chatID, "\x00\r\n") {
		return BeeperDraft{}, ErrBeeperDraftNotFound
	}
	var draftID string
	err := s.db.QueryRowContext(ctx, `SELECT draft_id FROM beeper_drafts WHERE source_id = ? AND chat_id = ?`, sourceID, chatID).Scan(&draftID)
	if errors.Is(err, sql.ErrNoRows) {
		return BeeperDraft{}, fmt.Errorf("source %d chat %q: %w", sourceID, chatID, ErrBeeperDraftNotFound)
	}
	if err != nil {
		return BeeperDraft{}, fmt.Errorf("find Beeper draft binding: %w", err)
	}
	return s.GetBeeperDraftContext(ctx, draftID)
}

func loadBeeperDraft(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, lockClause, draftID string) (BeeperDraft, error) {
	var draft BeeperDraft
	var committed, operation, phase, candidate, outcome sql.NullString
	err := q.QueryRowContext(ctx, `SELECT draft_id, source_id, account_id, chat_id, revision, committed_text, pending_operation, pending_phase, candidate_text, outcome_code, created_at, updated_at FROM beeper_drafts WHERE draft_id = ?`+lockClause, draftID).Scan(&draft.DraftID, &draft.SourceID, &draft.AccountID, &draft.ChatID, &draft.Revision, &committed, &operation, &phase, &candidate, &outcome, &draft.CreatedAt, &draft.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BeeperDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrBeeperDraftNotFound)
	}
	if err != nil {
		return BeeperDraft{}, fmt.Errorf("load Beeper draft %q: %w", draftID, err)
	}
	if committed.Valid {
		value := committed.String
		draft.CommittedText = &value
	}
	if operation.Valid {
		if !phase.Valid {
			return BeeperDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrBeeperDraftState)
		}
		draft.Pending = &BeeperDraftPending{Operation: operation.String, Phase: phase.String, Candidate: candidate.String, OutcomeCode: outcome.String}
		if operation.String != BeeperDraftOperationDelete && !candidate.Valid {
			return BeeperDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrBeeperDraftState)
		}
	}
	return draft, nil
}

func (s *Store) loadBeeperDraftTx(ctx context.Context, tx *loggedTx, id string) (BeeperDraft, error) {
	return loadBeeperDraft(ctx, tx, s.dialect.SelectForUpdate(), id)
}

func (s *Store) lockBeeperDraftTx(ctx context.Context, tx *loggedTx, id string) error {
	if lockSQL := s.dialect.RowWriterLockSQL("beeper_drafts", "updated_at"); lockSQL != "" {
		lockSQL = strings.Replace(lockSQL, "WHERE id = ?", "WHERE draft_id = ?", 1)
		if _, err := tx.ExecContext(ctx, lockSQL, id); err != nil {
			return fmt.Errorf("lock Beeper draft %q: %w", id, err)
		}
	}
	return nil
}

func (s *Store) updateBeeperDraftPending(ctx context.Context, tx *loggedTx, draft BeeperDraft, revision int64, operation, candidate string) (BeeperDraft, error) {
	var value any
	if operation != BeeperDraftOperationDelete {
		value = candidate
	}
	if _, err := tx.ExecContext(ctx, `UPDATE beeper_drafts SET revision = ?, pending_operation = ?, pending_phase = ?, candidate_text = ?, outcome_code = NULL, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ? AND pending_operation IS NULL`, revision, operation, BeeperDraftPhaseClaimed, value, draft.DraftID, draft.Revision); err != nil {
		return BeeperDraft{}, fmt.Errorf("claim Beeper draft: %w", err)
	}
	updated := draft
	updated.Revision = revision
	updated.Pending = &BeeperDraftPending{Operation: operation, Phase: BeeperDraftPhaseClaimed, Candidate: candidate}
	if operation == BeeperDraftOperationDelete {
		updated.Pending.Candidate = ""
	}
	return updated, nil
}

// ClaimBeeperDraftContext records an edit or clear claim under a revision.
func (s *Store) ClaimBeeperDraftContext(ctx context.Context, draftID string, revision int64, operation, candidate string) (BeeperDraft, error) {
	if err := validateBeeperDraftID(draftID); err != nil {
		return BeeperDraft{}, err
	}
	if revision <= 0 {
		return BeeperDraft{}, ErrBeeperDraftRevision
	}
	if operation != BeeperDraftOperationEdit && operation != BeeperDraftOperationDelete {
		return BeeperDraft{}, ErrBeeperDraftState
	}
	if operation == BeeperDraftOperationEdit {
		if err := validateBeeperDraftText(candidate); err != nil {
			return BeeperDraft{}, err
		}
	}
	var claimed BeeperDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockBeeperDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadBeeperDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		if draft.Pending != nil {
			return ErrBeeperDraftPending
		}
		var sourceType, identifier string
		if err := tx.QueryRowContext(ctx, `SELECT source_type, identifier FROM sources WHERE id = ?`+s.dialect.SelectForUpdate(), draft.SourceID).Scan(&sourceType, &identifier); err != nil {
			return err
		}
		if sourceType != "beeper" || identifier != draft.AccountID {
			return errors.New("invalid_source")
		}
		claimed, err = s.updateBeeperDraftPending(ctx, tx, draft, draft.Revision+1, operation, candidate)
		return err
	})
	return claimed, err
}

func beeperDraftPhaseRank(phase string) int {
	switch phase {
	case BeeperDraftPhaseClaimed:
		return 1
	case BeeperDraftPhaseClearDispatched:
		return 2
	case BeeperDraftPhaseClearConfirmed:
		return 3
	case BeeperDraftPhaseSetDispatched:
		return 4
	case BeeperDraftPhaseRejected, BeeperDraftPhaseRemoteUnknown, BeeperDraftPhaseAcceptedLocalFailed:
		return 5
	default:
		return 0
	}
}

func validBeeperDraftPhase(operation, phase string) bool {
	switch operation {
	case BeeperDraftOperationCreate:
		return phase == BeeperDraftPhaseClaimed || phase == BeeperDraftPhaseSetDispatched || phase == BeeperDraftPhaseRejected || phase == BeeperDraftPhaseRemoteUnknown || phase == BeeperDraftPhaseAcceptedLocalFailed
	case BeeperDraftOperationEdit:
		return beeperDraftPhaseRank(phase) > 0
	case BeeperDraftOperationDelete:
		return phase == BeeperDraftPhaseClaimed || phase == BeeperDraftPhaseClearDispatched || phase == BeeperDraftPhaseClearConfirmed || phase == BeeperDraftPhaseRejected || phase == BeeperDraftPhaseRemoteUnknown || phase == BeeperDraftPhaseAcceptedLocalFailed
	default:
		return false
	}
}

func validBeeperDraftTransition(operation, current, next, code string) bool {
	if current == next {
		return true
	}
	switch operation {
	case BeeperDraftOperationCreate:
		switch current {
		case BeeperDraftPhaseClaimed:
			return next == BeeperDraftPhaseSetDispatched || next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown
		case BeeperDraftPhaseSetDispatched:
			return next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown || next == BeeperDraftPhaseAcceptedLocalFailed
		}
	case BeeperDraftOperationEdit:
		switch current {
		case BeeperDraftPhaseClaimed:
			return next == BeeperDraftPhaseClearDispatched || next == BeeperDraftPhaseSetDispatched || next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown
		case BeeperDraftPhaseClearDispatched:
			return next == BeeperDraftPhaseClearConfirmed || next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown
		case BeeperDraftPhaseClearConfirmed:
			return next == BeeperDraftPhaseSetDispatched || next == BeeperDraftPhaseRemoteUnknown
		case BeeperDraftPhaseSetDispatched:
			return next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown || next == BeeperDraftPhaseAcceptedLocalFailed ||
				(next == BeeperDraftPhaseClearConfirmed && strings.HasPrefix(code, "set_rejected:"))
		}
	case BeeperDraftOperationDelete:
		switch current {
		case BeeperDraftPhaseClaimed:
			return next == BeeperDraftPhaseClearDispatched || next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown ||
				(next == BeeperDraftPhaseClearConfirmed && code == "already_empty")
		case BeeperDraftPhaseClearDispatched:
			return next == BeeperDraftPhaseClearConfirmed || next == BeeperDraftPhaseRejected || next == BeeperDraftPhaseRemoteUnknown
		case BeeperDraftPhaseClearConfirmed:
			return next == BeeperDraftPhaseAcceptedLocalFailed
		}
	}
	return false
}

// RecordBeeperDraftOutcomeContext advances durable provider evidence.
func (s *Store) RecordBeeperDraftOutcomeContext(ctx context.Context, draftID string, revision int64, phase, code string) error {
	if err := validateBeeperDraftID(draftID); err != nil {
		return err
	}
	if revision <= 0 || strings.TrimSpace(phase) == "" || strings.TrimSpace(code) == "" {
		return ErrBeeperDraftState
	}
	if beeperDraftPhaseRank(phase) == 0 {
		return ErrBeeperDraftState
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockBeeperDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadBeeperDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		if draft.Pending == nil {
			return ErrBeeperDraftState
		}
		if !validBeeperDraftPhase(draft.Pending.Operation, phase) {
			return ErrBeeperDraftState
		}
		if !validBeeperDraftTransition(draft.Pending.Operation, draft.Pending.Phase, phase, code) {
			return ErrBeeperDraftState
		}
		result, err := tx.ExecContext(ctx, `UPDATE beeper_drafts SET pending_phase = ?, outcome_code = ?, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL`, phase, nullableString(code), draftID, revision)
		if err != nil {
			return fmt.Errorf("record Beeper draft outcome: %w", err)
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrBeeperDraftState
		}
		return nil
	})
}

// AbortBeeperDraftClaimContext releases a claim only before any uncertain
// provider dispatch.
func (s *Store) AbortBeeperDraftClaimContext(ctx context.Context, draftID string, revision int64) error {
	if err := validateBeeperDraftID(draftID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockBeeperDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadBeeperDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		if draft.Pending == nil || (draft.Pending.Phase != BeeperDraftPhaseClaimed && draft.Pending.Phase != BeeperDraftPhaseRejected) {
			return ErrBeeperDraftState
		}
		_, err = tx.ExecContext(ctx, `UPDATE beeper_drafts SET pending_operation = NULL, pending_phase = NULL, candidate_text = NULL, outcome_code = NULL, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ?`, draftID, revision)
		return err
	})
}

// RetireBeeperDraftAfterEmptyObservationContext clears any pending local
// attempt after the owner has observed an explicit empty provider slot.
func (s *Store) RetireBeeperDraftAfterEmptyObservationContext(ctx context.Context, draftID string, revision int64) (BeeperDraft, error) {
	if err := validateBeeperDraftID(draftID); err != nil {
		return BeeperDraft{}, err
	}
	if revision <= 0 {
		return BeeperDraft{}, ErrBeeperDraftRevision
	}
	var retired BeeperDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockBeeperDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadBeeperDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		if draft.Pending == nil || !validBeeperDraftPhase(draft.Pending.Operation, draft.Pending.Phase) {
			return ErrBeeperDraftState
		}
		result, err := tx.ExecContext(ctx, `UPDATE beeper_drafts SET committed_text = NULL, revision = revision + 1, pending_operation = NULL, pending_phase = NULL, candidate_text = NULL, outcome_code = NULL, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL`, draftID, revision)
		if err != nil {
			return fmt.Errorf("retire Beeper draft after empty observation: %w", err)
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrBeeperDraftState
		}
		retired, err = loadBeeperDraft(ctx, tx, s.dialect.SelectForUpdate(), draftID)
		return err
	})
	return retired, err
}

// FinishBeeperDraftContext commits a provider observation and advances the
// public revision. A nil observation records a confirmed empty slot.
func (s *Store) FinishBeeperDraftContext(ctx context.Context, draftID string, revision int64, observedText *string) (BeeperDraft, error) {
	if err := validateBeeperDraftID(draftID); err != nil {
		return BeeperDraft{}, err
	}
	if observedText != nil && strings.ContainsAny(*observedText, "\x00") {
		return BeeperDraft{}, ErrBeeperDraftState
	}
	var finished BeeperDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockBeeperDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadBeeperDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrBeeperDraftRevision
		}
		if draft.Pending == nil {
			return ErrBeeperDraftState
		}
		if draft.Pending.Phase != BeeperDraftPhaseClearConfirmed && draft.Pending.Phase != BeeperDraftPhaseSetDispatched {
			return ErrBeeperDraftState
		}
		result, err := tx.ExecContext(ctx, `UPDATE beeper_drafts SET committed_text = ?, revision = revision + 1, pending_operation = NULL, pending_phase = NULL, candidate_text = NULL, outcome_code = NULL, updated_at = `+s.dialect.Now()+` WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL`, nullableStringPtr(observedText), draftID, revision)
		if err != nil {
			return fmt.Errorf("finish Beeper draft: %w", err)
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrBeeperDraftState
		}
		finished, err = loadBeeperDraft(ctx, tx, s.dialect.SelectForUpdate(), draftID)
		return err
	})
	return finished, err
}

func nullableStringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
