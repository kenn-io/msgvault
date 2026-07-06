package beeper

import (
	"encoding/json"
	"time"
)

// ChatState tracks one chat's sync progress.
//
// Newest is the cursor to resume incremental fetches from (direction=after).
// Oldest is the backfill resume point (direction=before); it advances as the
// backfill walks toward the beginning of history. Done means the backfill
// reached the beginning of the chat's locally-available history.
type ChatState struct {
	Newest string `json:"newest,omitempty"`
	Oldest string `json:"oldest,omitempty"`
	Done   bool   `json:"done,omitempty"`
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
	// Anchor is set once after the first message is archived; see AnchorProbe.
	Anchor *AnchorProbe `json:"anchor,omitempty"`
	// ListWatermark is the max chat lastActivity observed (RFC3339); the next
	// incremental run enumerates only chats active after it.
	ListWatermark string `json:"list_watermark,omitempty"`
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
	b, err := json.Marshal(s)
	return string(b), err
}

// Chat returns the (created-if-missing) state for chatID.
func (s *SyncState) Chat(chatID string) *ChatState {
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
// more recent (checkpoint) run whose cursors are at least as advanced. Done
// flags are OR'd. The later ListWatermark wins (RFC3339 is order-comparable).
func (s *SyncState) Merge(other *SyncState) {
	if other == nil {
		return
	}
	for chatID, ocs := range other.Chats {
		if ocs == nil {
			continue
		}
		cs := s.Chat(chatID)
		if ocs.Newest != "" {
			cs.Newest = ocs.Newest
		}
		if ocs.Oldest != "" {
			cs.Oldest = ocs.Oldest
		}
		cs.Done = cs.Done || ocs.Done
	}
	if s.Anchor == nil {
		s.Anchor = other.Anchor
	}
	if other.ListWatermark > s.ListWatermark {
		s.ListWatermark = other.ListWatermark
	}
}
