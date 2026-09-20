package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	GmailDraftOperationEdit   = "edit"
	GmailDraftOperationDelete = "delete"
)

// GmailDraftReceipt identifies Gmail's stable draft and its current message.
// Gmail replaces the message ID when a draft is updated.
type GmailDraftReceipt struct {
	SourceID       int64
	GmailDraftID   string
	GmailMessageID string
	ThreadID       string
}

// GmailDraftPending contains the original archive identity and candidate
// bytes needed to keep an uncertain mutation blocked.
type GmailDraftPending struct {
	Operation                 string
	OriginalMessageID         int64
	OriginalGmailMessageID    string
	Raw                       []byte
	ReplacementGmailMessageID string
	Code                      string
}

// GmailDraft is the Store-owned lifecycle record for one Gmail draft.
type GmailDraft struct {
	DraftID          string
	SourceID         int64
	CurrentMessageID int64
	CurrentReceipt   GmailDraftReceipt
	Revision         int64
	DiscardedAt      *time.Time
	Pending          *GmailDraftPending
}

var (
	ErrGmailDraftNotFound = errors.New("Gmail draft not found")
	ErrGmailDraftRevision = errors.New("Gmail draft revision mismatch")
	ErrGmailDraftPending  = errors.New("Gmail draft has a pending operation")
	ErrGmailDraftState    = errors.New("invalid Gmail draft state")
)

// PersistGmailDraftContext commits the local message and managed ownership in
// one transaction after Gmail has accepted a create.
func (s *Store) PersistGmailDraftContext(
	ctx context.Context,
	receipt GmailDraftReceipt,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (GmailDraft, error) {
	if err := validateGmailDraftReceipt(receipt); err != nil {
		return GmailDraft{}, err
	}
	if build == nil {
		return GmailDraft{}, errors.New("persist Gmail draft requires a message builder")
	}
	draftID, err := newIMAPDraftID()
	if err != nil {
		return GmailDraft{}, err
	}
	var draft GmailDraft
	before := func(ctx context.Context, tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, receipt.SourceID).Scan(&sourceType); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("invalid_source")
			}
			return fmt.Errorf("read Gmail draft source: %w", err)
		}
		if sourceType != "gmail" {
			return errors.New("invalid_source")
		}
		var existingID int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, receipt.SourceID, receipt.GmailMessageID).Scan(&existingID)
		if err == nil {
			return errors.New("source_key_conflict")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check Gmail draft source key: %w", err)
		}
		var existingDraftID string
		err = tx.QueryRowContext(ctx, `
			SELECT draft_id FROM gmail_drafts WHERE source_id = ? AND gmail_draft_id = ?
		`, receipt.SourceID, receipt.GmailDraftID).Scan(&existingDraftID)
		if err == nil {
			return errors.New("source_key_conflict")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check Gmail draft ownership: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT draft_id FROM imap_drafts WHERE draft_id = ?`, draftID).Scan(&existingDraftID); err == nil {
			return errors.New("draft_id_conflict")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check draft ID: %w", err)
		}
		return nil
	}
	prepare := func(ctx context.Context, tx *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
		if data == nil || data.Message == nil {
			return nil, errors.New("persist Gmail draft requires a message")
		}
		if data.Message.SourceID != receipt.SourceID || data.Message.SourceMessageID != receipt.GmailMessageID {
			return nil, errors.New("source_key_conflict")
		}
		if data.MIMEAttachmentReplacement != nil {
			return nil, errors.New("Gmail draft persistence cannot replace attachments")
		}
		return prepareGmailDraftMessage(ctx, tx, receipt.SourceID, data)
	}
	after := func(ctx context.Context, tx *loggedTx, _ *MessagePersistData, messageID int64) error {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO gmail_drafts (
				draft_id, source_id, gmail_draft_id, current_message_id,
				current_gmail_message_id, thread_id, revision, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 1, %s, %s)
		`, s.dialect.Now(), s.dialect.Now()), draftID, receipt.SourceID,
			receipt.GmailDraftID, messageID, receipt.GmailMessageID, receipt.ThreadID); err != nil {
			return fmt.Errorf("persist Gmail draft ownership: %w", err)
		}
		draft = GmailDraft{
			DraftID: draftID, SourceID: receipt.SourceID, CurrentMessageID: messageID,
			CurrentReceipt: receipt, Revision: 1,
		}
		return nil
	}
	if _, err := s.persistMessageWithParticipantsTransaction(ctx, before, participants, build, prepare, after); err != nil {
		return GmailDraft{}, err
	}
	return draft, nil
}

func validateGmailDraftReceipt(receipt GmailDraftReceipt) error {
	if receipt.SourceID <= 0 || strings.TrimSpace(receipt.GmailDraftID) == "" ||
		strings.TrimSpace(receipt.GmailMessageID) == "" || strings.TrimSpace(receipt.ThreadID) == "" {
		return errors.New("invalid Gmail draft receipt")
	}
	return nil
}

func validateGmailDraftID(draftID string) error {
	if strings.TrimSpace(draftID) == "" || strings.ContainsAny(draftID, "\x00\r\n") {
		return errors.New("invalid Gmail draft ID")
	}
	return nil
}

// GetGmailDraft returns one managed Gmail draft.
func (s *Store) GetGmailDraft(draftID string) (GmailDraft, error) {
	return s.GetGmailDraftContext(context.Background(), draftID)
}

// GetGmailDraftContext reads ownership by local draft ID without consulting
// Gmail. Provider reads belong to edit and delete decisions.
func (s *Store) GetGmailDraftContext(ctx context.Context, draftID string) (GmailDraft, error) {
	if err := validateGmailDraftID(draftID); err != nil {
		return GmailDraft{}, err
	}
	return loadGmailDraft(ctx, s.db, "", draftID)
}

func loadGmailDraft(
	ctx context.Context,
	q interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	},
	lockClause string,
	draftID string,
) (GmailDraft, error) {
	var (
		draft                            GmailDraft
		discardedAt                      nullableTimestamp
		pendingOperation                 sql.NullString
		pendingOriginalMessageID         sql.NullInt64
		pendingOriginalGmailMessageID    sql.NullString
		pendingRaw                       []byte
		pendingReplacementGmailMessageID sql.NullString
		pendingCode                      sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT draft_id, source_id, gmail_draft_id, current_message_id,
		       current_gmail_message_id, thread_id, revision, discarded_at,
		       pending_operation, pending_original_message_id,
		       pending_original_gmail_message_id, pending_raw,
		       pending_replacement_gmail_message_id, pending_code
		FROM gmail_drafts
		WHERE draft_id = ?`+lockClause, draftID).Scan(
		&draft.DraftID, &draft.SourceID, &draft.CurrentReceipt.GmailDraftID,
		&draft.CurrentMessageID, &draft.CurrentReceipt.GmailMessageID,
		&draft.CurrentReceipt.ThreadID, &draft.Revision, &discardedAt,
		&pendingOperation, &pendingOriginalMessageID,
		&pendingOriginalGmailMessageID, &pendingRaw,
		&pendingReplacementGmailMessageID, &pendingCode,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return GmailDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrGmailDraftNotFound)
	}
	if err != nil {
		return GmailDraft{}, fmt.Errorf("load Gmail draft %q: %w", draftID, err)
	}
	draft.CurrentReceipt.SourceID = draft.SourceID
	if discardedAt.Valid {
		t := discardedAt.Time
		draft.DiscardedAt = &t
	}
	if pendingOperation.Valid {
		if !pendingOriginalMessageID.Valid || !pendingOriginalGmailMessageID.Valid ||
			strings.TrimSpace(pendingOriginalGmailMessageID.String) == "" {
			return GmailDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrGmailDraftState)
		}
		if pendingOperation.String != GmailDraftOperationEdit && pendingOperation.String != GmailDraftOperationDelete {
			return GmailDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrGmailDraftState)
		}
		if pendingOperation.String == GmailDraftOperationEdit && len(pendingRaw) == 0 {
			return GmailDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrGmailDraftState)
		}
		if pendingOperation.String == GmailDraftOperationDelete && pendingReplacementGmailMessageID.Valid {
			return GmailDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrGmailDraftState)
		}
		draft.Pending = &GmailDraftPending{
			Operation:              pendingOperation.String,
			OriginalMessageID:      pendingOriginalMessageID.Int64,
			OriginalGmailMessageID: pendingOriginalGmailMessageID.String,
			Raw:                    append([]byte(nil), pendingRaw...), Code: pendingCode.String,
		}
		if pendingReplacementGmailMessageID.Valid {
			draft.Pending.ReplacementGmailMessageID = pendingReplacementGmailMessageID.String
		}
	}
	return draft, nil
}

func (s *Store) loadGmailDraftTx(ctx context.Context, tx *loggedTx, draftID string) (GmailDraft, error) {
	return loadGmailDraft(ctx, tx, s.dialect.SelectForUpdate(), draftID)
}

func (s *Store) lockGmailDraftTx(ctx context.Context, tx *loggedTx, draftID string) error {
	if lockSQL := s.dialect.RowWriterLockSQL("gmail_drafts", "updated_at"); lockSQL != "" {
		lockSQL = strings.Replace(lockSQL, "WHERE id = ?", "WHERE draft_id = ?", 1)
		if _, err := tx.ExecContext(ctx, lockSQL, draftID); err != nil {
			return fmt.Errorf("lock Gmail draft %q: %w", draftID, err)
		}
	}
	return nil
}

// ClaimGmailDraftContext stores the current archive identity before a provider
// mutation. The revision does not advance until the mutation publishes.
func (s *Store) ClaimGmailDraftContext(
	ctx context.Context,
	draftID string,
	revision int64,
	operation string,
	replacementRaw []byte,
) (GmailDraft, error) {
	if err := validateGmailDraftID(draftID); err != nil {
		return GmailDraft{}, err
	}
	if revision <= 0 {
		return GmailDraft{}, fmt.Errorf("%w: expected positive revision", ErrGmailDraftRevision)
	}
	if operation != GmailDraftOperationEdit && operation != GmailDraftOperationDelete {
		return GmailDraft{}, fmt.Errorf("%w: unknown operation %q", ErrGmailDraftState, operation)
	}
	if operation == GmailDraftOperationEdit && len(replacementRaw) == 0 {
		return GmailDraft{}, errors.New("edit candidate must not be empty")
	}
	var claimed GmailDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockGmailDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadGmailDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return fmt.Errorf("%w: expected %d, found %d", ErrGmailDraftRevision, revision, draft.Revision)
		}
		if draft.DiscardedAt != nil {
			return fmt.Errorf("%w: draft is discarded", ErrGmailDraftState)
		}
		if draft.Pending != nil {
			return ErrGmailDraftPending
		}
		var raw any
		if operation == GmailDraftOperationEdit {
			raw = append([]byte(nil), replacementRaw...)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE gmail_drafts
			SET pending_operation = ?,
			    pending_original_message_id = ?,
			    pending_original_gmail_message_id = ?,
			    pending_raw = ?,
			    pending_replacement_gmail_message_id = NULL,
			    pending_code = NULL,
			    updated_at = `+s.dialect.Now()+`
			WHERE draft_id = ? AND revision = ? AND pending_operation IS NULL
		`, operation, draft.CurrentMessageID, draft.CurrentReceipt.GmailMessageID,
			raw, draftID, revision)
		if err != nil {
			return fmt.Errorf("claim Gmail draft %q: %w", draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrGmailDraftPending
		}
		claimed = draft
		claimed.Pending = &GmailDraftPending{
			Operation: operation, OriginalMessageID: draft.CurrentMessageID,
			OriginalGmailMessageID: draft.CurrentReceipt.GmailMessageID,
			Raw:                    append([]byte(nil), replacementRaw...),
		}
		if operation == GmailDraftOperationDelete {
			claimed.Pending.Raw = nil
		}
		return nil
	})
	if err != nil {
		return GmailDraft{}, err
	}
	return claimed, nil
}

// AbortGmailDraftContext clears a claim after a provider rejection or a
// mutation that was cancelled before any provider request.
func (s *Store) AbortGmailDraftContext(ctx context.Context, draftID string, revision int64) (GmailDraft, error) {
	if err := validateGmailDraftID(draftID); err != nil {
		return GmailDraft{}, err
	}
	var active GmailDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockGmailDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadGmailDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrGmailDraftRevision
		}
		if draft.Pending == nil {
			return ErrGmailDraftState
		}
		if err := s.clearGmailDraftPendingTx(ctx, tx, draftID, revision); err != nil {
			return err
		}
		active = draft
		active.Pending = nil
		return nil
	})
	if err != nil {
		return GmailDraft{}, err
	}
	return active, nil
}

// RecordGmailDraftOutcomeContext records provider outcome evidence without
// changing the current revision.
func (s *Store) RecordGmailDraftOutcomeContext(
	ctx context.Context,
	draftID string,
	revision int64,
	code string,
	replacementGmailMessageID string,
) error {
	if err := validateGmailDraftID(draftID); err != nil {
		return err
	}
	if revision <= 0 || strings.TrimSpace(code) == "" {
		return fmt.Errorf("%w: outcome requires positive revision and code", ErrGmailDraftState)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockGmailDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadGmailDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrGmailDraftRevision
		}
		if draft.Pending == nil {
			return ErrGmailDraftState
		}
		if replacementGmailMessageID != "" && draft.Pending.Operation != GmailDraftOperationEdit {
			return errors.New("delete outcome cannot carry a replacement message ID")
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE gmail_drafts
			SET pending_code = ?, pending_replacement_gmail_message_id = ?,
			    updated_at = `+s.dialect.Now()+`
			WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL
		`, code, nullableString(replacementGmailMessageID), draftID, revision)
		if err != nil {
			return fmt.Errorf("record Gmail draft outcome %q: %w", draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrGmailDraftState
		}
		return nil
	})
}

// PublishGmailDraftReplacementContext stores a successful update and advances
// the local revision in one transaction. The predecessor stays available until
// explicit GC because it is tombstoned rather than deleted here.
func (s *Store) PublishGmailDraftReplacementContext(
	ctx context.Context,
	draftID string,
	revision int64,
	newGmailMessageID string,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (GmailDraft, error) {
	if err := validateGmailDraftID(draftID); err != nil {
		return GmailDraft{}, err
	}
	if revision <= 0 || strings.TrimSpace(newGmailMessageID) == "" || build == nil {
		return GmailDraft{}, errors.New("invalid Gmail draft publication")
	}
	var published GmailDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockGmailDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadGmailDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrGmailDraftRevision
		}
		if draft.Pending == nil || draft.Pending.Operation != GmailDraftOperationEdit ||
			draft.Pending.ReplacementGmailMessageID != newGmailMessageID {
			return errors.New("Gmail draft replacement receipt is not recorded")
		}
		var existingMessageID int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, draft.SourceID, newGmailMessageID).Scan(&existingMessageID); err == nil {
			return fmt.Errorf("replacement source key already belongs to message %d", existingMessageID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check replacement source key: %w", err)
		}
		prepare := func(ctx context.Context, tx *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
			if data == nil || data.Message == nil {
				return nil, errors.New("persist Gmail draft replacement requires a message")
			}
			if data.Message.SourceID != draft.SourceID || data.Message.SourceMessageID != newGmailMessageID {
				return nil, errors.New("replacement message identity does not match receipt")
			}
			if !bytes.Equal(data.RawMIME, draft.Pending.Raw) {
				return nil, errors.New("replacement MIME does not match the claimed candidate")
			}
			return prepareGmailDraftMessage(ctx, tx, draft.SourceID, data)
		}
		messageID, err := s.persistMessageWithParticipantsTx(ctx, tx, nil, participants, build, prepare, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET deleted_from_source_at = `+s.dialect.Now()+`
			WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
		`, draft.CurrentMessageID, draft.SourceID); err != nil {
			return fmt.Errorf("tombstone Gmail draft predecessor: %w", err)
		}
		if err := s.bumpDerivedDataRevision(tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE gmail_drafts
			SET current_message_id = ?, current_gmail_message_id = ?,
			    revision = revision + 1, pending_operation = NULL,
			    pending_original_message_id = NULL,
			    pending_original_gmail_message_id = NULL,
			    pending_raw = NULL, pending_replacement_gmail_message_id = NULL,
			    pending_code = NULL, updated_at = %s
			WHERE draft_id = ? AND revision = ? AND pending_operation = 'edit'
		`, s.dialect.Now()), messageID, newGmailMessageID, draftID, revision)
		if err != nil {
			return fmt.Errorf("publish Gmail draft replacement %q: %w", draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrGmailDraftRevision
		}
		published = draft
		published.CurrentMessageID = messageID
		published.CurrentReceipt.GmailMessageID = newGmailMessageID
		published.Revision++
		published.Pending = nil
		return nil
	})
	if err != nil {
		return GmailDraft{}, err
	}
	return published, nil
}

// AdoptGmailDraftObservationContext records a message ID observed from Gmail
// or sync, then refuses the requested mutation at the new revision.
func (s *Store) AdoptGmailDraftObservationContext(
	ctx context.Context,
	draftID string,
	revision int64,
	observed GmailDraftReceipt,
	participants []ParticipantPersistData,
	build func([]int64) *MessagePersistData,
) (GmailDraft, error) {
	if err := validateGmailDraftID(draftID); err != nil {
		return GmailDraft{}, err
	}
	if err := validateGmailDraftReceipt(observed); err != nil || build == nil {
		return GmailDraft{}, errors.New("invalid Gmail draft observation")
	}
	var adopted GmailDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockGmailDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadGmailDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrGmailDraftRevision
		}
		if draft.Pending != nil {
			return ErrGmailDraftPending
		}
		if observed.SourceID != draft.SourceID || observed.GmailDraftID != draft.CurrentReceipt.GmailDraftID {
			return errors.New("observed Gmail draft identity does not match ownership")
		}
		var messageID int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, draft.SourceID, observed.GmailMessageID).Scan(&messageID)
		if errors.Is(err, sql.ErrNoRows) {
			prepare := func(ctx context.Context, tx *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
				if data == nil || data.Message == nil || data.Message.SourceID != draft.SourceID || data.Message.SourceMessageID != observed.GmailMessageID {
					return nil, errors.New("observed message identity does not match receipt")
				}
				return prepareGmailDraftMessage(ctx, tx, draft.SourceID, data)
			}
			after := func(ctx context.Context, tx *loggedTx, data *MessagePersistData, messageID int64) error {
				if data.MIMEAttachmentReplacement == nil {
					return nil
				}
				q := boundQuerier{ctx: ctx, q: tx}
				if err := s.replaceMIMEAttachmentsWith(q, messageID, data.MIMEAttachmentReplacement); err != nil {
					return fmt.Errorf("persist observed Gmail attachments: %w", err)
				}
				if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
					return fmt.Errorf("recompute observed Gmail attachment stats: %w", err)
				}
				return nil
			}
			messageID, err = s.persistMessageWithParticipantsTx(ctx, tx, nil, participants, build, prepare, after)
		} else if err != nil {
			return fmt.Errorf("find observed Gmail message: %w", err)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET deleted_from_source_at = `+s.dialect.Now()+`
			WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
		`, draft.CurrentMessageID, draft.SourceID); err != nil {
			return fmt.Errorf("tombstone Gmail draft predecessor: %w", err)
		}
		if err := s.bumpDerivedDataRevision(tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE gmail_drafts
			SET current_message_id = ?, current_gmail_message_id = ?, thread_id = ?,
			    revision = revision + 1, updated_at = %s
			WHERE draft_id = ? AND revision = ? AND pending_operation IS NULL
		`, s.dialect.Now()), messageID, observed.GmailMessageID, observed.ThreadID, draftID, revision)
		if err != nil {
			return fmt.Errorf("adopt Gmail draft observation %q: %w", draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrGmailDraftRevision
		}
		adopted = draft
		adopted.CurrentMessageID = messageID
		adopted.CurrentReceipt = observed
		adopted.Revision++
		return nil
	})
	if err != nil {
		return GmailDraft{}, err
	}
	return adopted, nil
}

// FinishGmailDraftDeleteContext records confirmed provider absence and
// discards the local draft. alreadyAbsent is used for an inspection 404 before
// a delete claim exists.
func (s *Store) FinishGmailDraftDeleteContext(
	ctx context.Context,
	draftID string,
	revision int64,
	alreadyAbsent bool,
) (GmailDraft, error) {
	if err := validateGmailDraftID(draftID); err != nil {
		return GmailDraft{}, err
	}
	var finished GmailDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockGmailDraftTx(ctx, tx, draftID); err != nil {
			return err
		}
		draft, err := s.loadGmailDraftTx(ctx, tx, draftID)
		if err != nil {
			return err
		}
		if draft.Revision != revision {
			return ErrGmailDraftRevision
		}
		if draft.Pending == nil {
			if !alreadyAbsent {
				return ErrGmailDraftState
			}
		} else if draft.Pending.Operation != GmailDraftOperationDelete {
			return ErrGmailDraftState
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET deleted_from_source_at = `+s.dialect.Now()+`
			WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
		`, draft.CurrentMessageID, draft.SourceID); err != nil {
			return fmt.Errorf("tombstone deleted Gmail draft: %w", err)
		}
		if err := s.bumpDerivedDataRevision(tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE gmail_drafts
			SET discarded_at = %s, revision = revision + 1,
			    pending_operation = NULL, pending_original_message_id = NULL,
			    pending_original_gmail_message_id = NULL, pending_raw = NULL,
			    pending_replacement_gmail_message_id = NULL, pending_code = NULL,
			    updated_at = %s
			WHERE draft_id = ? AND revision = ?
		`, s.dialect.Now(), s.dialect.Now()), draftID, revision)
		if err != nil {
			return fmt.Errorf("discard Gmail draft %q: %w", draftID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrGmailDraftRevision
		}
		finished = draft
		finished.Revision++
		finished.DiscardedAt = ptrTimeNow()
		finished.Pending = nil
		return nil
	})
	if err != nil {
		return GmailDraft{}, err
	}
	return finished, nil
}

func (s *Store) clearGmailDraftPendingTx(ctx context.Context, tx *loggedTx, draftID string, revision int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE gmail_drafts
		SET pending_operation = NULL, pending_original_message_id = NULL,
		    pending_original_gmail_message_id = NULL, pending_raw = NULL,
		    pending_replacement_gmail_message_id = NULL, pending_code = NULL,
		    updated_at = `+s.dialect.Now()+`
		WHERE draft_id = ? AND revision = ? AND pending_operation IS NOT NULL
	`, draftID, revision)
	if err != nil {
		return fmt.Errorf("clear Gmail draft pending evidence %q: %w", draftID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrGmailDraftState
	}
	return nil
}

func prepareGmailDraftMessage(
	ctx context.Context,
	tx *loggedTx,
	sourceID int64,
	data *MessagePersistData,
) (*MessagePersistData, error) {
	refs := append([]MessageLabelRef(nil), data.LabelRefs...)
	foundDraftLabel := false
	for _, ref := range refs {
		if ref.SourceLabelID == "DRAFT" {
			foundDraftLabel = true
			break
		}
	}
	if !foundDraftLabel {
		refs = append(refs, MessageLabelRef{
			SourceLabelID: "DRAFT",
			Info:          LabelInfo{Name: "DRAFT", Type: "system"},
		})
	}
	labelIDs, err := ensureMessageLabelRefsWith(boundQuerier{ctx: ctx, q: tx}, sourceID, refs)
	if err != nil {
		return nil, fmt.Errorf("resolve Gmail draft labels: %w", err)
	}
	prepared := *data
	prepared.LabelIDs = labelIDs
	prepared.PreserveLabels = false
	return &prepared, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
