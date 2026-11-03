package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// The checkpoint contains only fixed edges, opaque cursors and at most one
// bounded response per active depth. Completed history is never accumulated.
type syncState struct {
	Version     int                      `json:"version"`
	Source      string                   `json:"source"`
	NextSurface PostEdge                 `json:"next_surface"`
	Phases      map[PostEdge]*phaseState `json:"phases"`
}

type phaseState struct {
	After     string         `json:"after,omitempty"`
	Exhausted bool           `json:"exhausted,omitempty"`
	Done      bool           `json:"done,omitempty"`
	Rescan    bool           `json:"rescan,omitempty"`
	Posts     []Object       `json:"posts,omitempty"`
	Stack     []commentFrame `json:"stack,omitempty"`
}

type commentFrame struct {
	Root    string   `json:"root"`
	Parent  string   `json:"parent"`
	After   string   `json:"after,omitempty"`
	Loaded  bool     `json:"loaded,omitempty"`
	Next    string   `json:"next,omitempty"`
	Pending []Object `json:"pending,omitempty"`
}

func freshState(source string) *syncState {
	s := &syncState{Version: 2, Source: source, NextSurface: Posts, Phases: map[PostEdge]*phaseState{}}
	for _, edge := range postEdges {
		s.Phases[edge] = &phaseState{}
	}
	return s
}

func (st *syncState) complete() bool {
	for _, edge := range postEdges {
		if !st.Phases[edge].Done {
			return false
		}
	}
	return true
}

func loadState(ctx context.Context, st *store.Store, src *store.Source, full bool) (*syncState, error) {
	if full {
		return freshState(src.Identifier), nil
	}
	var blob string
	var priorID int64
	prior, err := st.GetLastSuccessfulSyncContext(ctx, src.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	if prior != nil {
		blob = prior.CursorAfter.String
		priorID = prior.ID
	}
	checkpoint, err := st.GetLatestCheckpointedSyncContext(ctx, src.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	if checkpoint != nil && checkpoint.ID > priorID {
		blob = checkpoint.CursorBefore.String
	}
	if blob == "" {
		return freshState(src.Identifier), nil
	}
	var state syncState
	if json.Unmarshal([]byte(blob), &state) != nil || !state.valid(src.Identifier) {
		return nil, errors.New("invalid Facebook checkpoint; restart this source with --full")
	}
	if state.complete() {
		fresh := freshState(src.Identifier)
		fresh.NextSurface = state.NextSurface
		return fresh, nil
	}
	// A successful independent surface is rechecked after another surface's
	// failure. A budget-only pause retains all finished surfaces until the
	// remaining surfaces have had a turn, so bounded runs also make progress.
	for _, edge := range postEdges {
		p := state.Phases[edge]
		if p.Done && p.Rescan {
			state.Phases[edge] = &phaseState{}
		}
	}
	return &state, nil
}

func (st *syncState) valid(source string) bool {
	if st.Version != 2 || st.Source != source || !st.NextSurface.valid() || len(st.Phases) != len(postEdges) {
		return false
	}
	cursorOK := func(s string) bool { return len(s) <= 4096 && !strings.ContainsAny(s, "\r\n\x00") }
	for _, edge := range postEdges {
		p := st.Phases[edge]
		if p == nil || len(p.Posts) > pageLimit || len(p.Stack) > 128 || !cursorOK(p.After) {
			return false
		}
		for _, post := range p.Posts {
			if !safeObjectID(post.ID) {
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
				if !safeObjectID(obj.ID) {
					return false
				}
			}
		}
	}
	return true
}
