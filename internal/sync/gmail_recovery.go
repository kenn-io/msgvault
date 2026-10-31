package sync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const (
	recoveryCheckpointPrefix = "gmail-recovery:v1:"
	recoveryEnumerate        = "enumerate"
	recoveryReconcile        = "reconcile"
	recoveryCatchup          = "catchup"
)

// recoveryCheckpoint is the durable Gmail recovery protocol in sync_runs:
// cursor_before holds this versioned phase envelope, including on first/final
// pages; cursor_after holds H, pinned before enumeration and inherited on every
// retry. Phase, listing token and H are checkpointed atomically.
//
// enumerate resumes messages.list from PageToken. reconcile collects a fresh,
// complete presence inventory, then commits tombstones in one transaction.
// The catchup checkpoint is a separate commit: interruption in that gap repeats
// the idempotent reconciliation. Once catchup is durable, retries skip listings
// and restart History from H. History page tokens stay in memory, so every retry
// replays changes since H through idempotent message and tombstone writes.
//
// Catch-up completion publishes the new source cursor and completes the run
// atomically; publication has no separate resumable phase. Neither enumeration
// nor reconciliation publishes a source cursor. See initHistoryRecoveryState
// for legacy checkpoints that must restart enumeration while preserving H.
type recoveryCheckpoint struct {
	Phase     string `json:"phase"`
	PageToken string `json:"page_token,omitempty"`
	// Acquisition carries enumeration totals through catch-up. Each catch-up
	// retry replays History from H, so its own counts replace the previous
	// attempt's; the final summary adds them to these totals.
	Acquisition *recoveryCounts `json:"acquisition,omitempty"`
}

type recoveryCounts struct {
	Processed int64 `json:"processed"`
	Added     int64 `json:"added"`
	Updated   int64 `json:"updated"`
	Errors    int64 `json:"errors"`
}

func (state *syncState) durableCheckpoint() *store.Checkpoint {
	cp := *state.checkpoint
	if state.recoveryPhase != "" {
		encoded, _ := json.Marshal(recoveryCheckpoint{
			Phase: state.recoveryPhase, PageToken: cp.PageToken, Acquisition: state.acquisition,
		})
		cp.PageToken = recoveryCheckpointPrefix + string(encoded)
		cp.HandoffCursor = state.handoffCursor
		cp.InheritFromSyncID = state.predecessorID
	}
	return &cp
}

func (s *Syncer) saveFullCheckpoint(ctx context.Context, state *syncState) error {
	return s.store.UpdateSyncCheckpointContext(ctx, state.syncID, state.durableCheckpoint())
}

// decodeRecoveryCheckpoint reads the phase envelope saved in cursor_before.
func decodeRecoveryCheckpoint(token string) (recoveryCheckpoint, error) {
	if !strings.HasPrefix(token, "gmail-recovery:") {
		return recoveryCheckpoint{Phase: recoveryEnumerate, PageToken: token}, nil
	}
	if !strings.HasPrefix(token, recoveryCheckpointPrefix) {
		return recoveryCheckpoint{}, errors.New("unsupported Gmail recovery checkpoint version")
	}
	var cp recoveryCheckpoint
	if err := json.Unmarshal([]byte(strings.TrimPrefix(token, recoveryCheckpointPrefix)), &cp); err != nil {
		return recoveryCheckpoint{}, fmt.Errorf("decode Gmail recovery checkpoint: %w", err)
	}
	switch cp.Phase {
	case recoveryEnumerate, recoveryReconcile, recoveryCatchup:
	default:
		return recoveryCheckpoint{}, fmt.Errorf("unknown Gmail recovery phase %q", cp.Phase)
	}
	if cp.Phase != recoveryEnumerate && cp.PageToken != "" {
		return recoveryCheckpoint{}, fmt.Errorf("unexpected page token in Gmail recovery phase %q", cp.Phase)
	}
	return cp, nil
}

func restoreRecoveryCheckpoint(state *syncState, run *store.SyncRun) error {
	state.recoveryPhase = recoveryEnumerate
	if !checkpointMatchesRequest(run, gmailHistoryRecoveryCoverageFingerprint) {
		return nil // The narrower legacy listing must restart, keeping H.
	}
	cp, err := decodeRecoveryCheckpoint(run.CursorBefore.String)
	if err != nil {
		return err
	}
	state.recoveryPhase, state.pageToken = cp.Phase, cp.PageToken
	state.checkpoint = &store.Checkpoint{
		PageToken:         cp.PageToken,
		MessagesProcessed: run.MessagesProcessed,
		MessagesAdded:     run.MessagesAdded,
		MessagesUpdated:   run.MessagesUpdated,
		ErrorsCount:       run.ErrorsCount,
	}
	if cp.Phase == recoveryCatchup {
		// The prior run may be a failed catch-up whose counters cover only
		// History replay. Report the enumeration totals instead.
		state.acquisition = cp.Acquisition
		if state.acquisition == nil {
			state.acquisition = &recoveryCounts{}
		}
		state.checkpoint.MessagesProcessed = state.acquisition.Processed
		state.checkpoint.MessagesAdded = state.acquisition.Added
		state.checkpoint.MessagesUpdated = state.acquisition.Updated
		state.checkpoint.ErrorsCount = state.acquisition.Errors
	}
	return nil
}
