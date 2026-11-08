package forge

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"
)

// stateVersion is the version of the resume state's shape.
const stateVersion = 1

// syncState is a source's resume state: how far discovery came and which
// threads it found but has not archived yet. It is each run's checkpoint and
// its completion cursor, so the next run resumes from the newest one, as
// Slack's sync does. A discovery cursor advances in the same checkpoint that
// queues the threads it found, so a thread found is never lost.
type syncState struct {
	Version int `json:"version"`
	// Owners is the owner filter the discovery cursor was reached under.
	Owners string `json:"owners,omitempty"`
	// Cursor is the forge's discovery state.
	Cursor jsontext.Value `json:"cursor,omitempty"`
	// Full reports a full sync's discovery in progress: until it completes,
	// every thread it finds is fetched, archived at that update or not.
	Full bool `json:"full,omitempty"`
	// Queue holds the threads found and not yet archived.
	Queue []queuedThread `json:"queue,omitempty"`
}

// queuedThread is a thread waiting to be archived, and where its fetch
// stopped when an earlier run archived part of it.
type queuedThread struct {
	Ref          ThreadRef      `json:"ref"`
	Continuation jsontext.Value `json:"continuation,omitempty"`
	// ReadAt is when the fetch read the thread's first page. A fetch that
	// pauses keeps it, since a change made since to a page already read is
	// not in the archive.
	ReadAt time.Time `json:"read_at,omitzero"`
}

func decodeState(blob string) (syncState, error) {
	var state syncState
	if blob == "" {
		return syncState{Version: stateVersion}, nil
	}
	if err := json.Unmarshal([]byte(blob), &state); err != nil {
		return syncState{}, fmt.Errorf("decode forge sync state: %w", err)
	}
	if state.Version != stateVersion {
		return syncState{}, fmt.Errorf("forge sync state version %d is not %d", state.Version, stateVersion)
	}
	return state, nil
}

func (s *syncState) encode() (string, error) {
	data, err := json.Marshal(s, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode forge sync state: %w", err)
	}
	return string(data), nil
}

// queue adds found threads. A thread already queued keeps the place its
// fetch reached unless it changed again, and keeps its notification reason
// unless a newer one arrives.
// The queue is newest first, so recent activity is archived before an old
// backlog. It returns how many threads were not queued before.
func (s *syncState) queue(found []ThreadRef) int {
	index := make(map[string]int, len(s.Queue))
	for i, queued := range s.Queue {
		index[queued.Ref.Key] = i
	}
	added := 0
	for _, ref := range found {
		i, ok := index[ref.Key]
		if !ok {
			index[ref.Key] = len(s.Queue)
			s.Queue = append(s.Queue, queuedThread{Ref: ref})
			added++
			continue
		}
		queued := &s.Queue[i]
		reason := queued.Ref.Reason
		if ref.UpdatedAt.After(queued.Ref.UpdatedAt) {
			// The thread changed again; fetch it from the start, since an
			// earlier post may be what changed.
			queued.Ref = ref
			queued.Continuation = nil
			queued.ReadAt = time.Time{}
		}
		if queued.Ref.Reason == "" {
			queued.Ref.Reason = reason
		}
		if ref.Reason != "" {
			queued.Ref.Reason = ref.Reason
		}
	}
	slices.SortStableFunc(s.Queue, func(a, b queuedThread) int {
		return b.Ref.UpdatedAt.Compare(a.Ref.UpdatedAt)
	})
	return added
}
