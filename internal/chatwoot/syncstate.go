package chatwoot

import (
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"time"
)

const stateVersion = 1

// Both bounds are necessary: Chatwoot orders pages by creation time, while
// bounds filter numeric IDs. A short page never proves that a range is complete.
type idRange struct {
	After  int64 `json:"after"`
	Before int64 `json:"before"`
}

type conversationState struct {
	Pending      []idRange       `json:"pending"`
	HighWater    int64           `json:"high_water"`
	ReconciledAt time.Time       `json:"reconciled_at"`
	WalkingFull  bool            `json:"walking_full"`
	Artifacts    map[string]bool `json:"artifacts"`
	NextArtifact string          `json:"next_artifact"`
}

type syncState struct {
	Version               int                           `json:"version"`
	Scope                 string                        `json:"scope"`
	Conversations         map[string]*conversationState `json:"conversations"`
	FullReconciliation    bool                          `json:"full_reconciliation,omitempty"`
	NextConversation      int64                         `json:"next_conversation"`
	NextPage              int                           `json:"next_page"`
	NextSavedConversation string                        `json:"next_saved_conversation,omitempty"`
}

func newSyncState(scope string) *syncState {
	return &syncState{Version: stateVersion, Scope: scope, Conversations: map[string]*conversationState{}}
}

func parseSyncState(blob, scope string) (*syncState, error) {
	s := newSyncState(scope)
	if blob == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(blob), s); err != nil {
		return nil, errors.New("invalid Chatwoot sync checkpoint")
	}
	if s.Version != stateVersion || s.Scope != scope || s.Conversations == nil || s.NextPage < 0 {
		return nil, errors.New("chatwoot checkpoint scope or version mismatch")
	}
	for _, cs := range s.Conversations {
		if cs == nil || cs.HighWater < 0 || cs.HighWater > math.MaxInt32 {
			return nil, errors.New("invalid Chatwoot conversation checkpoint")
		}
		ordered := slices.Clone(cs.Pending)
		slices.SortFunc(ordered, func(a, b idRange) int {
			if a.After < b.After {
				return -1
			}
			if a.After > b.After {
				return 1
			}
			return 0
		})
		for idx, r := range ordered {
			if r.After <= 0 || r.After >= r.Before || (idx > 0 && ordered[idx-1].Before > r.After) {
				return nil, errors.New("invalid Chatwoot pending ranges")
			}
		}
	}
	return s, nil
}

func (s *syncState) marshal() (string, error) {
	b, err := json.Marshal(s, json.Deterministic(true))
	return string(b), err
}

// Subtract only IDs whose complete archival write has succeeded. Retaining
// every gap also catches records backdated beneath the largest returned ID.
func subtractHandled(r idRange, ids []int64) ([]idRange, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var gaps []idRange
	start := r.After
	for _, id := range ids {
		if id < r.After || id >= r.Before || id > math.MaxInt32 {
			return nil, errors.New("chatwoot API did not honor combined message bounds")
		}
		if start < id {
			gaps = append(gaps, idRange{start, id})
		}
		start = id + 1
	}
	if start < r.Before {
		gaps = append(gaps, idRange{start, r.Before})
	}
	return gaps, nil
}
