package slack

import (
	"encoding/json"
)

// PendingThread is one thread whose replies a window walk still owes: the
// containing history page was consumed (and the page cursor advanced), but
// the reply fetch was deferred or clipped by the --limit budget.
type PendingThread struct {
	RootTS string `json:"root"`
	// DrainedTo is the newest reply ts fetched so far ("" = not started):
	// the drain resumes with oldest=DrainedTo (exclusive), a self-validating
	// ts bound rather than an opaque page cursor.
	DrainedTo string `json:"drained_to,omitempty"`
	// Forecast is the remaining reply_count estimate, charged against the
	// run budget at recording time and converted to actuals as the drain
	// fetches replies. Pacing and progress only — never a completeness
	// signal (counts can be stale).
	Forecast int `json:"forecast,omitempty"`
}

// ConvState tracks one conversation's sync progress.
//
// Every fetch of top-level history is a pinned WINDOW WALK — the initial
// backfill walks ("", pin]; every later (incremental) walk covers
// (Cursor, pin]. Cursor is therefore a covered-through bound: top-level
// messages at or before it are persisted and their threads' replies (as of
// each walk) drained. BackfillLatest pins an in-flight walk's upper bound
// (arrivals must not shift the pinned newest-first pagination) and
// BackfillCursor is its resumable page cursor; both clear when the walk
// completes and Cursor advances to the pin. Done means the initial walk
// reached the beginning of history.
type ConvState struct {
	Cursor         string `json:"cursor,omitempty"`
	BackfillCursor string `json:"backfill_cursor,omitempty"`
	BackfillLatest string `json:"backfill_latest,omitempty"`
	Done           bool   `json:"done,omitempty"`
	// ThreadsPending marks conversation-level thread debt: an initial
	// backfill that consumed pages under --no-threads, or a non-channel
	// conversation recovering a sweep gap. The thread catch-up walk pays it
	// and clears the flag when the walk finishes; CatchUpCursor/
	// CatchUpLatest checkpoint a partially-walked catch-up (the page cursor
	// to resume from and the pin the walk was started under — page cursors
	// are only valid against the bound they were minted with), so limited
	// runs drain the walk across runs instead of restarting it.
	ThreadsPending bool   `json:"threads_pending,omitempty"`
	CatchUpCursor  string `json:"catch_up_cursor,omitempty"`
	CatchUpLatest  string `json:"catch_up_latest,omitempty"`
	// PendingThreads is the window walks' outstanding thread-drain debt,
	// bounded by one history page's roots: a walk never fetches a new
	// page while any entry is outstanding. Drained head-first; an entry
	// leaves the list only when its thread is fully fetched (or the
	// thread is gone).
	PendingThreads []PendingThread `json:"pending_threads,omitempty"`
	// SweptThrough is this conversation's reply-sweep boundary: the pin of
	// the last sweep that covered it (replies created at or before
	// SweptThrough − the lag margin are certainly archived; the margin is
	// re-covered by the next sweep's overlapped floor, absorbing search
	// index lag). It normally tracks the workspace SweepWatermark; it lags
	// when the conversation missed sweeps (excluded, gone, or filtered
	// while the watermark advanced), which the next sweep repairs with a
	// channel-scoped gap sweep before stamping it forward.
	SweptThrough string `json:"swept_through,omitempty"`
}

// SyncState holds per-conversation cursors plus the reply-sweep watermark
// for one Slack source, persisted as JSON in sync_runs.cursor_after
// (checkpointed mid-run in cursor_before). Resume selection is newest-blob-
// wins — states are never blended field-wise (see loadResumeState). Legacy
// blobs carrying per-root "threads" maps, incremental window cursors
// ("incr_cursor"/"incr_max_ts"), or a "generation" marker load cleanly;
// those fields no longer exist (an upgraded mid-window checkpoint re-walks
// at most one window into idempotent upserts).
type SyncState struct {
	Conversations map[string]*ConvState `json:"conversations"` // key = channel ID
	// SweepWatermark is the pin of the last completed workspace sweep for
	// the current target set (each conversation's own boundary is its
	// SweptThrough). Replies created at or before it minus the lag margin
	// are certainly archived; the trailing margin is re-covered by the next
	// sweep's overlapped floor. It advances only behind persisted work.
	SweepWatermark string `json:"sweep_watermark,omitempty"`
	// SweepOffset records the user tz_offset (seconds) in effect when the
	// watermark was written. Audit trail only: sweep-day arithmetic always
	// uses the IANA zone current at query time (probed live).
	SweepOffset int `json:"sweep_offset,omitempty"`
	// RepairPending marks an in-flight --full repair session. While set,
	// every run — full, plain, or limited — continues the repair through
	// the ordinary resumable walks; it clears when every eligible
	// conversation is Done with all thread debt paid. The --full reset it
	// rides on needs no lineage marker: resume selection is newest-blob-
	// wins (see loadResumeState), so the reset state simply supersedes.
	RepairPending bool `json:"repair_pending,omitempty"`
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

// RecordPendingThread appends a thread to the drain debt unless it is
// already recorded (a page refetched after a failure re-discovers its
// roots; the existing entry keeps its drain progress).
func (cs *ConvState) RecordPendingThread(rootTS string, forecast int) {
	for i := range cs.PendingThreads {
		if cs.PendingThreads[i].RootTS == rootTS {
			return
		}
	}
	cs.PendingThreads = append(cs.PendingThreads, PendingThread{RootTS: rootTS, Forecast: forecast})
}

// RecordPendingThreadTail records a thread whose replies are owed only from
// drainedTo (exclusive) onward — the reply sweep's shape: anchorTS is any
// in-thread ts (conversations.replies resolves it) and drainedTo sits just
// before the earliest discovered hit. An existing entry for the same anchor
// keeps its own drain progress (it covers at least as much).
func (cs *ConvState) RecordPendingThreadTail(anchorTS, drainedTo string) {
	for i := range cs.PendingThreads {
		if cs.PendingThreads[i].RootTS == anchorTS {
			return
		}
	}
	cs.PendingThreads = append(cs.PendingThreads, PendingThread{RootTS: anchorTS, DrainedTo: drainedTo})
}

// PendingForecast sums the remaining reply forecasts of the outstanding
// drain debt: the budget charge for work committed but not yet performed.
func (cs *ConvState) PendingForecast() int {
	n := 0
	for i := range cs.PendingThreads {
		n += cs.PendingThreads[i].Forecast
	}
	return n
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

// RepairComplete reports whether an in-flight repair session has finished:
// every ELIGIBLE conversation's initial walk is done and all its thread
// debt is paid. Completion is a question about the currently eligible set
// (the conversations this run enumerated), not the historical one: state is
// deliberately retained for departed/excluded conversations (it powers gap
// recovery and re-entry resume), and a conversation the repair can no
// longer reach must not wedge the session open forever — its generation-
// reset Done flag already guarantees a fresh walk if it ever re-enters.
func (s *SyncState) RepairComplete(eligible map[string]bool) bool {
	for cid := range eligible {
		cs, ok := s.Conversations[cid]
		if !ok || !cs.Done || cs.ThreadsPending || len(cs.PendingThreads) > 0 {
			return false
		}
	}
	return true
}
