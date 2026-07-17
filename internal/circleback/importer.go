package circleback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

// readBatchSize bounds ReadMeetings/GetTranscripts payloads.
const readBatchSize = 5

// rescanOverlap is how far the incremental creation-time refresh window
// reaches back before the stored watermark. Incremental discovery itself is
// unbounded because Circleback only exposes scheduled-date search filters;
// every unknown ID is hydrated, while known IDs outside this overlap are
// skipped. Edits to older known meetings require --full.
const rescanOverlap = 48 * time.Hour

const (
	transcriptRetryCadence     = 6 * time.Hour
	transcriptRetryWindow      = 7 * 24 * time.Hour
	unknownTranscriptRetrySpan = 48 * time.Hour
	// Bump after a released canonical format changes so archived rows receive
	// one repair refresh under the new rules.
	circlebackSnapshotVersion = 1
)

// meetingSource is the narrow client surface the importer needs (satisfied
// by *Session; faked in tests).
type meetingSource interface {
	SearchMeetings(ctx context.Context, start, end string, pageIndex int) ([]Meeting, error)
	ReadMeetings(ctx context.Context, ids []string) ([]Meeting, error)
	GetTranscripts(ctx context.Context, ids []string) (map[string]*Transcript, error)
}

// Importer ingests Circleback meetings into the msgvault store.
type Importer struct {
	store  *store.Store
	client meetingSource
	now    func() time.Time
}

// NewImporter creates an Importer backed by the given store and session.
func NewImporter(s *store.Store, c meetingSource) *Importer {
	return &Importer{store: s, client: c, now: time.Now}
}

// ImportOptions controls a sync run.
type ImportOptions struct {
	// Identifier names the source row (the configured account label).
	Identifier string
	// AccountEmail is the configured primary identity for organizer
	// attribution. Stored aliases for the source are included automatically.
	AccountEmail string
	// Full ignores the stored watermark and re-fetches everything (bounded
	// by CreatedAfter when set).
	Full bool
	// Limit caps newly searched meetings (0 = unlimited). Due transcript
	// maintenance items are additional work outside the cap.
	Limit int
	// CreatedAfter bounds a full sync to meetings after this time.
	CreatedAfter time.Time
	// Progress, when set, receives one-line status updates.
	Progress func(string)
}

// ImportSummary reports what a run did.
type ImportSummary struct {
	SourceID           int64
	MeetingsProcessed  int64
	MeetingsAdded      int64
	MeetingsUpdated    int64
	MaintenanceRetries int64
	Errors             int64
	Duration           time.Duration
}

// pendingTranscript is one bounded maintenance retry persisted in the sync
// cursor. Timestamps use RFC3339 for deterministic cursor JSON.
type pendingTranscript struct {
	MeetingID     string `json:"meeting_id"`
	NextAttemptAt string `json:"next_attempt_at"`
	RetryUntil    string `json:"retry_until"`
}

// syncState is the JSON cursor persisted in sync_runs.cursor_after.
type syncState struct {
	// CreatedAfter is the RFC3339 max createdAt across all meetings
	// ingested by the last fully-successful run.
	CreatedAfter       string              `json:"created_after"`
	PendingTranscripts []pendingTranscript `json:"pending_transcripts,omitempty"`
}

func (s syncState) marshal() string {
	sort.Slice(s.PendingTranscripts, func(i, j int) bool {
		return s.PendingTranscripts[i].MeetingID < s.PendingTranscripts[j].MeetingID
	})
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func schedulePendingTranscript(m *Meeting, now time.Time, previous *pendingTranscript) (pendingTranscript, bool) {
	now = now.UTC()
	retryUntil := time.Time{}
	if previous != nil {
		retryUntil, _ = time.Parse(time.RFC3339, previous.RetryUntil)
	}

	scheduledAt := m.ScheduledAt()
	if scheduledAt.IsZero() {
		if retryUntil.IsZero() {
			retryUntil = now.Add(unknownTranscriptRetrySpan)
		}
	} else if scheduledDeadline := scheduledAt.Add(transcriptRetryWindow); scheduledDeadline.After(retryUntil) {
		retryUntil = scheduledDeadline
	}
	if !retryUntil.After(now) {
		return pendingTranscript{}, false
	}

	nextAttempt := now.Add(transcriptRetryCadence)
	if previous == nil && scheduledAt.After(now) {
		if endedAt := m.EndedAt(); !endedAt.IsZero() {
			nextAttempt = endedAt
		} else {
			nextAttempt = scheduledAt.Add(time.Hour)
		}
	}
	if nextAttempt.After(retryUntil) {
		// The deadline may schedule a terminal maintenance transition sooner
		// than six hours, but expired records are never sent to the transcript
		// provider again.
		nextAttempt = retryUntil
	}

	return pendingTranscript{
		MeetingID:     string(m.ID),
		NextAttemptAt: nextAttempt.UTC().Format(time.RFC3339),
		RetryUntil:    retryUntil.UTC().Format(time.RFC3339),
	}, true
}

func (s syncState) validate() error {
	if s.CreatedAfter != "" {
		if _, err := time.Parse(time.RFC3339, s.CreatedAfter); err != nil {
			return fmt.Errorf("created_after %q is not RFC3339: %w", s.CreatedAfter, err)
		}
	}
	seen := make(map[string]struct{}, len(s.PendingTranscripts))
	for i, pending := range s.PendingTranscripts {
		id := strings.TrimSpace(pending.MeetingID)
		if id == "" {
			return fmt.Errorf("pending_transcripts[%d] has a blank meeting_id", i)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("pending_transcripts contains duplicate meeting_id %q", id)
		}
		seen[id] = struct{}{}
		nextAttempt, err := time.Parse(time.RFC3339, pending.NextAttemptAt)
		if err != nil {
			return fmt.Errorf("pending transcript %q next_attempt_at %q is not RFC3339: %w",
				id, pending.NextAttemptAt, err)
		}
		retryUntil, err := time.Parse(time.RFC3339, pending.RetryUntil)
		if err != nil {
			return fmt.Errorf("pending transcript %q retry_until %q is not RFC3339: %w",
				id, pending.RetryUntil, err)
		}
		if nextAttempt.After(retryUntil) {
			return fmt.Errorf("pending transcript %q next_attempt_at is after retry_until", id)
		}
	}
	return nil
}

func pendingTranscriptExpired(pending pendingTranscript, now time.Time) bool {
	retryUntil, err := time.Parse(time.RFC3339, pending.RetryUntil)
	return err == nil && !retryUntil.After(now)
}

func pendingTranscriptDue(pending pendingTranscript, now time.Time) bool {
	retryUntil, untilErr := time.Parse(time.RFC3339, pending.RetryUntil)
	if untilErr != nil || !retryUntil.After(now) {
		return true
	}
	nextAttempt, nextErr := time.Parse(time.RFC3339, pending.NextAttemptAt)
	return nextErr != nil || !nextAttempt.After(now)
}

func pendingTranscriptSlice(pendingByID map[string]pendingTranscript) []pendingTranscript {
	pending := make([]pendingTranscript, 0, len(pendingByID))
	for _, record := range pendingByID {
		pending = append(pending, record)
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].MeetingID < pending[j].MeetingID
	})
	return pending
}

// Import runs a full or incremental import for the configured account.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := imp.now()
	src, err := imp.store.GetOrCreateSource(SourceType, opts.Identifier)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}
	accountIdentities, err := meetingidentity.ForSource(imp.store, src.ID, opts.AccountEmail)
	if err != nil {
		return nil, err
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	syncID, err := imp.store.StartSync(src.ID, SourceType)
	if err != nil {
		return nil, err
	}
	var hardErrors []error
	defer func() {
		if err != nil {
			_ = imp.store.FailSyncWithCheckpoint(syncID, err.Error(), &store.Checkpoint{
				MessagesProcessed: sum.MeetingsProcessed,
				MessagesAdded:     sum.MeetingsAdded,
				MessagesUpdated:   sum.MeetingsUpdated,
				ErrorsCount:       sum.Errors,
			})
		}
	}()
	cancelIfDone := func(affected int) bool {
		ctxErr := ctx.Err()
		if ctxErr == nil {
			return false
		}
		sum.Errors += int64(max(affected, 1))
		hardErrors = append(hardErrors, ctxErr)
		err = errors.Join(hardErrors...)
		return true
	}

	var state syncState
	prev, prevErr := imp.store.GetLastSuccessfulSync(src.ID)
	if prevErr == nil && prev != nil && prev.CursorAfter.Valid && prev.CursorAfter.String != "" {
		if decodeErr := json.Unmarshal([]byte(prev.CursorAfter.String), &state); decodeErr != nil {
			sum.Errors++
			err = fmt.Errorf("decode previous Circleback sync cursor: %w", decodeErr)
			return sum, err
		}
	} else if prevErr != nil && !errors.Is(prevErr, store.ErrSyncRunNotFound) {
		sum.Errors++
		err = fmt.Errorf("load previous Circleback sync cursor: %w", prevErr)
		return sum, err
	}
	if stateErr := state.validate(); stateErr != nil {
		sum.Errors++
		err = fmt.Errorf("validate previous Circleback sync cursor: %w", stateErr)
		return sum, err
	}

	pendingByID := make(map[string]pendingTranscript, len(state.PendingTranscripts))
	for _, pending := range state.PendingTranscripts {
		id := strings.TrimSpace(pending.MeetingID)
		if id == "" {
			continue
		}
		pending.MeetingID = id
		pendingByID[id] = pending
	}

	searchStart := ""
	var refreshCreatedAfter time.Time
	if opts.Full {
		if !opts.CreatedAfter.IsZero() {
			searchStart = opts.CreatedAfter.UTC().Format(time.DateOnly)
		}
	} else if state.CreatedAfter != "" {
		if t, terr := time.Parse(time.RFC3339, state.CreatedAfter); terr == nil {
			refreshCreatedAfter = t.Add(-rescanOverlap)
		}
	}

	now := start.UTC()
	var dueIDs []string
	for id, pending := range pendingByID {
		if pendingTranscriptDue(pending, now) {
			dueIDs = append(dueIDs, id)
		}
	}
	sort.Strings(dueIDs)

	limit := max(opts.Limit, 0)
	processedIDs := make(map[string]struct{}, len(dueIDs))
	for _, id := range dueIDs {
		processedIDs[id] = struct{}{}
	}
	sum.MaintenanceRetries = int64(len(dueIDs))
	progress(fmt.Sprintf("maintenance worklist: %d due transcript maintenance items", len(dueIDs)))

	// maxCreated tracks the new watermark; it only advances past batches
	// that ingested cleanly, and only persists after a zero-error run.
	var maxCreated time.Time
	if state.CreatedAfter != "" {
		if t, terr := time.Parse(time.RFC3339, state.CreatedAfter); terr == nil {
			maxCreated = t
		}
	}

	processWorkIDs := func(workIDs []string) error {
		for batchStart := 0; batchStart < len(workIDs); batchStart += readBatchSize {
			if cancelIfDone(1) {
				return err
			}
			ids := workIDs[batchStart:min(batchStart+readBatchSize, len(workIDs))]

			meetings, readErr := imp.client.ReadMeetings(ctx, ids)
			if cancelIfDone(len(ids)) {
				return err
			}
			if readErr != nil {
				sum.Errors += int64(len(ids))
				hardErr := fmt.Errorf("read meetings %v: %w", ids, readErr)
				hardErrors = append(hardErrors, hardErr)
				progress(hardErr.Error())
			} else {
				requested := make(map[string]struct{}, len(ids))
				for _, id := range ids {
					requested[id] = struct{}{}
				}
				readIDs := make(map[string]struct{}, len(meetings))
				orderedMeetings := make([]Meeting, 0, len(meetings))
				for i := range meetings {
					id := string(meetings[i].ID)
					if id == "" {
						hardErr := errors.New("ReadMeetings returned a meeting without an id")
						sum.Errors++
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
						continue
					}
					if _, ok := requested[id]; !ok {
						hardErr := fmt.Errorf("ReadMeetings returned unrequested meeting %s", id)
						sum.Errors++
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
						continue
					}
					if _, duplicate := readIDs[id]; duplicate {
						hardErr := fmt.Errorf("ReadMeetings returned meeting %s more than once", id)
						sum.Errors++
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
						continue
					}
					readIDs[id] = struct{}{}
					orderedMeetings = append(orderedMeetings, meetings[i])
				}
				for _, id := range ids {
					if _, ok := readIDs[id]; !ok {
						hardErr := fmt.Errorf("meeting %s: missing from ReadMeetings result", id)
						sum.Errors++
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
					}
				}

				smids := make([]string, 0, len(orderedMeetings))
				for i := range orderedMeetings {
					smids = append(smids, "meeting:"+string(orderedMeetings[i].ID))
				}
				existing := map[string]int64{}
				if len(smids) > 0 {
					var exErr error
					existing, exErr = imp.store.MessageExistsBatch(src.ID, smids)
					if exErr != nil {
						sum.Errors += int64(len(orderedMeetings))
						hardErrors = append(hardErrors, fmt.Errorf("lookup existing meetings: %w", exErr))
						err = errors.Join(hardErrors...)
						return err
					}
				}

				unavailableIDs := make(map[string]struct{})
				stateErrorIDs := make(map[string]struct{})
				transcriptFetchIDs := make([]string, 0, len(orderedMeetings))
				for i := range orderedMeetings {
					id := string(orderedMeetings[i].ID)
					if pending, ok := pendingByID[id]; ok && !opts.Full && pendingTranscriptExpired(pending, now) {
						// Expiry is a terminal metadata transition, not another
						// provider attempt sooner than the six-hour cadence.
						continue
					}
					msgID, exists := existing["meeting:"+id]
					if exists && !opts.Full {
						archivedState, stateErr := imp.archivedTranscriptState(msgID)
						if stateErr != nil {
							hardErr := fmt.Errorf("meeting %s: read archived transcript state: %w", id, stateErr)
							sum.Errors++
							hardErrors = append(hardErrors, hardErr)
							stateErrorIDs[id] = struct{}{}
							progress(hardErr.Error())
							continue
						}
						if archivedState == transcriptStateUnavailable {
							unavailableIDs[id] = struct{}{}
							continue
						}
					}
					transcriptFetchIDs = append(transcriptFetchIDs, id)
				}

				transcripts := map[string]*Transcript{}
				var trErr error
				if len(transcriptFetchIDs) > 0 {
					transcripts, trErr = imp.client.GetTranscripts(ctx, transcriptFetchIDs)
					if cancelIfDone(len(transcriptFetchIDs)) {
						return err
					}
					if errors.Is(trErr, context.Canceled) {
						sum.Errors += int64(len(transcriptFetchIDs))
						hardErrors = append(hardErrors,
							fmt.Errorf("get transcripts %v: %w", transcriptFetchIDs, trErr))
						err = errors.Join(hardErrors...)
						return err
					}
					if trErr != nil {
						// Notes are still worth archiving for meetings not yet in the
						// store. Existing meetings are skipped so a transient tool
						// failure cannot overwrite their archived transcript.
						sum.Errors += int64(len(transcriptFetchIDs))
						hardErr := fmt.Errorf("get transcripts %v: %w", transcriptFetchIDs, trErr)
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
						transcripts = map[string]*Transcript{}
					}
				}

				for i := range orderedMeetings {
					m := &orderedMeetings[i]
					id := string(m.ID)
					sum.MeetingsProcessed++
					if _, stateFailed := stateErrorIDs[id]; stateFailed {
						continue
					}
					if _, unavailable := unavailableIDs[id]; unavailable {
						delete(pendingByID, id)
						if cancelIfDone(1) {
							return err
						}
						added, changed, ingErr := imp.ingestMeeting(
							src.ID, opts.Identifier, accountIdentities, m, nil, transcriptStateUnavailable, opts.Full,
						)
						if ingErr != nil {
							hardErr := fmt.Errorf("meeting %s: ingest unavailable refresh failed: %w", m.ID, ingErr)
							sum.Errors++
							hardErrors = append(hardErrors, hardErr)
							progress(hardErr.Error())
							continue
						}
						if added {
							sum.MeetingsAdded++
						} else if changed {
							sum.MeetingsUpdated++
						}
						if ct := m.CreatedTime(); ct.After(maxCreated) {
							maxCreated = ct
						}
						progress(fmt.Sprintf(
							"refreshed %q (%s); transcript retry expired, use --full to check again",
							meetingTitle(m), m.ID,
						))
						continue
					}
					tr := transcripts[id]
					msgID, exists := existing["meeting:"+id]
					if trErr != nil {
						if exists {
							progress(fmt.Sprintf("meeting %s: transcript fetch failed; keeping the archived copy", m.ID))
						} else if errors.Is(trErr, ErrContract) {
							progress(fmt.Sprintf("meeting %s: transcript contract failed; leaving the meeting unmodified", m.ID))
						} else {
							progress(fmt.Sprintf("meeting %s: transcript fetch failed; leaving the meeting unmodified for retry", m.ID))
						}
						continue
					}
					if tr != nil && tr.Classification() == TranscriptUnrecognized {
						hardErr := fmt.Errorf("meeting %s: %w: unrecognized transcript payload", m.ID, ErrContract)
						sum.Errors++
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
						continue
					}
					if exists && (tr == nil || tr.Classification() == TranscriptRecognizedEmpty) {
						recovered, recoveredOK, recoveryErr := imp.recoverArchivedTranscript(msgID)
						if recoveryErr != nil {
							hardErr := fmt.Errorf("meeting %s: recover archived transcript: %w", m.ID, recoveryErr)
							sum.Errors++
							hardErrors = append(hardErrors, hardErr)
							progress(hardErr.Error())
							continue
						}
						if recoveredOK {
							tr = &recovered
						}
					}

					desiredState := transcriptState("")
					if hasTranscriptContent(tr) {
						desiredState = transcriptStatePresent
						delete(pendingByID, id)
					} else if trErr == nil {
						var previous *pendingTranscript
						if pending, ok := pendingByID[id]; ok {
							priorPending := pending
							previous = &priorPending
						}
						pending, retry := schedulePendingTranscript(m, now, previous)
						if retry {
							desiredState = transcriptStatePending
							pendingByID[id] = pending
						} else {
							desiredState = transcriptStateUnavailable
							delete(pendingByID, id)
						}
					}

					if cancelIfDone(1) {
						return err
					}
					added, changed, ingErr := imp.ingestMeeting(
						src.ID, opts.Identifier, accountIdentities, m, tr, desiredState, opts.Full,
					)
					if ingErr != nil {
						hardErr := fmt.Errorf("meeting %s: ingest failed: %w", m.ID, ingErr)
						sum.Errors++
						hardErrors = append(hardErrors, hardErr)
						progress(hardErr.Error())
						continue
					}
					if added {
						sum.MeetingsAdded++
					} else if changed {
						sum.MeetingsUpdated++
					}
					if ct := m.CreatedTime(); ct.After(maxCreated) {
						maxCreated = ct
					}
					progress(fmt.Sprintf("imported %q (%s)", meetingTitle(m), m.ID))
				}
			}

			if cpErr := imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
				MessagesProcessed: sum.MeetingsProcessed,
				MessagesAdded:     sum.MeetingsAdded,
				MessagesUpdated:   sum.MeetingsUpdated,
				ErrorsCount:       sum.Errors,
			}); cpErr != nil {
				sum.Errors++
				checkpointErr := fmt.Errorf("checkpoint: %w", cpErr)
				hardErrors = append(hardErrors, checkpointErr)
				progress(checkpointErr.Error())
			}
		}
		return nil
	}

	if phaseErr := processWorkIDs(dueIDs); phaseErr != nil {
		err = phaseErr
		return sum, err
	}

	var searchedIDs []string
	seenSearchIDs := make(map[string]struct{})
	for pageIndex := 0; ; pageIndex++ {
		page, pageErr := imp.client.SearchMeetings(ctx, searchStart, "", pageIndex)
		if cancelIfDone(1) {
			return sum, err
		}
		if pageErr != nil {
			sum.Errors++
			hardErrors = append(hardErrors, fmt.Errorf("search meetings page %d: %w", pageIndex, pageErr))
			err = errors.Join(hardErrors...)
			return sum, err
		}
		if len(page) == 0 {
			break
		}
		pageSMIDs := make([]string, 0, len(page))
		for i := range page {
			if id := string(page[i].ID); id != "" {
				pageSMIDs = append(pageSMIDs, "meeting:"+id)
			}
		}
		existingPage, lookupErr := imp.store.MessageMetadataBatch(src.ID, pageSMIDs)
		if lookupErr != nil {
			sum.Errors++
			hardErrors = append(hardErrors, fmt.Errorf("lookup search page %d meetings: %w", pageIndex, lookupErr))
			err = errors.Join(hardErrors...)
			return sum, err
		}

		newOnPage := 0
		limitReached := false
		for _, meeting := range page {
			id := string(meeting.ID)
			if id == "" {
				sum.Errors++
				hardErrors = append(hardErrors,
					fmt.Errorf("search meetings page %d returned a meeting without an id", pageIndex))
				err = errors.Join(hardErrors...)
				return sum, err
			}
			if _, ok := seenSearchIDs[id]; ok {
				continue
			}
			seenSearchIDs[id] = struct{}{}
			newOnPage++
			if _, processed := processedIDs[id]; processed {
				continue
			}
			if _, pending := pendingByID[id]; pending && !opts.Full {
				continue
			}
			if !opts.Full {
				archived, exists := existingPage["meeting:"+id]
				created := parseFlexibleTime(meeting.CreatedAt)
				if exists && created.IsZero() && archived.Metadata.Valid {
					created = decodeArchivedMeetingCreatedAt(archived.Metadata.String)
				}
				if exists && !refreshCreatedAfter.IsZero() && !created.IsZero() && created.Before(refreshCreatedAfter) {
					archivedState := transcriptState("")
					if archived.Metadata.Valid {
						archivedState = decodeArchivedTranscriptState(archived.Metadata.String)
					}
					// A later hard failure can retain this item-level pending
					// write while rolling the successful cursor back to a state
					// without its retry entry. Recover that durable signal before
					// applying the creation watermark so the retry is not stranded.
					if archivedState != transcriptStatePending {
						continue
					}
				}
			}
			searchedIDs = append(searchedIDs, id)
			processedIDs[id] = struct{}{}
			if limit > 0 && len(searchedIDs) == limit {
				limitReached = true
				break
			}
		}
		if limitReached {
			break
		}
		if newOnPage == 0 {
			sum.Errors++
			hardErrors = append(hardErrors,
				fmt.Errorf("search meetings page %d repeated previously seen meetings", pageIndex))
			err = errors.Join(hardErrors...)
			return sum, err
		}
	}
	progress(fmt.Sprintf("search worklist: %d newly searched meetings", len(searchedIDs)))
	if phaseErr := processWorkIDs(searchedIDs); phaseErr != nil {
		err = phaseErr
		return sum, err
	}

	if cancelIfDone(1) {
		return sum, err
	}
	if recomputeErr := imp.store.RecomputeConversationStats(src.ID); recomputeErr != nil {
		sum.Errors++
		hardErrors = append(hardErrors, fmt.Errorf("recompute conversation stats: %w", recomputeErr))
	}
	if cancelIfDone(1) {
		return sum, err
	}
	if len(hardErrors) > 0 {
		err = errors.Join(hardErrors...)
		return sum, err
	}

	// A limited search is a partial traversal. Advancing its watermark can
	// strand older meetings outside the next incremental overlap window.
	cursor := state.CreatedAfter
	if limit == 0 && !maxCreated.IsZero() {
		cursor = maxCreated.UTC().Format(time.RFC3339)
	}
	completedState := syncState{
		CreatedAfter:       cursor,
		PendingTranscripts: pendingTranscriptSlice(pendingByID),
	}
	if cancelIfDone(1) {
		return sum, err
	}
	if err = imp.store.CompleteSync(syncID, completedState.marshal()); err != nil {
		sum.Errors++
		return sum, err
	}
	sum.Duration = imp.now().Sub(start)
	return sum, nil
}

func (imp *Importer) archivedTranscriptState(msgID int64) (transcriptState, error) {
	metaNS, err := imp.store.GetMessageMetadata(msgID)
	if err != nil {
		return "", err
	}
	if !metaNS.Valid || metaNS.String == "" {
		return "", nil
	}
	return decodeArchivedTranscriptState(metaNS.String), nil
}

func decodeArchivedMeetingCreatedAt(encoded string) time.Time {
	var meta meetingMetadata
	if json.Unmarshal([]byte(encoded), &meta) != nil {
		return time.Time{}
	}
	return parseFlexibleTime(meta.CreatedAt)
}

func decodeArchivedTranscriptState(encoded string) transcriptState {
	var meta meetingMetadata
	if json.Unmarshal([]byte(encoded), &meta) != nil {
		return ""
	}
	return meta.TranscriptState
}

// recoverArchivedTranscript loads content from the composed raw archive when a
// refresh omits a transcript or returns a recognized-empty payload. Explicit
// transcript state is authoritative: a present transcript must have a valid
// archived payload, while pending and unavailable transcripts are not restored.
func (imp *Importer) recoverArchivedTranscript(msgID int64) (Transcript, bool, error) {
	metaNS, err := imp.store.GetMessageMetadata(msgID)
	if err != nil {
		return Transcript{}, false, err
	}
	if !metaNS.Valid || metaNS.String == "" {
		return Transcript{}, false, errors.New("archived meeting has no metadata")
	}
	var meta meetingMetadata
	if err := json.Unmarshal([]byte(metaNS.String), &meta); err != nil {
		return Transcript{}, false, fmt.Errorf("decode archived metadata: %w", err)
	}
	switch meta.TranscriptState {
	case transcriptStatePending, transcriptStateUnavailable:
		return Transcript{}, false, nil
	case transcriptStatePresent:
		// Continue to the required raw transcript archive below.
	default:
		return Transcript{}, false, fmt.Errorf(
			"archived meeting has invalid transcript state %q", meta.TranscriptState)
	}

	raw, err := imp.store.GetMessageRaw(msgID)
	if err != nil {
		return Transcript{}, false, fmt.Errorf("read composed raw: %w", err)
	}
	var composed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &composed); err != nil {
		return Transcript{}, false, fmt.Errorf("decode composed raw: %w", err)
	}
	transcriptRaw, ok := composed["transcript"]
	if !ok || len(transcriptRaw) == 0 {
		return Transcript{}, false, errors.New("composed raw has no transcript")
	}
	tr, err := decodeTranscript(transcriptRaw)
	if err != nil {
		return Transcript{}, false, fmt.Errorf("decode transcript raw: %w", err)
	}
	if !hasTranscriptContent(tr) {
		return Transcript{}, false, errors.New("composed raw transcript has no usable content")
	}
	return *tr, true, nil
}

type transcriptState string

const (
	transcriptStatePresent     transcriptState = "present"
	transcriptStatePending     transcriptState = "pending"
	transcriptStateUnavailable transcriptState = "unavailable"
)

// meetingMetadata is the structured JSON stored in messages.metadata.
type meetingMetadata struct {
	Platform           string              `json:"platform"`
	MeetingID          string              `json:"meeting_id"`
	CreatedAt          string              `json:"created_at,omitempty"`
	Start              string              `json:"scheduled_start,omitempty"`
	End                string              `json:"scheduled_end,omitempty"`
	DurationSeconds    int64               `json:"duration_seconds,omitempty"`
	OrganizerEmail     string              `json:"organizer_email,omitempty"`
	MeetingURL         string              `json:"meeting_url,omitempty"`
	RecordingURL       string              `json:"recording_url,omitempty"`
	RecordingFetchedAt string              `json:"recording_url_fetched_at,omitempty"`
	Tags               []string            `json:"tags,omitempty"`
	ActionItems        []metaActionItem    `json:"action_items,omitempty"`
	Insights           []map[string]string `json:"insights,omitempty"`
	TranscriptState    transcriptState     `json:"transcript_state"`
	TranscriptSegments int                 `json:"transcript_segments,omitempty"`
	AccountID          string              `json:"account_identifier,omitempty"`
	SnapshotHash       string              `json:"snapshot_hash,omitempty"`
}

type metaActionItem struct {
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	DueDate  string `json:"due_date,omitempty"`
}

func circlebackSnapshotHash(
	m *Meeting,
	tr *Transcript,
	identifier string,
	fromMe bool,
	state transcriptState,
) (string, error) {
	meetingJSON, err := canonicalProviderJSON(m.Raw, m)
	if err != nil {
		return "", fmt.Errorf("canonicalize meeting: %w", err)
	}
	var transcriptJSON json.RawMessage
	if tr != nil {
		transcriptJSON, err = canonicalProviderJSON(tr.Raw, tr)
		if err != nil {
			return "", fmt.Errorf("canonicalize transcript: %w", err)
		}
	}
	payload, err := json.Marshal(struct {
		Version         int             `json:"version"`
		Meeting         json.RawMessage `json:"meeting"`
		Transcript      json.RawMessage `json:"transcript,omitempty"`
		TranscriptState transcriptState `json:"transcript_state"`
		AccountID       string          `json:"account_identifier,omitempty"`
		IsFromMe        bool            `json:"is_from_me"`
	}{
		Version:         circlebackSnapshotVersion,
		Meeting:         meetingJSON,
		Transcript:      transcriptJSON,
		TranscriptState: state,
		AccountID:       identifier,
		IsFromMe:        fromMe,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalProviderJSON(raw json.RawMessage, fallback any) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.Marshal(fallback)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("provider JSON contains multiple values")
		}
		return nil, err
	}
	return json.Marshal(decoded)
}

// ingestMeeting persists one meeting through the canonical write path.
// Idempotent via UpsertMessage's ON CONFLICT(source_id, source_message_id).
// Returns whether the message row was newly inserted and whether the
// persisted archive changed. Existing rows with an identical stable snapshot
// are left untouched so no-op overlap reads do not invalidate derived caches.
func (imp *Importer) ingestMeeting(
	sourceID int64,
	identifier string,
	accountIdentities meetingidentity.Set,
	m *Meeting,
	tr *Transcript,
	desiredTranscriptState transcriptState,
	force bool,
) (bool, bool, error) {
	if string(m.ID) == "" {
		return false, false, errors.New("meeting has no id")
	}
	smid := "meeting:" + string(m.ID)

	existing, err := imp.store.MessageExistsBatch(sourceID, []string{smid})
	if err != nil {
		return false, false, fmt.Errorf("lookup existing meeting: %w", err)
	}
	existingID, existed := existing[smid]

	organizerEmail, organizerName := "", ""
	if m.Organizer != nil {
		organizerEmail = normalizeEmail(m.Organizer.Email)
		organizerName = m.Organizer.Name
	}
	fromMe := organizerEmail != "" && accountIdentities.Contains(organizerEmail)
	snapshotHash, err := circlebackSnapshotHash(
		m, tr, identifier, fromMe, desiredTranscriptState,
	)
	if err != nil {
		return false, false, fmt.Errorf("hash meeting snapshot: %w", err)
	}
	if existed && !force {
		metadata, metadataErr := imp.store.GetMessageMetadata(existingID)
		if metadataErr != nil {
			return false, false, fmt.Errorf("read existing meeting metadata: %w", metadataErr)
		}
		var archived meetingMetadata
		if metadata.Valid && json.Unmarshal([]byte(metadata.String), &archived) == nil &&
			archived.SnapshotHash == snapshotHash {
			return false, false, nil
		}
	}

	var senderID int64
	if organizerEmail != "" {
		id, err := imp.store.EnsureParticipant(organizerEmail, organizerName, emailDomain(organizerEmail))
		if err != nil {
			return false, false, fmt.Errorf("organizer participant: %w", err)
		}
		senderID = id
	}

	// Attendees WITH an email become participants/recipients; name-only
	// attendees appear in the body's Attendees line only, so we don't mint
	// phantom address-less participant rows.
	var attendeeIDs []int64
	var attendeeNames []string
	var attendeeEmails []string
	for _, a := range m.Attendees {
		email := normalizeEmail(a.Email)
		if email == "" {
			continue
		}
		pid, err := imp.store.EnsureParticipant(email, a.Name, emailDomain(email))
		if err != nil {
			return false, false, fmt.Errorf("attendee participant: %w", err)
		}
		attendeeIDs = append(attendeeIDs, pid)
		attendeeNames = append(attendeeNames, a.Name)
		attendeeEmails = append(attendeeEmails, email)
	}

	title := meetingTitle(m)
	participants := make([]store.ConversationParticipantRef, 0, len(attendeeIDs))
	for _, participantID := range attendeeIDs {
		participants = append(participants, store.ConversationParticipantRef{ParticipantID: participantID, Role: "member"})
	}

	body := buildBody(m, tr)
	sentAt := m.StartedAt().UTC()

	message := &store.Message{
		SourceID:        sourceID,
		SourceMessageID: smid,
		MessageType:     MessageType,
		SentAt:          sql.NullTime{Time: sentAt, Valid: !sentAt.IsZero()},
		SenderID:        sql.NullInt64{Int64: senderID, Valid: senderID != 0},
		IsFromMe:        fromMe,
		Subject:         sql.NullString{String: title, Valid: title != ""},
		Snippet:         sql.NullString{String: snippet(body), Valid: body != ""},
		SizeEstimate:    int64(len(body)),
	}

	metaJSON, err := json.Marshal(imp.buildMetadata(
		m, tr, identifier, organizerEmail, desiredTranscriptState, snapshotHash,
	))
	if err != nil {
		return false, false, fmt.Errorf("marshal metadata: %w", err)
	}
	metadata := sql.NullString{String: string(metaJSON), Valid: true}

	raw, err := composeRaw(m, tr)
	if err != nil {
		return false, false, fmt.Errorf("compose raw: %w", err)
	}
	// Replace recipients unconditionally so re-syncs clear stale rows
	// (calsync precedent).
	var fromIDs []int64
	var fromNames []string
	if senderID != 0 {
		fromIDs = []int64{senderID}
		fromNames = []string{organizerName}
	}
	fts := &store.FTSDoc{
		Subject:  title,
		Body:     body,
		FromAddr: organizerEmail,
		ToAddrs:  strings.Join(attendeeEmails, " "),
	}
	if _, err := imp.store.PersistMessage(&store.MessagePersistData{
		Message: message,
		Conversation: &store.ConversationPersistData{
			SourceConversationID: smid,
			ConversationType:     ConversationType,
			Title:                title,
			Participants:         participants,
		},
		Metadata:  &metadata,
		BodyText:  sql.NullString{String: body, Valid: body != ""},
		RawMIME:   raw,
		RawFormat: RawFormat,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: fromIDs, DisplayNames: fromNames},
			{Type: "to", ParticipantIDs: attendeeIDs, DisplayNames: attendeeNames},
		},
		PreserveLabels: true,
		FTS:            fts,
	}); err != nil {
		return false, false, fmt.Errorf("persist meeting: %w", err)
	}

	return !existed, true, nil
}

func (imp *Importer) buildMetadata(
	m *Meeting,
	tr *Transcript,
	identifier string,
	organizerEmail string,
	desiredTranscriptState transcriptState,
	snapshotHash string,
) meetingMetadata {
	meta := meetingMetadata{
		Platform:        SourceType,
		MeetingID:       string(m.ID),
		CreatedAt:       m.CreatedAt,
		Start:           m.StartTime,
		End:             m.EndTime,
		DurationSeconds: m.DurationSecs(),
		OrganizerEmail:  organizerEmail,
		MeetingURL:      m.PlatformURL(),
		RecordingURL:    m.RecordingURL,
		Tags:            m.Tags,
		TranscriptState: desiredTranscriptState,
		AccountID:       identifier,
		SnapshotHash:    snapshotHash,
	}
	if m.RecordingURL != "" {
		meta.RecordingFetchedAt = imp.now().UTC().Format(time.RFC3339)
	}
	for _, ai := range m.ActionItems {
		meta.ActionItems = append(meta.ActionItems, metaActionItem{
			Title:    ai.DisplayTitle(),
			Status:   ai.Status,
			Assignee: ai.AssigneeLabel(),
			DueDate:  ai.DueDate,
		})
	}
	for _, in := range m.Insights {
		entry := map[string]string{}
		if name := firstNonEmpty(in.Name, in.Title); name != "" {
			entry["name"] = name
		}
		if content := firstNonEmpty(in.Content, in.Value); content != "" {
			entry["content"] = content
		}
		if len(entry) > 0 {
			meta.Insights = append(meta.Insights, entry)
		}
	}
	if hasTranscriptContent(tr) {
		meta.TranscriptState = transcriptStatePresent
		meta.TranscriptSegments = len(tr.ContentEntries())
	}
	return meta
}

// composeRaw archives both tool payloads verbatim in one JSON object.
func composeRaw(m *Meeting, tr *Transcript) ([]byte, error) {
	payload := map[string]json.RawMessage{"meeting": m.Raw}
	if len(m.Raw) == 0 {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		payload["meeting"] = b
	}
	if tr != nil && len(tr.Raw) > 0 {
		payload["transcript"] = tr.Raw
	}
	return json.Marshal(payload)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return email[i+1:]
	}
	return ""
}
