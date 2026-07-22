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
	// Floor is the entry's coverage claim: replies are owed strictly after
	// it ("" = from the root, the walks' full-drain claim; tail entries
	// carry their seed). The entry has covered (Floor, DrainedTo] and will
	// cover (Floor, ∞) when drained. Merges are decided against Floor, not
	// DrainedTo: a re-discovered hit above the Floor is already owed
	// (progress stands — rolling DrainedTo back would rewind the drain on
	// every overlapped sweep and never converge), while a hit at or below
	// it is genuinely outside the claim and widens the entry.
	Floor string `json:"floor,omitempty"`
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
	// ThreadsPending marks conversation-level thread debt: any initial
	// walk under --no-threads (unconditionally — a message can become a
	// thread root after the walk), or a non-channel conversation
	// recovering a sweep gap. The thread catch-up walk pays it
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
	// Legacy drain entries predate the Floor field. An in-flight entry
	// with progress but no floor is normalized to Floor = DrainedTo — the
	// safe misread in both directions: a legacy TAIL read as full coverage
	// would silently skip replies below its seed, while a legacy FULL walk
	// entry read as a tail merely widens down and re-fetches an idempotent
	// stretch if a hit surfaces beneath its progress.
	for _, cs := range s.Conversations {
		for i := range cs.PendingThreads {
			if cs.PendingThreads[i].Floor == "" && cs.PendingThreads[i].DrainedTo != "" {
				cs.PendingThreads[i].Floor = cs.PendingThreads[i].DrainedTo
			}
		}
	}
	return s, nil
}

func (s *SyncState) Marshal() (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

// RecordPendingThread commits a thread to the drain debt as FULL coverage:
// every reply from the root onward (Floor = ""). An existing entry with the
// same claim keeps its progress — that progress is contiguous from the root
// (a page refetched after a failure re-discovers its roots harmlessly). An
// existing TAIL entry cannot satisfy the claim: its parked DrainedTo proves
// nothing below its own seed, and keeping it would silently skip every
// reply under that seed while the caller (a walk or catch-up) certifies
// the thread covered. Such an entry WIDENS: progress resets to the root
// and the fresh forecast is taken. The reset re-fetches the tail's stretch
// into idempotent upserts, at most once — the widened entry answers every
// later record as full coverage.
func (cs *ConvState) RecordPendingThread(rootTS string, forecast int) {
	for i := range cs.PendingThreads {
		if cs.PendingThreads[i].RootTS == rootTS {
			if cs.PendingThreads[i].Floor != "" {
				cs.PendingThreads[i].Floor = ""
				cs.PendingThreads[i].DrainedTo = ""
				cs.PendingThreads[i].Forecast = forecast
			}
			return
		}
	}
	cs.PendingThreads = append(cs.PendingThreads, PendingThread{RootTS: rootTS, Forecast: forecast})
}

// RecordPendingThreadTail records a thread whose replies are owed from
// drainedTo (exclusive) onward — the reply sweep's shape: anchorTS is any
// in-thread ts (conversations.replies resolves it) and drainedTo sits just
// before the earliest discovered hit. The merge with an existing entry is
// decided against the entry's coverage Floor, NOT its progress:
//   - seed at/above the Floor: the hit is already inside the entry's claim
//     (fetched, or ahead of the resume point) — the entry stands. Sweep
//     floors overlap, so live runs re-discover the SAME hits every pass;
//     re-seeding progress from them would rewind the drain each run and a
//     long tail would never converge.
//   - seed below the Floor: a late-indexed reply surfaced beneath the
//     claim; the day's certification is already advancing past it, so this
//     merge is its last chance — the entry widens down (Floor and resume
//     point drop to the seed). The re-fetch of the already-drained stretch
//     above is an idempotent upsert.
func (cs *ConvState) RecordPendingThreadTail(anchorTS, drainedTo string) {
	for i := range cs.PendingThreads {
		if cs.PendingThreads[i].RootTS == anchorTS {
			if cs.PendingThreads[i].Floor != "" && tsLess(drainedTo, cs.PendingThreads[i].Floor) {
				cs.PendingThreads[i].Floor = drainedTo
				cs.PendingThreads[i].DrainedTo = drainedTo
			}
			return
		}
	}
	cs.PendingThreads = append(cs.PendingThreads, PendingThread{RootTS: anchorTS, DrainedTo: drainedTo, Floor: drainedTo})
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
