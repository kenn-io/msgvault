package slack

import (
	"encoding/json"
	"time"
)

// ConvState tracks one conversation's sync progress.
//
// Cursor is the max top-level message ts persisted; incremental history
// fetches oldest=Cursor. Backfill is the resumable oldest-ward walk toward
// the beginning of history: BackfillCursor is the page cursor to resume from
// and BackfillLatest pins the walk's upper bound so messages arriving during
// a long backfill cannot shift its pagination. Done means the backfill
// reached the beginning of history.
//
// Threads maps thread-root ts → max reply ts persisted for that root. Roots
// are tracked while younger than the run's thread lookback, then pruned;
// replies to pruned roots are only caught by --full runs (see
// docs/internal/slack-ingestion-design.md, LB-3).
type ConvState struct {
	Cursor         string            `json:"cursor,omitempty"`
	BackfillCursor string            `json:"backfill_cursor,omitempty"`
	BackfillLatest string            `json:"backfill_latest,omitempty"`
	Done           bool              `json:"done,omitempty"`
	Threads        map[string]string `json:"threads,omitempty"`
	// IncrCursor/IncrMaxTS checkpoint a partially-walked incremental window
	// (interrupted by --limit or a fetch error), so limited runs drain a
	// backlog across runs instead of restarting from the newest page. The
	// main Cursor still only advances once the window is exhausted.
	IncrCursor string `json:"incr_cursor,omitempty"`
	IncrMaxTS  string `json:"incr_max_ts,omitempty"`
}

// SyncState holds per-conversation cursors for one Slack source, persisted
// as JSON in sync_runs.cursor_after (checkpointed mid-run in cursor_before).
type SyncState struct {
	Conversations map[string]*ConvState `json:"conversations"` // key = channel ID
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
	if cs.Threads == nil {
		cs.Threads = map[string]string{}
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
		if ocs.Cursor != "" && (cs.Cursor == "" || tsLess(cs.Cursor, ocs.Cursor)) {
			cs.Cursor = ocs.Cursor
		}
		if ocs.BackfillCursor != "" {
			cs.BackfillCursor = ocs.BackfillCursor
		}
		if ocs.BackfillLatest != "" {
			cs.BackfillLatest = ocs.BackfillLatest
		}
		if ocs.IncrCursor != "" {
			cs.IncrCursor = ocs.IncrCursor
		}
		if ocs.IncrMaxTS != "" && (cs.IncrMaxTS == "" || tsLess(cs.IncrMaxTS, ocs.IncrMaxTS)) {
			cs.IncrMaxTS = ocs.IncrMaxTS
		}
		for root, replyTS := range ocs.Threads {
			if cur, ok := cs.Threads[root]; !ok || tsLess(cur, replyTS) {
				cs.Threads[root] = replyTS
			}
		}
		cs.Done = cs.Done || ocs.Done
	}
}

// TrackThread starts (or refreshes) reply tracking for a thread root.
// replyTS is the max reply ts already persisted ("" = none yet).
func (cs *ConvState) TrackThread(rootTS, replyTS string) {
	if cur, ok := cs.Threads[rootTS]; ok && (replyTS == "" || tsLess(replyTS, cur)) {
		return
	}
	cs.Threads[rootTS] = replyTS
}

// PruneThreads drops tracked roots older than cutoff, bounding both the
// per-sync conversations.replies call count and the checkpoint blob size.
// Only roots whose polling completed this run (polled) are eligible: pruning
// a root that was skipped (--limit) or whose fetch failed would permanently
// lose its replies to incremental sync.
func (cs *ConvState) PruneThreads(cutoff time.Time, polled map[string]bool) {
	for root := range cs.Threads {
		if !polled[root] {
			continue
		}
		if t := tsTime(root); !t.IsZero() && t.Before(cutoff) {
			delete(cs.Threads, root)
		}
	}
}
