package twilio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingrecording"
	"go.kenn.io/msgvault/internal/store"
)

type Source interface {
	ListRecordings(ctx context.Context, after time.Time) ([]Recording, error)
	ListCalls(ctx context.Context, after time.Time) ([]Call, error)
	GetCall(ctx context.Context, callID string) (Call, error)
	CallRecordings(ctx context.Context, callID string) ([]Recording, error)
	Transcripts(ctx context.Context, call Call, recordings []Recording) (Evidence, error)
	OpenRecording(ctx context.Context, recording Recording, maxBytes int64) (io.ReadCloser, error)
}

type Importer struct {
	store  *store.Store
	client Source
	now    func() time.Time
}

func NewImporter(st *store.Store, client Source) *Importer {
	return &Importer{store: st, client: client, now: time.Now}
}

type ImportOptions struct {
	Identifier, AccountEmail, AttachmentsDir string
	Full                                     bool
	Limit                                    int
	CreatedAfter                             time.Time
	MediaPolicy                              attachmentpolicy.Policy
	Progress                                 func(string)
}
type ImportSummary struct {
	SourceID, MeetingsProcessed, MeetingsAdded, MeetingsUpdated, AttachmentsStored, MaintenanceRetries, Errors int64
	Diagnostics                                                                                                []string
}
type knownCall struct {
	Signature               string            `json:"signature"`
	RecordingSignatures     map[string]string `json:"recording_signatures,omitempty"`
	CreatedAt               time.Time         `json:"created_at,omitzero"`
	FirstSeen               time.Time         `json:"first_seen"`
	NextAttempt             time.Time         `json:"next_attempt"`
	RetryUntil              time.Time         `json:"retry_until"`
	Failed                  bool              `json:"failed"`
	Unverified              bool              `json:"unverified"`
	CallMetadataUnavailable bool              `json:"call_metadata_unavailable,omitempty"`
	RecordingsUnavailable   bool              `json:"recordings_unavailable,omitempty"`
	PendingRecordings       []Recording       `json:"pending_recordings,omitempty"`
}
type discoveryCall struct {
	CallSID    string      `json:"call_sid"`
	Signature  string      `json:"signature"`
	Recordings []Recording `json:"recordings,omitempty"`
}
type syncState struct {
	Version            int                  `json:"version"`
	Watermark          time.Time            `json:"watermark"`
	Known              map[string]knownCall `json:"known"`
	FullQueue          []string             `json:"full_queue,omitempty"`
	FullQueueAfter     string               `json:"full_queue_after,omitempty"`
	DiscoveryWindow    string               `json:"discovery_window,omitempty"`
	DiscoveryStartedAt time.Time            `json:"discovery_started_at,omitzero"`
	DiscoveryCursor    string               `json:"discovery_cursor,omitempty"`
	DiscoveryDone      bool                 `json:"discovery_done,omitempty"`
	DiscoveryQueue     []discoveryCall      `json:"discovery_queue,omitempty"`
	RelayCursor        string               `json:"relay_cursor,omitempty"`
	RelayDone          bool                 `json:"relay_done,omitempty"`
}

type recordingPager interface {
	ListRecordingsPage(ctx context.Context, after time.Time, pageSize int, cursor string) ([]Recording, string, error)
}

type callPager interface {
	ListCallsPage(ctx context.Context, after time.Time, pageSize int, cursor string) ([]Call, string, error)
}

func discoveryWindow(after, createdAfter time.Time, full bool) string {
	format := func(value time.Time) string {
		if value.IsZero() {
			return ""
		}
		return value.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("full=%t|after=%s|created_after=%s", full, format(after), format(createdAfter))
}

func recordingSignature(recording Recording) string {
	raw, _ := json.Marshal(recording, json.Deterministic(true))
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func recordingsSignature(recordings []Recording) string {
	ordered := slices.Clone(recordings)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SID < ordered[j].SID })
	raw, _ := json.Marshal(ordered, json.Deterministic(true))
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func recordingSignatures(recordings []Recording) map[string]string {
	signatures := make(map[string]string, len(recordings))
	for _, recording := range recordings {
		signatures[recording.SID] = recordingSignature(recording)
	}
	return signatures
}

func recordingPageChanged(known knownCall, exists bool, recordings []Recording) bool {
	if known.Failed {
		return false
	}
	if !exists {
		return true
	}
	if len(recordings) == 0 {
		if len(known.RecordingSignatures) > 0 {
			return false
		}
		return recordingsSignature(nil) != known.Signature
	}
	if len(known.RecordingSignatures) == 0 {
		return recordingsSignature(recordings) != known.Signature
	}
	for _, recording := range recordings {
		if known.RecordingSignatures[recording.SID] != recordingSignature(recording) {
			return true
		}
	}
	return false
}

func removeDiscoveryCall(state *syncState, id string) {
	for index, call := range state.DiscoveryQueue {
		if call.CallSID == id {
			state.DiscoveryQueue = slices.Delete(state.DiscoveryQueue, index, index+1)
			return
		}
	}
}

func (imp *Importer) fillDiscoveryQueue(ctx context.Context, pager recordingPager, after time.Time, limit int, state *syncState, diagnostics *[]string) error {
	queued := make(map[string]int, len(state.DiscoveryQueue))
	for index, call := range state.DiscoveryQueue {
		queued[call.CallSID] = index
	}
	seenCursors := map[string]bool{}
	cursor := state.DiscoveryCursor
	pages := 0
	for len(state.DiscoveryQueue) < limit && !state.DiscoveryDone {
		if pages >= maxPages {
			return errors.New("twilio: pagination exceeded page limit")
		}
		if seenCursors[cursor] {
			return errors.New("twilio: pagination loop")
		}
		seenCursors[cursor] = true
		pages++
		recordings, next, err := pager.ListRecordingsPage(ctx, after, 1000, cursor)
		if err != nil {
			return err
		}
		groups := make(map[string][]Recording)
		order := make([]string, 0)
		for _, recording := range recordings {
			if recording.CallSID == "" {
				*diagnostics = append(*diagnostics, "recording_without_call_mapping")
				continue
			}
			if _, ok := groups[recording.CallSID]; !ok {
				order = append(order, recording.CallSID)
			}
			groups[recording.CallSID] = append(groups[recording.CallSID], recording)
		}
		for _, id := range order {
			pageRecordings := groups[id]
			sort.Slice(pageRecordings, func(i, j int) bool { return pageRecordings[i].SID < pageRecordings[j].SID })
			if index, ok := queued[id]; ok {
				combined := mergeRecordings(state.DiscoveryQueue[index].Recordings, pageRecordings)
				state.DiscoveryQueue[index].Recordings = combined
				state.DiscoveryQueue[index].Signature = recordingsSignature(combined)
				continue
			}
			known, exists := state.Known[id]
			if known.Failed || !recordingPageChanged(known, exists, pageRecordings) {
				continue
			}
			queued[id] = len(state.DiscoveryQueue)
			state.DiscoveryQueue = append(state.DiscoveryQueue, discoveryCall{
				CallSID:    id,
				Signature:  recordingsSignature(pageRecordings),
				Recordings: slices.Clone(pageRecordings),
			})
		}
		state.DiscoveryCursor = next
		if next == "" {
			state.DiscoveryDone = true
		}
		cursor = next
	}
	return nil
}

func (imp *Importer) fillRelayDiscoveryQueue(ctx context.Context, pager callPager, after, createdAfter time.Time, limit int, state *syncState) error {
	queued := make(map[string]bool, len(state.DiscoveryQueue))
	for _, call := range state.DiscoveryQueue {
		queued[call.CallSID] = true
	}
	seenCursors := map[string]bool{}
	cursor := state.RelayCursor
	pages := 0
	for len(state.DiscoveryQueue) < limit && !state.RelayDone {
		if pages >= maxPages {
			return errors.New("twilio: relay pagination exceeded page limit")
		}
		if seenCursors[cursor] {
			return errors.New("twilio: relay pagination loop")
		}
		seenCursors[cursor] = true
		pages++
		calls, next, err := pager.ListCallsPage(ctx, after, 1000, cursor)
		if err != nil {
			return err
		}
		for _, call := range calls {
			if !validSID(call.SID, "CA") {
				return errors.New("twilio: invalid call identity in Relay discovery page")
			}
			if !createdAfter.IsZero() {
				createdAt := ParseTime(call.DateCreated)
				if createdAt.IsZero() || createdAt.Before(createdAfter) {
					continue
				}
			}
			if queued[call.SID] {
				continue
			}
			known, exists := state.Known[call.SID]
			if known.Failed || !recordingPageChanged(known, exists, nil) {
				continue
			}
			queued[call.SID] = true
			state.DiscoveryQueue = append(state.DiscoveryQueue, discoveryCall{CallSID: call.SID, Signature: recordingsSignature(nil)})
		}
		state.RelayCursor = next
		if next == "" {
			state.RelayDone = true
		}
		cursor = next
	}
	return nil
}

func loadState(st *store.Store, sourceID int64) (syncState, error) {
	state := syncState{Version: 1, Known: map[string]knownCall{}}
	run, err := st.GetLatestCheckpointedSyncByType(sourceID, SourceType)
	cursor := ""
	if err == nil {
		cursor = run.CursorBefore.String
	} else if !errors.Is(err, store.ErrSyncRunNotFound) {
		return state, err
	} else {
		run, err = st.GetLastSuccessfulSync(sourceID)
		if err == nil {
			cursor = run.CursorAfter.String
		} else if !errors.Is(err, store.ErrSyncRunNotFound) {
			return state, err
		}
	}
	if cursor != "" {
		if err := json.Unmarshal([]byte(cursor), &state); err != nil {
			return state, fmt.Errorf("decode Twilio checkpoint: %w", err)
		}
	}
	if state.Version != 1 {
		return state, errors.New("unsupported Twilio checkpoint version")
	}
	if state.Known == nil {
		state.Known = map[string]knownCall{}
	}
	for id, k := range state.Known {
		if strings.TrimSpace(id) == "" || k.FirstSeen.IsZero() {
			return state, errors.New("invalid Twilio known-call checkpoint")
		}
	}
	return state, nil
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("twilio importer unavailable")
	}
	if opts.Limit < 0 {
		return nil, errors.New("limit must be zero or positive")
	}
	if err := opts.MediaPolicy.Validate(); err != nil {
		return nil, err
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	if err != nil {
		return nil, err
	}
	sum = &ImportSummary{SourceID: source.ID}
	state, err := loadState(imp.store, source.ID)
	if err != nil {
		return sum, err
	}
	now := imp.now().UTC()
	runID, err := imp.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := imp.store.ScopedToSync(source.ID, runID)
	checkpoint := func() *store.Checkpoint {
		raw, _ := json.Marshal(state, json.Deterministic(true))
		return &store.Checkpoint{PageToken: string(raw), MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpoint(runID, "Twilio sync incomplete", checkpoint()))
		}
	}()
	after := opts.CreatedAfter
	if !opts.Full && !state.Watermark.IsZero() {
		overlap := state.Watermark.Add(-7 * 24 * time.Hour)
		if after.IsZero() || overlap.After(after) {
			after = overlap
		}
	}
	// Failed discovery must not advance the enumeration watermark. Previously
	// checkpointed calls can still acquire artifacts independently of discovery.
	recordingsPageSource, hasRecordingsPager := imp.client.(recordingPager)
	callsPageSource, hasCallsPager := imp.client.(callPager)
	relayDiscovery := false
	if relay, ok := imp.client.(interface{ DiscoverCalls() bool }); ok {
		relayDiscovery = relay.DiscoverCalls()
	}
	pagedRecordings := opts.Limit > 0 && hasRecordingsPager
	pagedRelayCalls := opts.Limit > 0 && relayDiscovery && hasCallsPager
	pagedDiscovery := pagedRecordings || pagedRelayCalls
	var listErr error
	groups := map[string][]Recording{}
	order := []string{}
	signatures := map[string]string{}
	if pagedDiscovery {
		window := discoveryWindow(after, opts.CreatedAfter, opts.Full)
		if state.DiscoveryWindow != window {
			state.DiscoveryWindow = window
			state.DiscoveryStartedAt = now
			state.DiscoveryCursor = ""
			state.DiscoveryDone = false
			state.RelayCursor = ""
			state.RelayDone = false
			state.DiscoveryQueue = nil
		}
		if state.DiscoveryStartedAt.IsZero() {
			// Resume older checkpoints that predate the traversal-start field.
			state.DiscoveryStartedAt = now
		}
		if !pagedRecordings {
			state.DiscoveryCursor = ""
			state.DiscoveryDone = true
		}
	} else {
		state.DiscoveryWindow = ""
		state.DiscoveryStartedAt = time.Time{}
		state.DiscoveryCursor = ""
		state.DiscoveryDone = false
		state.RelayCursor = ""
		state.RelayDone = false
		state.DiscoveryQueue = nil
	}
	if pagedRecordings {
		if !state.DiscoveryDone {
			listErr = imp.fillDiscoveryQueue(ctx, recordingsPageSource, after, opts.Limit, &state, &sum.Diagnostics)
		}
	} else {
		recordings, err := imp.client.ListRecordings(ctx, after)
		listErr = err
		for _, r := range recordings {
			if r.CallSID == "" {
				sum.Diagnostics = append(sum.Diagnostics, "recording_without_call_mapping")
				continue
			}
			if _, ok := groups[r.CallSID]; !ok {
				order = append(order, r.CallSID)
			}
			groups[r.CallSID] = append(groups[r.CallSID], r)
		}
		for id, rs := range groups {
			sort.Slice(rs, func(i, j int) bool { return rs[i].SID < rs[j].SID })
			signatures[id] = recordingsSignature(rs)
		}
	}
	if relayDiscovery {
		if pagedRelayCalls {
			if !state.RelayDone {
				listErr = errors.Join(listErr, imp.fillRelayDiscoveryQueue(ctx, callsPageSource, after, opts.CreatedAfter, opts.Limit, &state))
			}
		} else {
			calls, err := imp.client.ListCalls(ctx, after)
			listErr = errors.Join(listErr, err)
			for _, c := range calls {
				if !opts.CreatedAfter.IsZero() {
					createdAt := ParseTime(c.DateCreated)
					if createdAt.IsZero() || createdAt.Before(opts.CreatedAfter) {
						continue
					}
				}
				if _, ok := groups[c.SID]; !ok {
					order = append(order, c.SID)
					groups[c.SID] = nil
					signatures[c.SID] = recordingsSignature(nil)
				}
			}
		}
	}
	if pagedDiscovery {
		ordered := make(map[string]bool, len(order))
		for _, id := range order {
			ordered[id] = true
		}
		for _, queued := range state.DiscoveryQueue {
			id := queued.CallSID
			if existing, ok := groups[id]; ok {
				if len(queued.Recordings) > 0 {
					groups[id] = mergeRecordings(existing, queued.Recordings)
					signatures[id] = recordingsSignature(groups[id])
				}
			} else {
				groups[id] = slices.Clone(queued.Recordings)
				signatures[id] = queued.Signature
			}
			if !ordered[id] {
				order = append(order, id)
				ordered[id] = true
			}
		}
	}
	if opts.Full {
		fullQueueAfter := ""
		if !opts.CreatedAfter.IsZero() {
			fullQueueAfter = opts.CreatedAfter.UTC().Format(time.RFC3339Nano)
		}
		if state.FullQueueAfter != fullQueueAfter {
			state.FullQueue = nil
			state.FullQueueAfter = fullQueueAfter
		}
		if len(state.FullQueue) == 0 {
			for id, known := range state.Known {
				createdAt := known.CreatedAt
				if !opts.CreatedAfter.IsZero() && createdAt.IsZero() {
					prior, err := loadArchive(ctx, scoped, source.ID, id)
					if err != nil {
						return sum, fmt.Errorf("read archived Twilio call %s for creation-date filter: %w", id, err)
					}
					createdAt = ParseTime(prior.Call.DateCreated)
				}
				if !opts.CreatedAfter.IsZero() && (createdAt.IsZero() || createdAt.Before(opts.CreatedAfter)) {
					continue
				}
				state.FullQueue = append(state.FullQueue, id)
			}
			sort.Strings(state.FullQueue)
		}
	}
	selected := []string{}
	seen := map[string]bool{}
	limited := false
	// Failed discoveries are already durable independent work. They do not
	// repeatedly consume the user's discovery limit or starve unseen calls.
	queuedIDs := map[string]bool{}
	queuedSelected := 0
	if pagedDiscovery {
		for _, queued := range state.DiscoveryQueue {
			queuedIDs[queued.CallSID] = true
			if opts.Limit > 0 && len(selected) >= opts.Limit {
				limited = true
				continue
			}
			selected = append(selected, queued.CallSID)
			seen[queued.CallSID] = true
			queuedSelected++
		}
	}
	for _, id := range order {
		if queuedIDs[id] || seen[id] {
			continue
		}
		k, known := state.Known[id]
		if known && k.Failed {
			continue
		}
		if pagedDiscovery {
			if known && len(groups[id]) == 0 {
				continue
			}
			if !recordingPageChanged(k, known, groups[id]) {
				continue
			}
		} else if known && k.Signature == signatures[id] {
			continue
		}
		if opts.Limit > 0 && len(selected) >= opts.Limit {
			limited = true
			continue
		}
		selected = append(selected, id)
		seen[id] = true
	}
	discoveryPending := len(state.DiscoveryQueue) > queuedSelected
	if pagedRecordings && (state.DiscoveryCursor != "" || !state.DiscoveryDone) {
		discoveryPending = true
	}
	if pagedRelayCalls && (state.RelayCursor != "" || !state.RelayDone) {
		discoveryPending = true
	}
	if pagedDiscovery && discoveryPending {
		limited = true
	}
	for _, id := range state.FullQueue {
		if seen[id] {
			continue
		}
		if opts.Limit > 0 && len(selected) >= opts.Limit {
			limited = true
			break
		}
		selected = append(selected, id)
		seen[id] = true
	}
	retryIDs := []string{}
	dueRetries := map[string]bool{}
	for id, k := range state.Known {
		// Include a final due attempt after the absence window so pending
		// artifacts become unavailable and their NextAttempt can be cleared.
		if !seen[id] && !k.NextAttempt.IsZero() && !now.Before(k.NextAttempt) {
			retryIDs = append(retryIDs, id)
			dueRetries[id] = true
		}
	}
	sort.Strings(retryIDs)
	selected = append(selected, retryIDs...)
	sum.MaintenanceRetries = int64(len(retryIDs))
	// Each snapshot contains archival identities as well as the retry ledger.
	// Bound full-state snapshots per run rather than serializing an increasing
	// map after every call. The final/failure checkpoint saves the latest state;
	// an abrupt restart safely replays the uncheckpointed, idempotent writes.
	checkpointEvery := max(1, (len(selected)+15)/16)
	checkpointAttempt := func(index int) error {
		if (index+1)%checkpointEvery != 0 {
			return nil
		}
		return scoped.UpdateSyncCheckpointContext(ctx, runID, checkpoint())
	}
	var failures []error
	if listErr != nil {
		failures = append(failures, listErr)
		sum.Errors++
	}
	for index, id := range selected {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		// Selection is not an attempt: cancellation must retain queued calls
		// that never ran. Attempt failures become durable independent retries.
		if index := slices.Index(state.FullQueue, id); index >= 0 {
			state.FullQueue = slices.Delete(state.FullQueue, index, index+1)
		}
		k := state.Known[id]
		if k.FirstSeen.IsZero() {
			k.FirstSeen = now
			k.RetryUntil = now.Add(48 * time.Hour)
		}
		k.Unverified = true
		k.PendingRecordings = mergeRecordings(k.PendingRecordings, groups[id])
		prior, priorErr := loadArchive(ctx, scoped, source.ID, id)
		if priorErr != nil {
			failures = append(failures, priorErr)
			sum.Errors++
			k.Failed = true
			k.NextAttempt = now.Add(6 * time.Hour)
			state.Known[id] = k
			removeDiscoveryCall(&state, id)
			if err := checkpointAttempt(index); err != nil {
				return sum, errors.Join(priorErr, err)
			}
			continue
		}
		call, callErr := imp.client.GetCall(ctx, id)
		k.CallMetadataUnavailable = isNotFound(callErr)
		if k.CallMetadataUnavailable {
			// Calls expire independently of retained recordings/transcriptions.
			// Keep authoritative archived metadata, or just the discovered identity.
			call = prior.Call
			call.SID = id
			if call.AccountSID == "" && len(groups[id]) > 0 {
				call.AccountSID = groups[id][0].AccountSID
			}
			callErr = nil
		}
		if callErr != nil {
			k.Failed = true
			k.NextAttempt = now.Add(6 * time.Hour)
			state.Known[id] = k
			removeDiscoveryCall(&state, id)
			sum.Errors++
			failures = append(failures, callErr)
			if err := checkpointAttempt(index); err != nil {
				return sum, errors.Join(callErr, err)
			}
			continue
		}
		createdAt := ParseTime(call.DateCreated)
		// Recording creation can lag call creation, so enforce a requested
		// call-date bound against the call metadata after discovery.
		if !opts.CreatedAfter.IsZero() && !dueRetries[id] && (createdAt.IsZero() || createdAt.Before(opts.CreatedAfter)) {
			removeDiscoveryCall(&state, id)
			continue
		}
		if !createdAt.IsZero() {
			k.CreatedAt = createdAt
		}
		if end := ParseTime(call.EndTime); !end.IsZero() {
			k.RetryUntil = end.Add(7 * 24 * time.Hour)
		}
		freshRecordings, recordingErr := imp.client.CallRecordings(ctx, id)
		k.RecordingsUnavailable = isNotFound(recordingErr)
		if k.RecordingsUnavailable {
			// Account-wide recording discovery remains useful when the Call's
			// subresource has disappeared. Do not discard that authenticated evidence.
			recordingErr = nil
		}
		all := mergeRecordings(prior.Recordings, k.PendingRecordings)
		all = mergeRecordings(all, groups[id])
		if recordingErr == nil {
			all = mergeRecordings(all, freshRecordings)
		}
		evidence, transcriptErr := imp.client.Transcripts(ctx, call, all)
		if k.CallMetadataUnavailable {
			evidence.Diagnostics = append(evidence.Diagnostics, "call_metadata_unavailable")
		}
		if k.RecordingsUnavailable {
			evidence.Diagnostics = append(evidence.Diagnostics, "call_recordings_unavailable")
		}
		archive := mergeArchive(prior, call, all, evidence)
		snapshot, formatErr := archive.snapshot(source.ID, opts.AccountEmail)
		if formatErr != nil {
			return sum, formatErr
		}
		result, writeErr := meetingarchive.New(scoped).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
		if result.Changed {
			if result.Created {
				sum.MeetingsAdded++
			} else {
				sum.MeetingsUpdated++
			}
		}
		sum.MeetingsProcessed++
		mediaPending := false
		artifactsChanged := false
		var mediaErr error
		if writeErr == nil {
			mediaPending, artifactsChanged, mediaErr = imp.persistRecordings(ctx, scoped, result.MessageID, archive.Recordings, archive.RecordingArtifacts, opts, sum, k.RetryUntil)
		}
		if artifactsChanged {
			updatedSnapshot, artifactErr := archive.snapshot(source.ID, opts.AccountEmail)
			if artifactErr == nil {
				_, artifactErr = meetingarchive.New(scoped).Upsert(ctx, updatedSnapshot, meetingarchive.UpsertOptions{})
			}
			mediaErr = errors.Join(mediaErr, artifactErr)
		}
		itemErr := errors.Join(recordingErr, transcriptErr, writeErr, mediaErr)
		if writeErr == nil {
			// The archive now owns the recording metadata, so the independent
			// retry copy is no longer needed.
			k.PendingRecordings = nil
		}
		k.Failed = itemErr != nil
		if itemErr != nil {
			failures = append(failures, itemErr)
			sum.Errors++
			sum.Diagnostics = append(sum.Diagnostics, "artifact_acquisition_failed")
		}
		if writeErr == nil && recordingErr == nil {
			if signature, ok := signatures[id]; ok {
				k.Signature = signature
			}
			k.RecordingSignatures = recordingSignatures(all)
			k.Unverified = false
		}
		// A usable archived transcript can survive an upstream omission. Keep
		// polling new calls for late additional artifacts until the absence window.
		if k.Failed || mediaPending || now.Before(k.RetryUntil) {
			k.NextAttempt = now.Add(6 * time.Hour)
		} else {
			k.NextAttempt = time.Time{}
		}
		state.Known[id] = k
		removeDiscoveryCall(&state, id)
		sum.Diagnostics = append(sum.Diagnostics, evidence.Diagnostics...)
		if err := checkpointAttempt(index); err != nil {
			return sum, err
		}
	}
	unresolved := false
	for _, k := range state.Known {
		if k.CallMetadataUnavailable {
			sum.Diagnostics = append(sum.Diagnostics, "call_metadata_unavailable")
		}
		if k.RecordingsUnavailable {
			sum.Diagnostics = append(sum.Diagnostics, "call_recordings_unavailable")
		}
		if k.Unverified {
			unresolved = true
		}
	}
	if len(failures) == 0 && !limited && !unresolved {
		if opts.CreatedAfter.IsZero() {
			state.Watermark = now
			if pagedDiscovery && !state.DiscoveryStartedAt.IsZero() {
				state.Watermark = state.DiscoveryStartedAt
			}
		}
		if pagedDiscovery {
			state.DiscoveryWindow = ""
			state.DiscoveryStartedAt = time.Time{}
			state.DiscoveryCursor = ""
			state.DiscoveryDone = false
			state.RelayCursor = ""
			state.RelayDone = false
			state.DiscoveryQueue = nil
		}
	}
	sort.Strings(sum.Diagnostics)
	sum.Diagnostics = compactStrings(sum.Diagnostics)
	if len(failures) > 0 {
		// Failed manual syncs skip the completion summary. Surface coverage
		// here; successful syncs print it once through the summary instead.
		if opts.Progress != nil {
			for _, diagnostic := range sum.Diagnostics {
				opts.Progress("Coverage: " + diagnostic)
			}
		}
		return sum, errors.Join(failures...)
	}
	raw, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return sum, err
	}
	if err := scoped.UpdateSyncCheckpointContext(ctx, runID, checkpoint()); err != nil {
		return sum, err
	}
	return sum, scoped.CompleteSyncContext(ctx, runID, string(raw))
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func compactStrings(values []string) []string {
	out := values[:0]
	for _, v := range values {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func (imp *Importer) persistRecordings(ctx context.Context, st *store.Store, messageID int64, recordings []Recording, artifacts map[string]recordingArtifact, opts ImportOptions, sum *ImportSummary, retryUntil time.Time) (bool, bool, error) {
	msg, err := st.GetMessageContext(ctx, messageID)
	if err != nil {
		return true, false, err
	}
	stored := map[string]bool{}
	for _, a := range msg.Attachments {
		if a.ContentHash != "" {
			stored[a.Filename] = true
		}
	}
	capBytes := opts.MediaPolicy.MaxBytes
	if capBytes <= 0 {
		capBytes = attachmentpolicy.DefaultChatMaxBytes
	}
	var failures []error
	pending := false
	artifactsChanged := false
	for _, r := range recordings {
		filename := r.SID + ".wav"
		if stored[filename] || stored[r.SID+".mp3"] || stored[r.SID+".flac"] || stored[r.SID+".ogg"] {
			continue
		}
		metadata, _ := json.Marshal(r)
		write := store.AttachmentWrite{Filename: filename, MIMEType: "audio/wav", SourceAttachmentID: "twilio:" + r.SID, SourcePartKey: "twilio:" + r.SID, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, MediaType: "audio", Metadata: string(metadata), State: attachmentpolicy.StatePending}
		write.DurationMS = int64(durationSeconds(r.Duration) * 1000)
		if artifact, ok := artifacts[r.SID]; ok {
			if artifact.State == attachmentpolicy.StateStored && artifact.ContentHash != "" && artifact.StoragePath != "" {
				if stored[artifact.Filename] {
					continue
				}
				if artifact.Filename != "" {
					write.Filename = artifact.Filename
				}
				if artifact.MIMEType != "" {
					write.MIMEType = artifact.MIMEType
				}
				write.ContentHash = artifact.ContentHash
				write.StoragePath = artifact.StoragePath
				write.Size = artifact.Size
				write.State = attachmentpolicy.StateStored
				if err := st.UpsertAttachmentRecordPreservingStored(ctx, messageID, write); err != nil {
					failures = append(failures, err)
				}
				continue
			}
			if !opts.Full && artifact.State == attachmentpolicy.StateSkipped && artifact.SkipReason == attachmentpolicy.SkipSizeCap {
				write.State = attachmentpolicy.StateSkipped
				write.SkipReason = attachmentpolicy.SkipSizeCap
				if err := st.UpsertAttachmentRecordPreservingStored(ctx, messageID, write); err != nil {
					failures = append(failures, err)
				}
				continue
			}
		}
		if reason := opts.MediaPolicy.Evaluate(attachmentpolicy.Conversation{Type: "meeting", ParticipantCount: 2}, 0); reason != "" {
			write.State = attachmentpolicy.StateSkipped
			write.SkipReason = reason
		} else if r.Status != "completed" {
			if r.Status == "deleted" || r.Status == "absent" {
				write.State = attachmentpolicy.StateUnavailable
				write.SkipReason = attachmentpolicy.SkipSourceUnavailable
			} else if imp.now().Before(retryUntil) {
				pending = true
			} else {
				write.State = attachmentpolicy.StateUnavailable
				write.SkipReason = attachmentpolicy.SkipSourceUnavailable
			}
		} else {
			reader, fetchErr := imp.client.OpenRecording(ctx, r, capBytes)
			var blob meetingrecording.Blob
			if fetchErr == nil {
				if media, ok := reader.(*recordingMedia); ok {
					filename = r.SID + "." + media.format.extension
					write.Filename = filename
					write.MIMEType = media.format.mimeType
				}
				blob, fetchErr = meetingrecording.StoreAudio(ctx, opts.AttachmentsDir, reader, capBytes)
				fetchErr = errors.Join(fetchErr, reader.Close())
			}
			if fetchErr != nil {
				var apiErr *APIError
				if errors.Is(fetchErr, meetingrecording.ErrSizeLimit) || errors.Is(fetchErr, ErrMediaSizeLimit) {
					write.State = attachmentpolicy.StateSkipped
					write.SkipReason = attachmentpolicy.SkipSizeCap
				} else if errors.As(fetchErr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
					if imp.now().Before(retryUntil) {
						write.State = attachmentpolicy.StatePending
						pending = true
					} else {
						write.State = attachmentpolicy.StateUnavailable
						write.SkipReason = attachmentpolicy.SkipSourceUnavailable
					}
				} else {
					write.State = attachmentpolicy.StateFailed
					write.SkipReason = attachmentpolicy.SkipFetchFailure
					pending = true
					failures = append(failures, fetchErr)
				}
			} else {
				write.State = attachmentpolicy.StateStored
				write.ContentHash = blob.Hash
				write.StoragePath = blob.Path
				write.Size = blob.Size
				sum.AttachmentsStored++
			}
		}
		if write.State == attachmentpolicy.StateStored {
			artifact := recordingArtifact{State: write.State, Filename: write.Filename, MIMEType: write.MIMEType, ContentHash: write.ContentHash, StoragePath: write.StoragePath, Size: write.Size}
			if artifacts[r.SID] != artifact {
				artifacts[r.SID] = artifact
				artifactsChanged = true
			}
		} else if write.State == attachmentpolicy.StateSkipped && write.SkipReason == attachmentpolicy.SkipSizeCap {
			artifact := recordingArtifact{State: write.State, SkipReason: write.SkipReason}
			if artifacts[r.SID] != artifact {
				artifacts[r.SID] = artifact
				artifactsChanged = true
			}
		}
		if err := st.UpsertAttachmentRecordPreservingStored(ctx, messageID, write); err != nil {
			failures = append(failures, err)
		}
	}
	if len(recordings) > 0 {
		if err := st.RecomputeMessageAttachmentStats(messageID); err != nil {
			failures = append(failures, err)
		}
	}
	return pending, artifactsChanged, errors.Join(failures...)
}
