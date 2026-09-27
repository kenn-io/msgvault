package beeper

import (
	"encoding/json/v2"
	"time"
)

// ChatState tracks one chat's sync progress.
//
// Newest is the cursor to resume incremental fetches from (direction=after).
// Oldest is the backfill resume point (direction=before); it advances as the
// backfill walks toward the beginning of history. Done means the backfill
// reached the beginning of the chat's locally-available history.
type ChatState struct {
	// Visited records completed work in the current discovery cycle. A
	// budget-limited run resumes at unvisited chats before starting a new cycle.
	Visited bool   `json:"visited,omitzero"`
	Newest  string `json:"newest,omitempty"`
	Oldest  string `json:"oldest,omitempty"`
	// TailProbeCursor resumes a budget-limited history probe at the page
	// boundary it reached, rather than spending every run re-reading the same
	// oldest pages.
	TailProbeCursor string `json:"tail_probe_cursor,omitempty"`
	Done            bool   `json:"done,omitzero"`
	// PendingReplies holds [child, parent] source-message-ID pairs whose
	// parents were not yet archived when the walk stopped (backfill walks
	// newest→oldest); they are linked once the backfill completes.
	PendingReplies [][2]string `json:"pending_replies,omitempty"`
	// TailProbed is the tail-scan cycle (SyncState.TailScanStarted) that last
	// visited this chat, so a budget-limited scan resumes instead of
	// re-probing the same chats every run.
	TailProbed string `json:"tail_probed,omitempty"`
}

// AnchorProbe fingerprints the Beeper installation's message-ID space.
// Message IDs are only unique per installation; a reinstall or re-index could
// re-assign them, which would silently corrupt incremental dedup. Each run
// re-fetches the anchor message and aborts on mismatch instead.
type AnchorProbe struct {
	ChatID    string    `json:"chat_id"`
	MessageID string    `json:"message_id"`
	Timestamp time.Time `json:"timestamp"`
}

// SyncState holds per-chat incremental cursors for one Beeper account source,
// persisted as JSON in sync_runs.cursor_after (and checkpointed mid-run in
// cursor_before).
type SyncState struct {
	Chats map[string]*ChatState `json:"chats"` // key = chat ID (Matrix room ID)
	// Anchors fingerprint the installation across several distinct chats (see
	// AnchorProbe): ordinary churn can delete any one anchor's chat, so a
	// reinstall is only concluded when no anchor survives.
	Anchors []AnchorProbe `json:"anchors,omitempty"`
	// ListWatermark is the max chat lastActivity observed (RFC3339); the next
	// incremental run enumerates only chats active after it.
	ListWatermark string `json:"list_watermark,omitempty"`
	// CycleWatermark freezes the discovery boundary while Visited chats wait
	// for the remaining chats in a budget-limited cycle.
	CycleWatermark string `json:"cycle_watermark,omitempty"`
	// LastTailScan is when this source last re-probed completed chats for
	// history Beeper backfilled after they were marked done (RFC3339). See
	// tailScanInterval.
	LastTailScan string `json:"last_tail_scan,omitempty"`
	// TailScanStarted identifies the tail scan in progress (RFC3339 start).
	// It is cleared when a run finishes the scan.
	TailScanStarted string `json:"tail_scan_started,omitempty"`
}

func NewSyncState() *SyncState {
	return &SyncState{Chats: map[string]*ChatState{}}
}

func LoadSyncState(blob string) (*SyncState, error) {
	s := NewSyncState()
	if blob == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(blob), s); err != nil {
		return nil, err
	}
	if s.Chats == nil {
		s.Chats = map[string]*ChatState{}
	}
	return s, nil
}

func (s *SyncState) Marshal() (string, error) {
	b, err := json.Marshal(s, json.Deterministic(true))
	return string(b), err
}

// EnsureChat returns the (created-if-missing) state for chatID.
func (s *SyncState) EnsureChat(chatID string) *ChatState {
	cs, ok := s.Chats[chatID]
	if !ok {
		cs = &ChatState{}
		s.Chats[chatID] = cs
	}
	return cs
}

// Merge incorporates cursors from other into s. Cursors are opaque API tokens
// that cannot be compared by value; like the Teams deltaLink merge, we prefer
// other's non-empty values wholesale on the assumption that other represents a
// more recent (checkpoint) run whose cursors are at least as advanced. Tail
// scan cursors are authoritative in a checkpoint because an empty value clears
// a completed probe. Checkpoint Done values are authoritative so a reopened
// chat can clear a previous completion. The later ListWatermark wins
// (RFC3339 is order-comparable).
func (s *SyncState) Merge(other *SyncState) {
	if other == nil {
		return
	}
	for chatID, ocs := range other.Chats {
		if ocs == nil {
			continue
		}
		cs := s.EnsureChat(chatID)
		if ocs.Newest != "" {
			cs.Newest = ocs.Newest
		}
		if ocs.Oldest != "" {
			cs.Oldest = ocs.Oldest
		}
		cs.Visited = ocs.Visited
		cs.TailProbeCursor = ocs.TailProbeCursor
		if len(ocs.PendingReplies) > 0 {
			cs.PendingReplies = ocs.PendingReplies
		}
		cs.Done = ocs.Done
		if ocs.TailProbed > cs.TailProbed {
			cs.TailProbed = ocs.TailProbed
		}
	}
	if len(s.Anchors) == 0 {
		s.Anchors = other.Anchors
	}
	if other.ListWatermark > s.ListWatermark {
		s.ListWatermark = other.ListWatermark
	}
	if other.LastTailScan > s.LastTailScan {
		s.LastTailScan = other.LastTailScan
	}
	s.CycleWatermark = other.CycleWatermark
	s.TailScanStarted = other.TailScanStarted
}
