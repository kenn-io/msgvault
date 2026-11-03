package meta

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

type mentionSyncState struct {
	Version       int
	Source, After string
	Done          bool
}

func loadMentionState(ctx context.Context, st *store.Store, src *store.Source, full bool) (*mentionSyncState, error) {
	state := &mentionSyncState{Version: 1, Source: src.Identifier}
	if full {
		return state, nil
	}
	var blob string
	var newest int64
	for i, lookup := range []func(context.Context, int64, string) (*store.SyncRun, error){st.GetLastSuccessfulSyncByTypeContext, st.GetLatestCheckpointedSyncByTypeContext} {
		run, err := lookup(ctx, src.ID, MentionsSourceType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, err
		}
		if run != nil && run.ID > newest {
			blob, newest = run.CursorAfter.String, run.ID
			if i == 1 {
				blob = run.CursorBefore.String
			}
		}
	}
	if blob != "" {
		var saved mentionSyncState
		if json.Unmarshal([]byte(blob), &saved) != nil || saved.Version != 1 || saved.Source != src.Identifier {
			return nil, errors.New("invalid Instagram mentions checkpoint; restart this source with --full")
		}
		state = &saved
	}
	if state.Done {
		state.After, state.Done = "", false
	}
	return state, nil
}

func (s *mentionSyncState) checkpoint(sum *ImportSummary) (*store.Checkpoint, error) {
	cp := &store.Checkpoint{MessagesAdded: int64(sum.MessagesAdded), MessagesUpdated: int64(sum.MessagesUpdated), MessagesProcessed: int64(sum.MessagesProcessed)}
	if s != nil {
		blob, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		cp.PageToken = string(blob)
	}
	return cp, nil
}
