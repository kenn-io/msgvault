package telegram

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

const liveSyncType = "sync-telegram"

// WithLiveSyncState owns a source run, recovers only live-sync checkpoints,
// and publishes the inherited checkpoint before work begins. The callback
// must save each state transition after its effects commit. Only the last
// successfully saved snapshot can be finalized or resumed.
func WithLiveSyncState(ctx context.Context, st *store.Store, sourceID int64, work func(context.Context, *store.Store, *SyncState, func(context.Context, *SyncState) error) error) (retErr error) {
	if st == nil || sourceID <= 0 || work == nil {
		return errors.New("invalid Telegram live sync")
	}
	execution, err := st.AcquireSyncExecutionContext(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("acquire Telegram sync: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("load Telegram sync source: %w", err)
	}
	if source == nil || source.SourceType != SourceType {
		return errors.New("Telegram sync requires a Telegram source")
	}
	state := NewSyncState()
	completedRun, err := st.GetLastSuccessfulSyncByTypeContext(ctx, sourceID, liveSyncType)
	if err == nil {
		if !completedRun.CursorAfter.Valid || completedRun.CursorAfter.String == "" {
			return errors.New("completed Telegram sync has no state")
		}
		state, err = LoadSyncState(completedRun.CursorAfter.String)
		if err != nil {
			return fmt.Errorf("load completed Telegram sync: %w", err)
		}
	} else if !errors.Is(err, store.ErrSyncRunNotFound) {
		return err
	}
	checkpoint, err := st.GetLatestCheckpointedSyncByTypeContext(ctx, sourceID, liveSyncType)
	if err == nil {
		state, err = LoadSyncState(checkpoint.CursorBefore.String)
		if err != nil {
			return fmt.Errorf("load Telegram sync checkpoint: %w", err)
		}
	} else if !errors.Is(err, store.ErrSyncRunNotFound) {
		return err
	}
	runID, err := execution.StartSyncContext(ctx, liveSyncType, "")
	if err != nil {
		return fmt.Errorf("start Telegram sync: %w", err)
	}
	scoped := st.ScopedToSync(sourceID, runID)
	var saved *store.Checkpoint
	completed := false
	defer func() {
		if completed || retErr == nil {
			return
		}
		if err := scoped.InterruptSyncWithCheckpoint(ctx, runID, retErr, saved); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("finalize Telegram sync: %w", err))
		}
	}()
	save := func(ctx context.Context, next *SyncState) error {
		blob, err := next.Marshal()
		if err != nil {
			return err
		}
		cp := &store.Checkpoint{PageToken: blob, MessagesProcessed: next.Counts.Processed, MessagesAdded: next.Counts.Added, MessagesUpdated: next.Counts.Updated}
		if err := scoped.UpdateSyncCheckpointContext(ctx, runID, cp); err != nil {
			return fmt.Errorf("save Telegram sync checkpoint: %w", err)
		}
		saved = cp
		return nil
	}
	if err := save(ctx, state); err != nil {
		return err
	}
	if err := work(ctx, scoped, state, save); err != nil {
		return err
	}
	if err := scoped.CompleteSyncContext(ctx, runID, saved.PageToken); err != nil {
		return fmt.Errorf("complete Telegram sync: %w", err)
	}
	completed = true
	return nil
}
