package slack

import (
	"encoding/json"
)

// ConvState tracks one conversation's sync progress.
//
// Cursor is the max top-level message ts persisted; incremental history
// fetches oldest=Cursor. Backfill is the resumable oldest-ward walk toward
// the beginning of history: BackfillCursor is the page cursor to resume from
// and BackfillLatest pins the walk's upper bound so messages arriving during
// a long backfill cannot shift its pagination. Done means the backfill
// reached the beginning of history.
type ConvState struct {
	Cursor         string `json:"cursor,omitempty"`
	BackfillCursor string `json:"backfill_cursor,omitempty"`
	BackfillLatest string `json:"backfill_latest,omitempty"`
	Done           bool   `json:"done,omitempty"`
	// IncrCursor/IncrMaxTS checkpoint a partially-walked incremental window
	// (interrupted by --limit or a fetch error), so limited runs drain a
	// backlog across runs instead of restarting from the newest page. The
	// main Cursor still only advances once the window is exhausted.
	IncrCursor string `json:"incr_cursor,omitempty"`
	IncrMaxTS  string `json:"incr_max_ts,omitempty"`
	// ThreadsPending marks a backfill that consumed pages under --no-threads:
	// their roots' replies were never inline-fetched, and the sweep floor
	// (the backfill pin) postdates them. The next threaded run performs a
	// thread catch-up walk and clears the flag.
	ThreadsPending bool `json:"threads_pending,omitempty"`
	// SweptThrough is this conversation's reply-certification boundary
	// (UTC ts): every thread reply created at or before it has been
	// archived. It normally tracks the workspace SweepWatermark; it lags
	// when the conversation missed sweeps (excluded, gone, or filtered
	// while the watermark advanced), which the next sweep repairs with a
	// channel-scoped gap sweep before stamping it forward.
	SweptThrough string `json:"swept_through,omitempty"`
}

// SyncState holds per-conversation cursors plus the reply-sweep watermark
// for one Slack source, persisted as JSON in sync_runs.cursor_after
// (checkpointed mid-run in cursor_before). Legacy blobs carrying per-root
// "threads" maps load cleanly; the field no longer exists.
type SyncState struct {
	Conversations map[string]*ConvState `json:"conversations"` // key = channel ID
	// SweepWatermark is a UTC ts: the reply-sweep certification boundary
	// for the current target set as a whole (each conversation's own
	// boundary is its SweptThrough, which lags for conversations that
	// missed sweeps — see docs/internal/slack-reply-sweep-design.md). It
	// derives only from fully-searched intervals, advances only behind
	// persisted work, and never passes now − lag margin.
	SweepWatermark string `json:"sweep_watermark,omitempty"`
	// SweepOffset records the user tz_offset (seconds) in effect when the
	// watermark was written. Audit trail only: sweep-day arithmetic always
	// uses the offset current at query time, because search date modifiers
	// are evaluated in the user's CURRENT profile timezone (probed live).
	SweepOffset int `json:"sweep_offset,omitempty"`
}

func NewSyncState() *SyncState {
	return &SyncState{Conversations: map[string]*ConvState{}}
}

func LoadSyncState(blob string) (*SyncState, error) {
	s := NewSyncState()
	if blob == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(blob), s); err != nil {
		return nil, err
	}
	if s.Conversations == nil {
		s.Conversations = map[string]*ConvState{}
	}
	return s, nil
}

func (s *SyncState) Marshal() (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

// EnsureConv returns the (created-if-missing) state for channelID.
func (s *SyncState) EnsureConv(channelID string) *ConvState {
	cs, ok := s.Conversations[channelID]
	if !ok {
		cs = &ConvState{}
		s.Conversations[channelID] = cs
	}
	return cs
}

// Merge incorporates cursors from other (a newer checkpoint) into s. Message
// ts cursors compare numerically; the more-advanced value wins. Backfill page
// cursors are opaque, so other's non-empty value wins wholesale (Teams
// deltaLink precedent). Done flags are OR'd.
func (s *SyncState) Merge(other *SyncState) {
	if other == nil {
		return
	}
	for channelID, ocs := range other.Conversations {
		if ocs == nil {
			continue
		}
		cs := s.EnsureConv(channelID)
		// Cursor and IncrCursor/IncrMaxTS form ONE resume unit: a window page
		// cursor is only valid relative to the oldest=Cursor bound it was
		// minted under. The more-advanced unit wins wholesale — including
		// cleared fields, so a completed window's clear is authoritative and
		// a stale checkpoint's mid-window state never pairs with a newer
		// cursor.
		switch {
		case ocs.Cursor != "" && (cs.Cursor == "" || tsLess(cs.Cursor, ocs.Cursor)):
			cs.Cursor = ocs.Cursor
			cs.IncrCursor = ocs.IncrCursor
			cs.IncrMaxTS = ocs.IncrMaxTS
		case ocs.Cursor == cs.Cursor:
			if ocs.IncrCursor != "" {
				cs.IncrCursor = ocs.IncrCursor
			}
			if ocs.IncrMaxTS != "" && (cs.IncrMaxTS == "" || tsLess(cs.IncrMaxTS, ocs.IncrMaxTS)) {
				cs.IncrMaxTS = ocs.IncrMaxTS
			}
		}
		if ocs.BackfillCursor != "" {
			cs.BackfillCursor = ocs.BackfillCursor
		}
		if ocs.BackfillLatest != "" {
			cs.BackfillLatest = ocs.BackfillLatest
		}
		cs.Done = cs.Done || ocs.Done
		cs.ThreadsPending = cs.ThreadsPending || ocs.ThreadsPending
		if ocs.SweptThrough != "" && (cs.SweptThrough == "" || tsLess(cs.SweptThrough, ocs.SweptThrough)) {
			cs.SweptThrough = ocs.SweptThrough
		}
	}
	// The further-advanced watermark wins, carrying its audit offset with it
	// (the pair is one unit, like the incremental cursor group).
	if other.SweepWatermark != "" && (s.SweepWatermark == "" || tsLess(s.SweepWatermark, other.SweepWatermark)) {
		s.SweepWatermark = other.SweepWatermark
		s.SweepOffset = other.SweepOffset
	}
}
