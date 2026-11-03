package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// The checkpoint contains only fixed edges, opaque cursors and at most one
// bounded response per active depth. Completed history is never accumulated.
type syncState struct {
	Version     int                      `json:"version"`
	Source      string                   `json:"source"`
	AdAccountID string                   `json:"ad_account_id,omitempty"`
	Platform    string                   `json:"platform,omitempty"`
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
	for _, edge := range st.edges() {
		if !st.Phases[edge].Done {
			return false
		}
	}
	return true
}

func (st *syncState) edges() []PostEdge {
	if st.Platform == "instagram" {
		if st.AdAccountID != "" {
			return instagramAdEdges[:]
		}
		return instagramEdges[:]
	}
	return postEdges[:]
}

func freshSourceState(src *store.Source, adAccount string) *syncState {
	if src.SourceType != InstagramSourceType {
		return freshState(src.Identifier)
	}
	state := &syncState{Version: 1, Platform: "instagram", Source: src.Identifier, AdAccountID: adAccount, NextSurface: instagramMediaEdge, Phases: map[PostEdge]*phaseState{}}
	if adAccount != "" {
		state.Version = 2
	}
	for _, edge := range state.edges() {
		state.Phases[edge] = &phaseState{}
	}
	return state
}

func loadState(ctx context.Context, st *store.Store, src *store.Source, full bool, adAccount string) (*syncState, error) {
	if full {
		return freshSourceState(src, adAccount), nil
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
		return freshSourceState(src, adAccount), nil
	}
	var state syncState
	if json.Unmarshal([]byte(blob), &state) != nil || !state.valid(src.Identifier) || state.AdAccountID != adAccount {
		return nil, errors.New("invalid comment checkpoint; restart this source with --full")
	}
	if state.complete() {
		fresh := freshSourceState(src, adAccount)
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
	identity, instagram := ParseInstagramSourceIdentifier(source)
	expectedVersion := 1
	if st.AdAccountID != "" {
		expectedVersion = 2
	}
	if st.Source != source || (instagram && (st.Platform != "instagram" || st.Version != expectedVersion || (st.AdAccountID != "" && !ValidAccountID(st.AdAccountID)))) ||
		(!instagram && (st.Platform != "" || st.AdAccountID != "" || st.Version != 2)) || !slices.Contains(st.edges(), st.NextSurface) || len(st.Phases) != len(st.edges()) {
		return false
	}
	cursorOK := func(s string) bool { return len(s) <= 4096 && !strings.ContainsAny(s, "\r\n\x00") }
	for _, edge := range st.edges() {
		p := st.Phases[edge]
		if p == nil || len(p.Posts) > pageLimit || len(p.Stack) > 128 || !cursorOK(p.After) {
			return false
		}
		for _, post := range p.Posts {
			if !safeObjectID(post.ID) {
				return false
			}
			if instagram && edge == adCreativesEdge {
				creative := post.AdCreative
				if creative == nil || !safeObjectID(creative.ID) || (creative.EffectiveInstagramMediaID != "" && !safeObjectID(creative.EffectiveInstagramMediaID)) {
					return false
				}
				if media := post.InstagramMedia; media == nil {
					if post.ID != creative.ID {
						return false
					}
				} else if media.ID != post.ID || creative.EffectiveInstagramMediaID != post.ID || creative.AccountID != st.AdAccountID || media.Owner == nil || media.Owner.ID != identity.AccountID {
					return false
				}
			} else if instagram && (post.InstagramMedia == nil || post.InstagramMedia.ID != post.ID) {
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
