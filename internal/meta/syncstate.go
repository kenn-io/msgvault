package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// The checkpoint contains only fixed edges, opaque cursors and at most one
// bounded response per active depth. Completed history is never accumulated.
type syncState struct {
	Version     int                      `json:"version"`
	Source      string                   `json:"source"`
	Platform    string                   `json:"platform,omitempty"`
	NextSurface PostEdge                 `json:"next_surface"`
	Phases      map[PostEdge]*phaseState `json:"phases"`
}

type phaseState struct {
	ReadRequest int            `json:"read_request,omitempty"`
	ReadSyncID  int64          `json:"read_sync_id,omitempty"`
	After       string         `json:"after,omitempty"`
	Exhausted   bool           `json:"exhausted,omitempty"`
	Done        bool           `json:"done,omitempty"`
	Rescan      bool           `json:"rescan,omitempty"`
	Posts       []Object       `json:"posts,omitempty"`
	Stack       []commentFrame `json:"stack,omitempty"`
}

type commentFrame struct {
	ReadRequest int      `json:"read_request,omitempty"`
	ReadSyncID  int64    `json:"read_sync_id,omitempty"`
	Root        string   `json:"root"`
	Parent      string   `json:"parent"`
	After       string   `json:"after,omitempty"`
	Loaded      bool     `json:"loaded,omitempty"`
	Next        string   `json:"next,omitempty"`
	Pending     []Object `json:"pending,omitempty"`
}

// responseRead is polling bookkeeping. Archive markers keep it durable without
// making unchanged message content look edited to change-feed consumers.
type responseRead struct {
	ArchiveUID string `json:"archive_uid,omitempty"`
	SyncID     int64  `json:"sync_id"`
	Request    int    `json:"request"`
}

func (read responseRead) newer(other responseRead) bool {
	return read.SyncID > other.SyncID || (read.SyncID == other.SyncID && read.Request > other.Request)
}

func (r *importRun) responseReadKey(id string) string {
	return fmt.Sprintf("meta.poll_read:%d:%s", r.source.ID, id)
}

func (r *importRun) readArchiveUID(ctx context.Context) (string, error) {
	if r.archiveUID == "" {
		uid, err := r.imp.store.ArchiveUIDContext(ctx)
		if err != nil {
			return "", err
		}
		r.archiveUID = uid
	}
	return r.archiveUID, nil
}

func (r *importRun) loadResponseRead(ctx context.Context, id string, legacy responseRead) (responseRead, error) {
	uid, err := r.readArchiveUID(ctx)
	if err != nil {
		return responseRead{}, err
	}
	// Subsets preserve native evidence, but their sync-run sequence is new.
	// Unbound legacy references are usable only before a bound marker exists.
	if (legacy.ArchiveUID != "" && legacy.ArchiveUID != uid) || legacy.SyncID > r.syncID {
		legacy = responseRead{}
	}
	blob, found, err := r.imp.store.GetArchiveMarker(ctx, r.responseReadKey(id))
	if err != nil || !found {
		return legacy, err
	}
	var read responseRead
	if json.Unmarshal([]byte(blob), &read) != nil || read.SyncID < 0 || read.Request < 0 {
		return responseRead{}, errors.New("invalid polling response marker")
	}
	if (read.ArchiveUID != "" && read.ArchiveUID != uid) || read.SyncID > r.syncID {
		return legacy, nil
	}
	if legacy.ArchiveUID == "" && read.ArchiveUID != "" {
		legacy = responseRead{}
	}
	if legacy.newer(read) {
		return legacy, nil
	}
	return read, nil
}

func (r *importRun) saveResponseRead(ctx context.Context, id string, read responseRead) error {
	if r.state == nil || read.SyncID == 0 {
		return nil
	}
	uid, err := r.readArchiveUID(ctx)
	if err != nil {
		return err
	}
	read.ArchiveUID = uid
	blob, err := json.Marshal(read)
	if err != nil {
		return err
	}
	return r.imp.store.SetArchiveMarker(ctx, r.responseReadKey(id), string(blob))
}

func freshState(source string) *syncState {
	s := &syncState{Version: 2, Source: source, NextSurface: Posts, Phases: map[PostEdge]*phaseState{}}
	for _, edge := range postEdges {
		s.Phases[edge] = &phaseState{}
	}
	return s
}

func (st *syncState) complete() bool {
	for _, edge := range st.edges() {
		if !st.Phases[edge].Done {
			return false
		}
	}
	return true
}

func (st *syncState) edges() []PostEdge {
	if st.Platform == "instagram" {
		return instagramEdges[:]
	}
	return postEdges[:]
}

func freshSourceState(src *store.Source) *syncState {
	if src.SourceType == InstagramSourceType {
		return &syncState{Version: 1, Platform: "instagram", Source: src.Identifier, NextSurface: instagramMediaEdge, Phases: map[PostEdge]*phaseState{instagramMediaEdge: {}}}
	}
	return freshState(src.Identifier)
}

func loadState(ctx context.Context, st *store.Store, src *store.Source, full bool) (*syncState, error) {
	if full {
		return freshSourceState(src), nil
	}
	var blob string
	var priorID int64
	prior, err := st.GetLastSuccessfulSyncByTypeContext(ctx, src.ID, src.SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	if prior != nil {
		blob = prior.CursorAfter.String
		priorID = prior.ID
	}
	checkpoint, err := st.GetLatestCheckpointedSyncByTypeContext(ctx, src.ID, src.SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	if checkpoint != nil && checkpoint.ID > priorID {
		blob = checkpoint.CursorBefore.String
	}
	if blob == "" {
		return freshSourceState(src), nil
	}
	var state syncState
	if json.Unmarshal([]byte(blob), &state) != nil || !state.valid(src.Identifier) {
		return nil, errors.New("invalid comment checkpoint; restart this source with --full")
	}
	if state.complete() {
		fresh := freshSourceState(src)
		fresh.NextSurface = state.NextSurface
		return fresh, nil
	}
	// A successful independent surface is rechecked after another surface's
	// failure. A budget-only pause retains all finished surfaces until the
	// remaining surfaces have had a turn, so bounded runs also make progress.
	for _, edge := range state.edges() {
		p := state.Phases[edge]
		if p.Done && p.Rescan {
			state.Phases[edge] = &phaseState{}
		}
	}
	return &state, nil
}

func (st *syncState) valid(source string) bool {
	_, instagram := ParseInstagramSourceIdentifier(source)
	if st.Source != source || (instagram && (st.Platform != "instagram" || st.Version != 1 || st.NextSurface != instagramMediaEdge)) ||
		(!instagram && (st.Platform != "" || st.Version != 2 || !st.NextSurface.valid())) || len(st.Phases) != len(st.edges()) {
		return false
	}
	cursorOK := func(s string) bool { return len(s) <= 4096 && !strings.ContainsAny(s, "\r\n\x00") }
	for _, edge := range st.edges() {
		p := st.Phases[edge]
		if p == nil || len(p.Posts) > pageLimit || len(p.Stack) > 128 || !cursorOK(p.After) {
			return false
		}
		for _, post := range p.Posts {
			if !safeObjectID(post.ID) || (instagram && (post.InstagramMedia == nil || post.InstagramMedia.ID != post.ID)) {
				return false
			}
		}
		for i, f := range p.Stack {
			if !safeObjectID(f.Root) || !safeObjectID(f.Parent) || !cursorOK(f.After) || !cursorOK(f.Next) || len(f.Pending) > pageLimit {
				return false
			}
			if i > 0 && f.Root != p.Stack[0].Root {
				return false
			}
			for _, obj := range f.Pending {
				if !safeObjectID(obj.ID) || (instagram && (obj.InstagramComment == nil || obj.InstagramComment.ID != obj.ID)) {
					return false
				}
			}
		}
	}
	return true
}
