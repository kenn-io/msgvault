package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

var ErrSourceMergeInvalid = errors.New("invalid source merge")

// ErrArchiveOnlyDeletionTarget reports a provider deletion manifest that names
// a historical message occurrence preserved only as merge evidence.
var ErrArchiveOnlyDeletionTarget = errors.New("archive-only messages cannot be deleted at source")

type MergeSourcesRequest struct {
	FromSourceID int64 `json:"from_source_id"`
	IntoSourceID int64 `json:"into_source_id"`
	DryRun       bool  `json:"dry_run"`
}

// SourceMergeResult reports only archive metadata, never message content.
type SourceMergeResult struct {
	FromSourceID        int64 `json:"from_source_id"`
	IntoSourceID        int64 `json:"into_source_id"`
	MessagesMoved       int64 `json:"messages_moved"`
	DuplicatesHidden    int64 `json:"duplicates_hidden"`
	AmbiguousMatches    int64 `json:"ambiguous_matches"`
	ConversationsMoved  int64 `json:"conversations_moved"`
	AttachmentsCopied   int64 `json:"attachments_copied"`
	IdentitiesMerged    int64 `json:"identities_merged"`
	CheckpointConflicts int64 `json:"checkpoint_conflicts"`
	AlreadyMerged       bool  `json:"already_merged"`
	DryRun              bool  `json:"dry_run"`
}

func invalidSourceMerge(reason string) error {
	return fmt.Errorf("%w: %s", ErrSourceMergeInvalid, reason)
}

func (s *Store) sourceMergeRecorded(ctx context.Context, req MergeSourcesRequest) (SourceMergeResult, bool, error) {
	var result SourceMergeResult
	var intoID int64
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT into_source_id, report FROM source_merges WHERE from_source_id = ?`, req.FromSourceID).Scan(&intoID, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, fmt.Errorf("read source merge journal: %w", err)
	}
	if intoID != req.IntoSourceID {
		return result, true, invalidSourceMerge("source was already merged into another destination")
	}
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return result, true, fmt.Errorf("decode source merge report: %w", err)
	}
	result.AlreadyMerged = true
	result.DryRun = req.DryRun
	return result, true, nil
}

// MergeSourcesContext owns both sources' execution locks without abandoned-run
// recovery. All archive changes and retirement commit together; a failed or
// canceled call can be retried, and a committed retry returns the saved report.
func (s *Store) MergeSourcesContext(ctx context.Context, req MergeSourcesRequest) (result SourceMergeResult, retErr error) {
	result = SourceMergeResult{FromSourceID: req.FromSourceID, IntoSourceID: req.IntoSourceID, DryRun: req.DryRun}
	if req.FromSourceID <= 0 || req.IntoSourceID <= 0 || req.FromSourceID == req.IntoSourceID {
		return result, invalidSourceMerge("two distinct positive source IDs are required")
	}
	if previous, recorded, err := s.sourceMergeRecorded(ctx, req); recorded || err != nil {
		return previous, err
	}
	if s.sourceMergeAfterJournalCheckHook != nil {
		s.sourceMergeAfterJournalCheckHook()
	}
	first, second := req.FromSourceID, req.IntoSourceID
	if second < first {
		first, second = second, first
	}
	for _, id := range []int64{first, second} {
		lock, err := s.acquireSyncExecutionLock(ctx, id)
		if err != nil {
			if previous, recorded, journalErr := s.sourceMergeRecorded(ctx, req); recorded || journalErr != nil {
				return previous, journalErr
			}
			return result, err
		}
		defer func() { retErr = errors.Join(retErr, s.abandonSyncExecutionLock(id, lock)) }()
	}
	if previous, recorded, err := s.sourceMergeRecorded(ctx, req); recorded || err != nil {
		return previous, err
	}
	retErr = s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		// Serialized source removal takes the identity lock before its table
		// locks. Keep the same order before locking either source row here.
		if s.sourceMergeBeforeIdentityLockHook != nil {
			s.sourceMergeBeforeIdentityLockHook()
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := lockSourceMaintenance(ctx, tx); err != nil {
			return err
		}
		for _, id := range []int64{first, second} {
			if err := lockSyncSourceTx(ctx, tx, id); err != nil {
				return err
			}
			var busy bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_runs WHERE source_id = ? AND status = 'running')
    OR EXISTS(SELECT 1 FROM sync_operations WHERE source_id = ? AND status IN ('pending', 'running'))`, id, id).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return fmt.Errorf("source %d: %w", id, ErrSyncAlreadyActive)
			}
		}
		from, err := scanSource(tx.QueryRowContext(ctx, `SELECT `+sourceCatalogColumns+` FROM sources WHERE id = ?`, req.FromSourceID))
		if err != nil {
			return fmt.Errorf("read merge source: %w", err)
		}
		into, err := scanSource(tx.QueryRowContext(ctx, `SELECT `+sourceCatalogColumns+` FROM sources WHERE id = ?`, req.IntoSourceID))
		if err != nil {
			return fmt.Errorf("read merge destination: %w", err)
		}
		if from.SourceType != into.SourceType {
			return invalidSourceMerge("source types must match")
		}
		if from.MergedIntoSourceID != 0 || into.MergedIntoSourceID != 0 {
			return ErrSourceRetired
		}
		// Active provider receipts and local chat drafts remain bound to their
		// source. Discarded provider receipts are retained as audit evidence and
		// do not block retirement unless an operation is still unresolved.
		for _, draftTable := range []struct {
			table  string
			active string
		}{
			{table: "gmail_drafts", active: "discarded_at IS NULL OR pending_operation IS NOT NULL"},
			{table: "imap_drafts", active: "discarded_at IS NULL OR pending_operation IS NOT NULL"},
			{table: "beeper_drafts", active: "discarded_at IS NULL OR pending_operation IS NOT NULL"},
			{table: "chat_drafts", active: "1 = 1"},
		} {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+draftTable.table+` WHERE source_id = ? AND (`+draftTable.active+`)`, req.FromSourceID).Scan(&count); err != nil {
				return fmt.Errorf("check %s before merge: %w", draftTable.table, err)
			}
			if count > 0 {
				return invalidSourceMerge("sources with active drafts or unresolved provider operations cannot be merged")
			}
		}
		plan, err := buildSourceMergePlan(ctx, tx, req.FromSourceID, req.IntoSourceID, &result)
		if err != nil {
			return fmt.Errorf("plan source merge: %w", err)
		}
		if req.DryRun {
			return nil
		}
		if err := s.applySourceMergePlan(ctx, tx, req.FromSourceID, req.IntoSourceID, plan); err != nil {
			return fmt.Errorf("transfer source archive: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_settings(source_id, history_only, merged_into_source_id, retired_at)
   VALUES (?, TRUE, ?, `+s.dialect.Now()+`) ON CONFLICT (source_id) DO UPDATE SET
   history_only = TRUE, merged_into_source_id = excluded.merged_into_source_id, retired_at = excluded.retired_at`, req.FromSourceID, req.IntoSourceID); err != nil {
			return fmt.Errorf("retire merged source: %w", err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO source_merges(from_source_id, into_source_id, report) VALUES (?, ?, ?)`, req.FromSourceID, req.IntoSourceID, string(encoded))
		return err
	})
	return result, retErr
}
