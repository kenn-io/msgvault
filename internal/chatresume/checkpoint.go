// Package chatresume shares checkpoint handling for chat imports.
package chatresume

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

func Load(ctx context.Context, st *store.Store, sourceID int64, syncType string, dst any) (bool, error) {
	prev, err := st.GetLatestCheckpointedSyncByType(ctx, sourceID, syncType)
	if errors.Is(err, store.ErrSyncRunNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if prev == nil || !prev.CursorBefore.Valid {
		return false, nil
	}
	if err := json.Unmarshal([]byte(prev.CursorBefore.String), dst); err != nil {
		return false, err
	}
	return true, nil
}

func Save(ctx context.Context, st *store.Store, syncID int64, cursor any, processed, added int64) (*store.Checkpoint, error) {
	cp := &store.Checkpoint{MessagesProcessed: processed, MessagesAdded: added}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return cp, err
	}
	cp.PageToken = string(payload)
	if err := st.UpdateSyncCheckpointContext(ctx, syncID, cp); err != nil {
		return cp, err
	}
	return cp, nil
}

func Finish(ctx context.Context, st *store.Store, sourceID, syncID int64, messageType string, cursor any, processed, added int64, paused bool, runErr error) error {
	// Finalization must record the outcome even when the caller cancels.
	ctx = context.WithoutCancel(ctx)
	runErr = errors.Join(runErr, st.ResolveDeferredMessageRepliesContext(ctx, sourceID, messageType, 500), st.RecomputeConversationStatsContext(ctx, sourceID))
	cp, err := Save(ctx, st, syncID, cursor, processed, added)
	runErr = errors.Join(runErr, err)
	if runErr != nil {
		return errors.Join(runErr, st.FailSyncContext(ctx, syncID, runErr.Error()))
	}
	if paused {
		return st.PauseSyncWithCheckpoint(ctx, syncID, cp)
	}
	return st.CompleteSyncContext(ctx, syncID, "")
}
