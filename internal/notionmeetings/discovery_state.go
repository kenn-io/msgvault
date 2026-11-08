package notionmeetings

import (
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

type discoveryLimits struct {
	Requests, Attempts, Depth, Scopes, Pending, Frontier, Bytes, Diagnostics, KnownRefresh int
	Duration                                                                               time.Duration
}

func defaultDiscoveryLimits() discoveryLimits {
	return discoveryLimits{Requests: 256, Attempts: 512, Depth: 32, Scopes: 10000, Pending: 200, Frontier: 1000, Bytes: 2 << 20, Diagnostics: 100, KnownRefresh: 32, Duration: 2 * time.Minute}
}

type scopeTask struct {
	Kind    string   `json:"kind"`
	ID      string   `json:"id,omitempty"`
	Cursor  string   `json:"cursor,omitempty"`
	Depth   int      `json:"depth"`
	Offset  int      `json:"offset,omitempty"`
	Cursors []string `json:"cursors,omitempty"`
}

func (t scopeTask) key() string { return t.Kind + ":" + t.ID }

type deferredScope struct {
	Task          *scopeTask   `json:"task,omitempty"`
	Window        *queryWindow `json:"window,omitempty"`
	Reason        string       `json:"reason"`
	NextAttemptAt time.Time    `json:"next_attempt_at"`
}
type coverageDiagnostic struct {
	Reason string `json:"reason"`
	Kind   string `json:"kind,omitempty"`
}

func newSyncState() syncState {
	return syncState{Version: syncStateVersion, Known: map[string]knownMeeting{}, TranscriptRetryUntil: map[string]string{}, Seen: map[string]bool{}, Coverage: Coverage{Reasons: map[string]int{}}}
}
func (state *syncState) initialize() {
	state.Version = syncStateVersion
	if state.Known == nil {
		state.Known = map[string]knownMeeting{}
	}
	if state.TranscriptRetryUntil == nil {
		state.TranscriptRetryUntil = map[string]string{}
	}
	for _, pending := range append(append([]pendingTranscript(nil), state.Pending...), state.PendingBacklog...) {
		if retained := state.TranscriptRetryUntil[pending.BlockID]; retained == "" || retained < pending.RetryUntil {
			state.TranscriptRetryUntil[pending.BlockID] = pending.RetryUntil
		}
	}
	if state.Seen == nil {
		state.Seen = map[string]bool{}
	}
	if state.Coverage.Reasons == nil {
		state.Coverage.Reasons = map[string]int{}
	}
}

func (state *syncState) schedulePendingBacklog(now time.Time, limit int) {
	pending := append(append([]pendingTranscript(nil), state.Pending...), state.PendingBacklog...)
	pending = slices.DeleteFunc(pending, func(item pendingTranscript) bool { return pendingExpired(item, now) })
	sort.Slice(pending, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339, pending[i].NextAttemptAt)
		right, _ := time.Parse(time.RFC3339, pending[j].NextAttemptAt)
		if left.Equal(right) {
			return pending[i].BlockID < pending[j].BlockID
		}
		return left.Before(right)
	})
	active := min(len(pending), limit)
	state.Pending = pending[:active:active]
	state.PendingBacklog = pending[active:]
}
func normalizedIDs(ids []string) []string {
	set := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			set[id] = true
		}
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
func searchEnabled(opts ImportOptions) bool {
	return opts.SearchAccessible == nil || *opts.SearchAccessible
}
func scopeFingerprint(source string, identity Identity, opts ImportOptions) string {
	value := struct {
		Source                       string
		Identity                     Identity
		Mode                         string
		Pages, DataSources, Meetings []string
		Search                       bool
		After                        time.Time
	}{source, identity, strings.ToLower(strings.TrimSpace(opts.TokenType)), normalizedIDs(opts.RootPageIDs), normalizedIDs(opts.RootDataSourceIDs), normalizedIDs(opts.MeetingBlockIDs), searchEnabled(opts), opts.CreatedAfter.UTC()}
	raw, _ := json.Marshal(value, json.Deterministic(true))
	return checksumBytes(raw)
}
func (state *syncState) startSweep(opts ImportOptions, now time.Time) {
	state.Active = true
	state.FreshRequested = false
	state.Seen = map[string]bool{}
	state.Windows = []queryWindow{{To: now.UTC().Truncate(time.Minute).Add(time.Minute)}}
	for _, id := range normalizedIDs(opts.RootPageIDs) {
		state.Frontier = append(state.Frontier, scopeTask{Kind: "blocks", ID: id})
		state.Seen["blocks:"+id] = true
	}
	for _, id := range normalizedIDs(opts.RootDataSourceIDs) {
		state.Frontier = append(state.Frontier, scopeTask{Kind: "data_source", ID: id})
		state.Seen["data_source:"+id] = true
	}
	if searchEnabled(opts) {
		state.Frontier = append(state.Frontier, scopeTask{Kind: "search"})
		state.Seen["search:"] = true
		state.reason("search_best_effort", "search")
	}
	// Explicit meeting IDs are admitted by the scheduler in bounded batches, not
	// appended here beyond the pending cap.
	for _, id := range normalizedIDs(opts.MeetingBlockIDs) {
		state.Frontier = append(state.Frontier, scopeTask{Kind: "meeting", ID: id})
		state.Seen["meeting:"+id] = true
	}
}
func (state *syncState) reason(reason, kind string) {
	if state.Coverage.Reasons == nil {
		state.Coverage.Reasons = map[string]int{}
	}
	state.Coverage.Reasons[reason]++
	state.Coverage.DiagnosticCount++
	if len(state.Coverage.Diagnostics) < 100 {
		state.Coverage.Diagnostics = append(state.Coverage.Diagnostics, coverageDiagnostic{Reason: reason, Kind: kind})
	}
}
func (state *syncState) deferTask(t scopeTask, reason string, now time.Time) {
	state.reason(reason, t.Kind)
	for i, gap := range state.Deferred {
		if gap.Task != nil && gap.Task.key() == t.key() && gap.Task.Cursor == t.Cursor && gap.Task.Offset == t.Offset {
			state.Deferred[i].Reason = reason
			state.Deferred[i].NextAttemptAt = now.Add(transcriptRetryCadence)
			return
		}
	}
	state.Deferred = append(state.Deferred, deferredScope{Task: &t, Reason: reason, NextAttemptAt: now.Add(transcriptRetryCadence)})
}
func (state *syncState) activateDeferred(now time.Time, limits discoveryLimits) {
	retained := make([]deferredScope, 0, len(state.Deferred))
	var displaced *scopeTask
	for _, gap := range state.Deferred {
		if gap.NextAttemptAt.After(now) {
			retained = append(retained, gap)
			continue
		}
		if gap.Task != nil && len(state.Frontier) < limits.Frontier {
			state.Frontier = append(state.Frontier, *gap.Task)
		} else if gap.Task != nil && displaced == nil && len(state.Frontier) > 0 {
			// Reserve one slot for due continuation work even when roots fill the frontier.
			last := state.Frontier[len(state.Frontier)-1]
			displaced = &last
			state.Frontier = append([]scopeTask{*gap.Task}, state.Frontier[:len(state.Frontier)-1]...)
		} else if gap.Window != nil && len(state.Windows) < limits.Frontier {
			state.Windows = append(state.Windows, *gap.Window)
		} else {
			retained = append(retained, gap)
		}
	}
	state.Deferred = retained
	if displaced != nil {
		state.deferTask(*displaced, "frontier_budget", now)
	}
}
func (state *syncState) validateTransient() error {
	s := *state
	return s.validateTransientWith(defaultDiscoveryLimits())
}
func (state *syncState) validateTransientWith(limits discoveryLimits) error {
	s := *state
	if len(s.Admissions) > limits.Pending || len(s.Frontier) > limits.Frontier || len(s.Seen) > limits.Scopes || len(s.Deferred) > limits.Scopes || len(s.Windows) > limits.Frontier || len(s.Pending) > limits.Pending || len(s.Coverage.Diagnostics) > limits.Diagnostics {
		return errors.New("notion discovery cursor exceeds queue limits")
	}
	s.Known = nil
	s.TranscriptRetryUntil = nil
	s.PendingBacklog = nil
	encoded, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(encoded) > limits.Bytes {
		return errors.New("notion discovery cursor exceeds transient byte limit")
	}
	return nil
}
func (state *syncState) workRemaining() int {
	s := *state
	return len(s.Admissions) + len(s.Frontier) + len(s.Windows) + len(s.Pending) + len(s.PendingBacklog) + len(s.Deferred)
}

// copyDiscovery isolates a provider page until all its work fits a durable cursor.
func (state *syncState) copyDiscovery() syncState {
	s := *state
	s.Admissions = append([]string(nil), s.Admissions...)
	s.Frontier = append([]scopeTask(nil), s.Frontier...)
	s.Windows = append([]queryWindow(nil), s.Windows...)
	s.Deferred = append([]deferredScope(nil), s.Deferred...)
	s.Seen = maps.Clone(s.Seen)
	s.Coverage.Reasons = maps.Clone(s.Coverage.Reasons)
	s.Coverage.Diagnostics = append([]coverageDiagnostic(nil), s.Coverage.Diagnostics...)
	return s
}
func validateScopeTask(task scopeTask) error {
	if task.Depth < 0 || task.Offset < 0 {
		return errors.New("notion cursor has negative scope position")
	}
	switch task.Kind {
	case "search":
	case "blocks", "data_source", "database", "meeting":
		if strings.TrimSpace(task.ID) == "" {
			return errors.New("notion cursor has blank scope ID")
		}
	default:
		return errors.New("notion cursor has unsupported scope kind")
	}
	return nil
}
func validateWindow(window queryWindow) error {
	if (!window.From.IsZero() && !window.From.Equal(window.From.Truncate(time.Minute))) || (!window.To.IsZero() && !window.To.Equal(window.To.Truncate(time.Minute))) {
		return errors.New("notion cursor query bounds must use minute precision")
	}
	if !window.From.IsZero() && !window.To.IsZero() && !window.To.After(window.From) {
		return errors.New("notion cursor has invalid query interval")
	}
	return nil
}
func (state *syncState) validateDiscovery() error {
	s := *state
	if s.NextLane < 0 || s.NextLane > 3 {
		return errors.New("notion cursor has invalid scheduler lane")
	}
	ids := map[string]bool{}
	for _, id := range s.Admissions {
		if strings.TrimSpace(id) == "" || ids[id] {
			return errors.New("notion cursor has blank or duplicate admission")
		}
		ids[id] = true
	}
	for _, task := range s.Frontier {
		if err := validateScopeTask(task); err != nil {
			return err
		}
	}
	for _, window := range s.Windows {
		if err := validateWindow(window); err != nil {
			return err
		}
	}
	for _, gap := range s.Deferred {
		if (gap.Task == nil) == (gap.Window == nil) || gap.NextAttemptAt.IsZero() || gap.Reason == "" {
			return errors.New("notion cursor has invalid deferred scope")
		}
		if gap.Task != nil {
			if err := validateScopeTask(*gap.Task); err != nil {
				return err
			}
		} else if err := validateWindow(*gap.Window); err != nil {
			return err
		}
	}
	return s.validateTransient()
}
func validateDiscoveryOptions(opts ImportOptions) error {
	s := newSyncState()
	s.startSweep(opts, time.Now().UTC())
	return s.validateTransient()
}

func (state *syncState) resolveTask(task scopeTask) {
	retained := state.Deferred[:0]
	for _, gap := range state.Deferred {
		if gap.Task != nil && gap.Task.key() == task.key() && gap.Task.Cursor == task.Cursor && gap.Task.Offset == task.Offset {
			continue
		}
		retained = append(retained, gap)
	}
	state.Deferred = retained
}

// Reasons describe current debt; DiagnosticCount and Diagnostics retain history.
func (state *syncState) currentCoverage(search bool) {
	current := map[string]int{}
	for _, gap := range state.Deferred {
		current[gap.Reason]++
	}
	for _, known := range state.Known {
		if known.FetchError != "" && known.FetchError != "not_selected" {
			current[known.FetchError]++
		}
	}
	if search {
		current["search_best_effort"] = 1
	}
	for _, reason := range []string{"request_budget", "discovery_budget", "unfinished_progress", "cursor_budget", "time_budget", "interrupted", "users_unavailable", "pending_budget"} {
		if n := state.Coverage.Reasons[reason]; n > 0 {
			current[reason] = n
		}
	}
	state.Coverage.Reasons = current
}

func (state *syncState) deferWindow(window queryWindow, reason string, now time.Time) {
	state.reason(reason, "query")
	for i, gap := range state.Deferred {
		if gap.Window != nil && gap.Window.From.Equal(window.From) && gap.Window.To.Equal(window.To) {
			state.Deferred[i].Reason = reason
			state.Deferred[i].NextAttemptAt = now.Add(transcriptRetryCadence)
			return
		}
	}
	state.Deferred = append(state.Deferred, deferredScope{Window: &window, Reason: reason, NextAttemptAt: now.Add(transcriptRetryCadence)})
}
func (state *syncState) resolveWindow(window queryWindow) {
	retained := state.Deferred[:0]
	for _, gap := range state.Deferred {
		if gap.Window != nil {
			fromContained := window.From.IsZero() || (!gap.Window.From.IsZero() && !gap.Window.From.Before(window.From))
			toContained := window.To.IsZero() || (!gap.Window.To.IsZero() && !gap.Window.To.After(window.To))
			if fromContained && toContained {
				continue
			}
		}
		retained = append(retained, gap)
	}
	state.Deferred = retained
}
