package notionmeetings

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

const (
	syncStateVersion           = 1
	notionSnapshotVersion      = 2
	transcriptRetryCadence     = 6 * time.Hour
	transcriptRetryWindow      = 7 * 24 * time.Hour
	unknownTranscriptRetrySpan = 48 * time.Hour
)

// Source is the read-only Notion surface required by a sync run.
type Source interface {
	hydrationSource
	QueryMeetingNotes(ctx context.Context, limit int) (*QueryResult, error)
}

type Importer struct {
	users  UserSource
	store  *store.Store
	client Source
	now    func() time.Time
}

func NewImporter(st *store.Store, client Source) *Importer {
	return &Importer{store: st, client: client, now: time.Now}
}

// WithUserSource resolves attendees with a separate workspace users token.
func (imp *Importer) WithUserSource(users UserSource) *Importer {
	imp.users = users
	return imp
}

type ImportOptions struct {
	Identifier   string
	AccountEmail string
	Full         bool
	Limit        int
	CreatedAfter time.Time
	Progress     func(string)
}

type ImportSummary struct {
	SourceID           int64
	MeetingsProcessed  int64
	MeetingsAdded      int64
	MeetingsUpdated    int64
	MaintenanceRetries int64
	Errors             int64
	PartialCoverage    bool
	Duration           time.Duration
}

type knownMeeting struct {
	LastEditedTime  string `json:"last_edited_time"`
	SnapshotSHA256  string `json:"snapshot_sha256,omitempty"`
	SnapshotVersion int    `json:"snapshot_version"`
}

type pendingTranscript struct {
	BlockID       string `json:"block_id"`
	NextAttemptAt string `json:"next_attempt_at"`
	RetryUntil    string `json:"retry_until"`
}

type Coverage struct {
	HasMore    bool   `json:"has_more"`
	Returned   int    `json:"returned"`
	OldestEdit string `json:"oldest_edit,omitempty"`
	NewestEdit string `json:"newest_edit,omitempty"`
	Limited    bool   `json:"limited,omitzero"`
}

type syncState struct {
	Version              int                     `json:"version"`
	LastQueryAt          string                  `json:"last_query_at"`
	Known                map[string]knownMeeting `json:"known"`
	Pending              []pendingTranscript     `json:"pending,omitempty"`
	TranscriptRetryUntil map[string]string       `json:"transcript_retry_until,omitempty"`
	Coverage             Coverage                `json:"coverage"`
}

func (s syncState) marshal() (string, error) {
	sort.Slice(s.Pending, func(i, j int) bool { return s.Pending[i].BlockID < s.Pending[j].BlockID })
	encoded, err := json.Marshal(s, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("marshal Notion sync cursor: %w", err)
	}
	return string(encoded), nil
}

func (s syncState) validate() error {
	if s.Version != syncStateVersion {
		return fmt.Errorf("unsupported Notion sync cursor version %d", s.Version)
	}
	seen := make(map[string]struct{}, len(s.Pending))
	for index, pending := range s.Pending {
		if strings.TrimSpace(pending.BlockID) == "" {
			return fmt.Errorf("pending[%d] has a blank block_id", index)
		}
		if _, duplicate := seen[pending.BlockID]; duplicate {
			return errors.New("pending contains duplicate block_id")
		}
		seen[pending.BlockID] = struct{}{}
		if _, err := time.Parse(time.RFC3339, pending.NextAttemptAt); err != nil {
			return errors.New("pending.next_attempt_at must be an RFC3339 timestamp")
		}
		if _, err := time.Parse(time.RFC3339, pending.RetryUntil); err != nil {
			return errors.New("pending.retry_until must be an RFC3339 timestamp")
		}
	}
	deadlineIDs := make([]string, 0, len(s.TranscriptRetryUntil))
	for id := range s.TranscriptRetryUntil {
		deadlineIDs = append(deadlineIDs, id)
	}
	sort.Strings(deadlineIDs)
	for _, id := range deadlineIDs {
		if strings.TrimSpace(id) == "" {
			return errors.New("transcript retry deadline has a blank block ID")
		}
		if _, err := time.Parse(time.RFC3339, s.TranscriptRetryUntil[id]); err != nil {
			return errors.New("transcript_retry_until must be an RFC3339 timestamp")
		}
	}
	return nil
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("notion meeting importer is unavailable")
	}
	started := imp.now().UTC()
	source, err := imp.store.GetSourceByTypeAndIdentifier(ctx, SourceType, strings.TrimSpace(opts.Identifier))
	if err != nil {
		return nil, err
	}
	sum = &ImportSummary{SourceID: source.ID}
	if strings.TrimSpace(opts.AccountEmail) != "" {
		if err := imp.store.AddAccountIdentityContext(ctx, source.ID, opts.AccountEmail, "account-email"); err != nil {
			return sum, fmt.Errorf("confirm Notion account identity: %w", err)
		}
	}

	state := syncState{Version: syncStateVersion, Known: map[string]knownMeeting{}}
	previous, err := imp.store.GetLastSuccessfulSyncContext(ctx, source.ID)
	if err == nil && previous.CursorAfter.Valid && previous.CursorAfter.String != "" {
		if err := json.Unmarshal([]byte(previous.CursorAfter.String), &state); err != nil {
			return sum, fmt.Errorf("decode previous Notion sync cursor: %w", err)
		}
		if err := state.validate(); err != nil {
			return sum, fmt.Errorf("validate previous Notion sync cursor: %w", err)
		}
	} else if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return sum, fmt.Errorf("load previous Notion sync cursor: %w", err)
	}
	if state.Known == nil {
		state.Known = map[string]knownMeeting{}
	}
	if state.TranscriptRetryUntil == nil {
		state.TranscriptRetryUntil = map[string]string{}
	}

	syncID, err := imp.store.StartSync(source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scopedStore := imp.store.ScopedToSync(source.ID, syncID)
	defer func() {
		if retErr != nil {
			_ = scopedStore.FailSyncWithCheckpoint(syncID, retErr.Error(), &store.Checkpoint{
				MessagesProcessed: sum.MeetingsProcessed,
				MessagesAdded:     sum.MeetingsAdded,
				MessagesUpdated:   sum.MeetingsUpdated,
				ErrorsCount:       sum.Errors,
			})
		}
	}()

	result, err := imp.client.QueryMeetingNotes(ctx, maxQueryResults)
	if err != nil {
		sum.Errors++
		return sum, hydrationFailure("query Notion meeting notes", err)
	}
	if result == nil {
		sum.Errors++
		return sum, errors.New("query Notion meeting notes returned no response")
	}

	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	next := syncState{
		Version:              syncStateVersion,
		LastQueryAt:          imp.now().UTC().Format(time.RFC3339),
		Known:                map[string]knownMeeting{},
		TranscriptRetryUntil: make(map[string]string, len(state.TranscriptRetryUntil)),
		Coverage:             coverageFor(result, opts.Limit > 0),
	}
	maps.Copy(next.TranscriptRetryUntil, state.TranscriptRetryUntil)
	sum.PartialCoverage = result.HasMore
	if result.HasMore {
		progress(fmt.Sprintf("Notion discovery is partial: %d meetings returned and more are unavailable through pagination", len(result.Results)))
	}

	pendingByID := make(map[string]pendingTranscript, len(state.Pending))
	for _, pending := range state.Pending {
		if retained := next.TranscriptRetryUntil[pending.BlockID]; retained != "" {
			retainedAt, _ := time.Parse(time.RFC3339, retained)
			pendingUntil, _ := time.Parse(time.RFC3339, pending.RetryUntil)
			if retainedAt.After(pendingUntil) {
				pending.RetryUntil = retained
			}
		}
		next.TranscriptRetryUntil[pending.BlockID] = pending.RetryUntil
		pendingByID[pending.BlockID] = pending
		if known, ok := state.Known[pending.BlockID]; ok {
			next.Known[pending.BlockID] = known
		}
	}

	visible := make(map[string]MeetingNote, len(result.Results))
	work := make(map[string]MeetingNote)
	workOrder := make([]string, 0, len(result.Results)+len(state.Pending))
	due := make(map[string]bool)
	seen := make(map[string]struct{}, len(result.Results))
	for _, meeting := range result.Results {
		id := strings.TrimSpace(meeting.ID)
		if id == "" {
			sum.Errors++
			return sum, errors.New("notion discovery returned a meeting without an ID")
		}
		if _, duplicate := seen[id]; duplicate {
			sum.Errors++
			return sum, errors.New("notion discovery returned a duplicate meeting ID")
		}
		seen[id] = struct{}{}
		visible[id] = meeting
		if known, ok := state.Known[id]; ok {
			next.Known[id] = known
		}
	}

	now := imp.now().UTC()
	for id, pending := range pendingByID {
		if pendingExpired(pending, now) {
			delete(pendingByID, id)
			continue
		}
		if !pendingDue(pending, now) {
			continue
		}
		due[id] = true
		if meeting, ok := visible[id]; ok {
			work[id] = meeting
			workOrder = append(workOrder, id)
			continue
		}
		meeting, loadErr := imp.retrieveMeeting(ctx, id)
		if loadErr != nil {
			sum.Errors++
			if !errors.Is(loadErr, ErrRateLimited) {
				return sum, hydrationFailure("load pending Notion meeting", loadErr)
			}
			pending.NextAttemptAt = now.Add(transcriptRetryCadence).Format(time.RFC3339)
			pendingByID[id] = pending
			progress("pending Notion meeting: load failed; retry deferred")
			continue
		}
		work[id] = meeting
		workOrder = append(workOrder, id)
	}
	sum.MaintenanceRetries = int64(len(due))

	remaining := max(opts.Limit, 0)
	for _, meeting := range result.Results {
		id := meeting.ID
		if due[id] {
			continue
		}
		if !opts.CreatedAfter.IsZero() && candidateTime(meeting).Before(opts.CreatedAfter) {
			continue
		}
		if opts.Limit > 0 && remaining == 0 {
			continue
		}
		if opts.Limit > 0 {
			remaining--
		}
		work[id] = meeting
		workOrder = append(workOrder, id)
	}

	hydrator := NewHydrator(imp.client).WithUserSource(imp.users)
	archiver := meetingarchive.New(scopedStore)
	var hardErrors []error
	for _, id := range workOrder {
		if err := ctx.Err(); err != nil {
			hardErrors = append(hardErrors, err)
			break
		}
		meeting := work[id]
		sum.MeetingsProcessed++
		hydrated, hydrateErr := hydrator.Hydrate(ctx, meeting)
		if hydrateErr != nil {
			sum.Errors++
			hardErrors = append(hardErrors, hydrationFailure("hydrate Notion meeting", hydrateErr))
			progress("Notion meeting: hydration failed")
			continue
		}

		// Archived preservation can replace this tree with complete older evidence.
		// Maintenance follows the current fetch, including nonempty prefixes.
		providerTranscriptIncomplete := strings.TrimSpace(hydrated.Transcript) == "" ||
			!hydrated.TranscriptTree.Complete || hydrated.TranscriptTree.Filtered
		archived, recoverErr := imp.archivedState(ctx, source.ID, id)
		if recoverErr != nil {
			sum.Errors++
			hardErrors = append(hardErrors, hydrationFailure("recover archived Notion evidence", recoverErr))
			continue
		}
		keepArchive := preserveArchivedSections(hydrated, archived)
		if hydrated.AttendeeResolutionDegraded {
			preserveArchivedAttendees(hydrated, archived.ResolvedUsers)
			if archived.MessageID != 0 {
				recipients, recipientErr := imp.store.GetMessageRecipientsContext(ctx, archived.MessageID, "to")
				if recipientErr != nil {
					sum.Errors++
					hardErrors = append(hardErrors, hydrationFailure("recover archived Notion attendees", recipientErr))
					continue
				}
				preserveArchivedAttendeeRelationships(hydrated, recipients, archived.ResolvedUsers)
			}
		}

		terminal := terminalStatus(hydrated.Discovery.MeetingNotes.Status)
		if providerTranscriptIncomplete && !terminal {
			previousPending, hasPrevious := pendingByID[id]
			if retained := next.TranscriptRetryUntil[id]; retained != "" {
				previousPending.RetryUntil = retained
				hasPrevious = true
			}
			pending, keep := schedulePending(hydrated, now, previousPending, hasPrevious)
			if keep {
				pendingByID[id] = pending
				next.TranscriptRetryUntil[id] = pending.RetryUntil
			} else {
				delete(pendingByID, id)
				hydrated.Warnings = append(hydrated.Warnings, "transcript retry window expired")
			}
		} else {
			delete(pendingByID, id)
		}

		if keepArchive {
			sum.PartialCoverage = true
			progress("legacy_unscoped: archived meeting retained unchanged; current sections are incomplete")
			continue
		}

		snapshot, snapshotErr := hydrated.ArchiveSnapshot(source.ID, opts.Identifier, opts.AccountEmail)
		if snapshotErr != nil {
			sum.Errors++
			hardErrors = append(hardErrors, hydrationFailure("build Notion archive snapshot", snapshotErr))
			continue
		}
		checksum := checksumBytes(snapshot.Raw)
		known, exists := state.Known[id]
		if archived.HasEvidence && exists && !opts.Full && known.SnapshotVersion == notionSnapshotVersion &&
			known.SnapshotSHA256 == checksum {
			// The archived meeting is current, but attendee linking may not
			// have finished before an earlier run stopped.
			if _, linkErr := archiver.LinkIdentities(ctx, source.ID, snapshot.Attendees); linkErr != nil {
				sum.Errors++
				hardErrors = append(hardErrors, hydrationFailure("link Notion attendee identities", linkErr))
				continue
			}
			next.Known[id] = knownMeeting{
				LastEditedTime:  meeting.LastEditedTime,
				SnapshotSHA256:  checksum,
				SnapshotVersion: notionSnapshotVersion,
			}
			progress("verified Notion meeting")
			continue
		}
		archiveResult, archiveErr := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{
			Force: opts.Full,
		})
		if archiveResult.Created {
			sum.MeetingsAdded++
		} else if archiveResult.Changed {
			sum.MeetingsUpdated++
		}
		if archiveErr != nil {
			sum.Errors++
			hardErrors = append(hardErrors, hydrationFailure("archive Notion meeting", archiveErr))
			continue
		}
		next.Known[id] = knownMeeting{
			LastEditedTime:  meeting.LastEditedTime,
			SnapshotSHA256:  checksum,
			SnapshotVersion: notionSnapshotVersion,
		}
		progress("imported Notion meeting")
	}

	if len(hardErrors) > 0 {
		retErr = errors.Join(hardErrors...)
		return sum, retErr
	}
	for id, pending := range pendingByID {
		next.Pending = append(next.Pending, pending)
		if known, ok := state.Known[id]; ok {
			if _, exists := next.Known[id]; !exists {
				next.Known[id] = known
			}
		}
	}
	for id := range next.Known {
		if _, inWindow := visible[id]; inWindow {
			continue
		}
		if _, pending := pendingByID[id]; !pending {
			delete(next.Known, id)
		}
	}
	cursor, err := next.marshal()
	if err != nil {
		return sum, err
	}
	checkpoint := &store.Checkpoint{
		MessagesProcessed: sum.MeetingsProcessed,
		MessagesAdded:     sum.MeetingsAdded,
		MessagesUpdated:   sum.MeetingsUpdated,
		ErrorsCount:       sum.Errors,
	}
	if err := scopedStore.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
		return sum, err
	}
	if err := scopedStore.CompleteSync(syncID, cursor); err != nil {
		return sum, err
	}
	sum.Duration = imp.now().UTC().Sub(started)
	return sum, nil
}

func (imp *Importer) retrieveMeeting(ctx context.Context, id string) (MeetingNote, error) {
	block, err := imp.client.RetrieveBlock(ctx, id)
	if err != nil {
		return MeetingNote{}, err
	}
	var meeting MeetingNote
	if err := json.Unmarshal(block.Raw, &meeting); err != nil {
		return MeetingNote{}, fmt.Errorf("decode meeting-note block: %w", err)
	}
	if meeting.ID != id || meeting.Type != "meeting_notes" {
		return MeetingNote{}, errors.New("retrieved block has unexpected ID or type")
	}
	return meeting, nil
}

type archivedMeetingState struct {
	Evidence      rawEvidence
	HasEvidence   bool
	Body          string
	MessageID     int64
	ResolvedUsers []resolvedUser
}

func (imp *Importer) archivedState(ctx context.Context, sourceID int64, id string) (archivedMeetingState, error) {
	existing, err := imp.store.MessageExistsBatchContext(ctx, sourceID, []string{id})
	if err != nil {
		return archivedMeetingState{}, err
	}
	messageID, ok := existing[id]
	if !ok {
		return archivedMeetingState{}, nil
	}
	body, err := imp.store.GetMessageBodyText(ctx, messageID)
	if err != nil {
		return archivedMeetingState{}, err
	}
	archived := archivedMeetingState{Body: body, MessageID: messageID}
	metadata, err := imp.store.GetMessageMetadataContext(ctx, messageID)
	if err != nil {
		return archivedMeetingState{}, err
	}
	if metadata.Valid {
		var decoded meetingMetadata
		if json.Unmarshal([]byte(metadata.String), &decoded) == nil {
			archived.ResolvedUsers = decoded.ResolvedUsers
		}
	}
	raw, err := imp.store.GetMessageRawContext(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrInvalidMessageRaw) {
		return archived, nil
	}
	if err != nil {
		return archivedMeetingState{}, err
	}
	evidence, ok := decodeArchivedEvidence(raw, id)
	archived.Evidence = evidence
	archived.HasEvidence = ok
	if ok {
		archived.ResolvedUsers = evidence.ResolvedUsers
	}
	return archived, nil
}

func decodeArchivedEvidence(raw []byte, expectedID string) (rawEvidence, bool) {
	var evidence rawEvidence
	if json.Unmarshal(raw, &evidence) != nil {
		return rawEvidence{}, false
	}
	if evidence.SchemaVersion != 1 && evidence.SchemaVersion != notionSnapshotVersion {
		return rawEvidence{}, false
	}
	var discovery MeetingNote
	if json.Unmarshal(evidence.Discovery, &discovery) != nil ||
		discovery.ID != expectedID || discovery.Type != "meeting_notes" {
		return rawEvidence{}, false
	}
	var meetingBlock Block
	if json.Unmarshal(evidence.MeetingBlock, &meetingBlock) != nil ||
		meetingBlock.ID != expectedID || meetingBlock.Type != "meeting_notes" {
		return rawEvidence{}, false
	}
	if evidence.SchemaVersion == 1 {
		var markdown MarkdownPage
		if json.Unmarshal(evidence.PageMarkdown, &markdown) != nil || markdown.Object != "page_markdown" || strings.TrimSpace(markdown.ID) == "" {
			return rawEvidence{}, false
		}
	} else {
		content := meetingcontent.Decode(RawFormat, raw, nil)
		// Only the reconstructed admitted blocks can prove canonical text. An
		// invalid section or conflicting header must never be promoted as scoped.
		for _, reason := range []string{content.Summary.Reason, content.Notes.Reason, content.Transcript.Reason} {
			if reason != "" && reason != "incomplete_tree" {
				return rawEvidence{}, false
			}
		}
		if content.Summary.Text != evidence.Canonical.Summary || content.Notes.Text != evidence.Canonical.Notes || content.Transcript.Text != evidence.Canonical.Transcript {
			return rawEvidence{}, false
		}
		// A fresh tree belongs to the validated current header. Reject conflicting
		// provenance before preservation could bind it to an older source header.
		for _, tree := range []rawBlockTree{evidence.Summary, evidence.Notes, evidence.Transcript} {
			if len(tree.Root) > 0 && !tree.Preserved &&
				(tree.SourceLastEditedTime != discovery.LastEditedTime || len(tree.SourceMeetingBlock) > 0) {
				return rawEvidence{}, false
			}
		}
	}
	return evidence, true
}

// preserveArchivedSections never attributes legacy body or page Markdown to a
// meeting block. If attribution cannot be proved, the caller retains the whole
// old archive and reports the stale scope instead of writing a v2 snapshot.
func preserveArchivedSections(meeting *HydratedMeeting, archived archivedMeetingState) bool {
	if archived.MessageID == 0 {
		return false
	}
	old := archived.Evidence
	oldTexts := canonicalEvidence{
		Summary:    archivedBodySection(archived.Body, "Summary"),
		Notes:      archivedBodySection(archived.Body, "Notes"),
		Transcript: transcriptFromArchivedBody(archived.Body),
	}
	if archived.HasEvidence {
		oldTexts = old.Canonical
	}
	sections := []struct {
		name      string
		tree      *blockTree
		text      *string
		prior     rawBlockTree
		priorText string
	}{
		{"summary", &meeting.SummaryTree, &meeting.Summary, old.Summary, oldTexts.Summary},
		{"notes", &meeting.NotesTree, &meeting.Notes, old.Notes, oldTexts.Notes},
		{"transcript", &meeting.TranscriptTree, &meeting.Transcript, old.Transcript, oldTexts.Transcript},
	}
	for _, section := range sections {
		if strings.TrimSpace(section.priorText) == "" || (section.tree.Complete && !section.tree.Filtered && strings.TrimSpace(*section.text) != "") {
			continue
		}
		if !archived.HasEvidence || old.SchemaVersion != 2 {
			return true
		}
		tree, ok := restoreArchivedTree(section.prior)
		if !ok || renderTree(tree) != section.priorText {
			return true
		}
		tree.Preserved = true
		if len(tree.SourceMeetingBlock) == 0 {
			tree.SourceMeetingBlock = old.MeetingBlock
		}
		*section.tree = tree
		*section.text = section.priorText
		meeting.Warnings = append(meeting.Warnings, "provider returned incomplete "+section.name+"; preserved archived meeting-owned "+section.name)
	}
	return false
}

func restoreArchivedTree(raw rawBlockTree) (blockTree, bool) {
	if len(raw.Root) == 0 || len(raw.Blocks) == 0 || len(raw.Blocks) > maxHydrationBlocks {
		return blockTree{}, false
	}
	var root Block
	if json.Unmarshal(raw.Root, &root) != nil || root.ID == "" || root.Object != "block" || root.Type == "meeting_notes" {
		return blockTree{}, false
	}
	tree := blockTree{Root: &root, Complete: raw.Complete, Filtered: raw.Filtered, SourceLastEditedTime: raw.SourceLastEditedTime, SourceMeetingBlock: raw.SourceMeetingBlock}
	seen := map[string]bool{}
	for _, item := range raw.Blocks {
		var block Block
		if json.Unmarshal(item, &block) != nil || block.ID == "" || block.Object != "block" || block.Type == "meeting_notes" || seen[block.ID] {
			return blockTree{}, false
		}
		seen[block.ID] = true
		tree.Blocks = append(tree.Blocks, block)
	}
	if tree.Blocks[0].ID != root.ID {
		return blockTree{}, false
	}
	return tree, true
}

func archivedBodySection(body, label string) string {
	_, text, ok := strings.Cut(body, "\n\n"+label+":\n")
	if !ok {
		return ""
	}
	for _, next := range []string{"Summary", "Notes", "Transcript"} {
		if prefix, _, found := strings.Cut(text, "\n\n"+next+":\n"); found {
			text = prefix
		}
	}
	return strings.TrimSpace(text)
}

func transcriptFromArchivedBody(body string) string {
	const marker = "\n\nTranscript:\n"
	_, transcript, ok := strings.Cut(body, marker)
	if !ok {
		return ""
	}
	return strings.TrimSpace(transcript)
}

func preserveArchivedAttendees(meeting *HydratedMeeting, archived []resolvedUser) {
	if meeting == nil || len(archived) == 0 {
		return
	}

	archivedByID := make(map[string]resolvedUser, len(archived))
	for _, user := range archived {
		id := strings.TrimSpace(user.ID)
		email := normalizedUserEmail(user.Email)
		if id == "" || email == "" || !user.EmailVerified {
			continue
		}
		user.ID = id
		user.Name = strings.TrimSpace(user.Name)
		user.Email = email
		archivedByID[id] = user
	}
	if len(archivedByID) == 0 {
		return
	}

	currentByID := make(map[string]resolvedUser, len(meeting.ResolvedUsers))
	for _, user := range meeting.ResolvedUsers {
		currentByID[user.ID] = user
	}
	unresolved := make(map[string]struct{}, len(meeting.UnresolvedAttendeeIDs))
	for _, id := range meeting.UnresolvedAttendeeIDs {
		unresolved[id] = struct{}{}
	}

	attendeeIDs := meeting.Discovery.MeetingNotes.CalendarEvent.Attendees
	labels := make([]string, 0, len(attendeeIDs))
	attendees := make([]meetingarchive.Person, 0, len(attendeeIDs))
	resolved := make([]resolvedUser, 0, len(attendeeIDs))
	seenEmails := make(map[string]struct{}, len(attendeeIDs))
	seenResolvedIDs := make(map[string]struct{}, len(attendeeIDs))
	restored := 0
	for index, id := range attendeeIDs {
		label := id
		if index < len(meeting.AttendeeLabels) && strings.TrimSpace(meeting.AttendeeLabels[index]) != "" {
			label = strings.TrimSpace(meeting.AttendeeLabels[index])
		}

		user, ok := currentByID[id]
		if !ok {
			if _, isUnresolved := unresolved[id]; isUnresolved &&
				(meeting.failedAttendeeIDs == nil || meeting.failedAttendeeIDs[id]) {
				user, ok = archivedByID[id]
				if ok {
					restored++
					delete(unresolved, id)
				}
			}
		}
		if !ok {
			labels = append(labels, label)
			continue
		}

		user.ID = strings.TrimSpace(user.ID)
		user.Name = strings.TrimSpace(user.Name)
		user.Email = normalizedUserEmail(user.Email)
		if user.Name != "" {
			label = user.Name
		}
		labels = append(labels, label)
		if user.Email == "" || !user.EmailVerified {
			continue
		}
		if _, seen := seenResolvedIDs[user.ID]; !seen {
			resolved = append(resolved, user)
			seenResolvedIDs[user.ID] = struct{}{}
		}
		if _, seen := seenEmails[user.Email]; seen {
			continue
		}
		attendees = append(attendees, meetingarchive.Person{
			Name: user.Name, Email: user.Email, Anchor: userAnchor(user.ID),
		})
		seenEmails[user.Email] = struct{}{}
	}
	if restored == 0 {
		return
	}

	remainingUnresolved := make([]string, 0, len(meeting.UnresolvedAttendeeIDs))
	for _, id := range meeting.UnresolvedAttendeeIDs {
		if _, remains := unresolved[id]; remains {
			remainingUnresolved = append(remainingUnresolved, id)
		}
	}
	meeting.AttendeeLabels = labels
	meeting.Attendees = attendees
	meeting.ResolvedUsers = resolved
	meeting.UnresolvedAttendeeIDs = remainingUnresolved
	meeting.Warnings = append(meeting.Warnings, "user directory lookup failed; preserved previously verified attendees from archived evidence")
}

func preserveArchivedAttendeeRelationships(
	meeting *HydratedMeeting,
	archived []store.MessageRecipient,
	archivedUsers []resolvedUser,
) {
	if meeting == nil || len(archived) == 0 {
		return
	}
	currentByID := make(map[string]int, len(meeting.ResolvedUsers))
	emailUsers := make(map[string]map[int]struct{}, len(meeting.ResolvedUsers))
	addEmailUser := func(email string, userIndex int) {
		email = normalizedUserEmail(email)
		if email == "" {
			return
		}
		users := emailUsers[email]
		if users == nil {
			users = make(map[int]struct{})
			emailUsers[email] = users
		}
		users[userIndex] = struct{}{}
	}
	addAlias := func(userIndex int, email string) {
		email = normalizedUserEmail(email)
		user := &meeting.ResolvedUsers[userIndex]
		if email == "" || email == normalizedUserEmail(user.Email) {
			return
		}
		for _, alias := range user.EmailAliases {
			if strings.EqualFold(strings.TrimSpace(alias), email) {
				return
			}
		}
		user.EmailAliases = append(user.EmailAliases, email)
	}
	for index := range meeting.ResolvedUsers {
		user := &meeting.ResolvedUsers[index]
		id := strings.TrimSpace(user.ID)
		email := normalizedUserEmail(user.Email)
		if id == "" || email == "" || !user.EmailVerified {
			continue
		}
		user.ID = id
		user.Email = email
		currentByID[id] = index
		addEmailUser(email, index)
		for _, alias := range user.EmailAliases {
			addEmailUser(alias, index)
		}
	}
	for _, user := range archivedUsers {
		index, current := currentByID[strings.TrimSpace(user.ID)]
		if !current || !user.EmailVerified {
			continue
		}
		for _, email := range append([]string{user.Email}, user.EmailAliases...) {
			addEmailUser(email, index)
			addAlias(index, email)
		}
	}
	if len(emailUsers) == 0 {
		return
	}
	currentParticipants := make(map[int64]map[int]struct{}, len(emailUsers))
	for _, recipient := range archived {
		email := normalizedUserEmail(recipient.EmailAddress)
		users := emailUsers[email]
		if len(users) == 0 {
			continue
		}
		participantUsers := currentParticipants[recipient.ParticipantID]
		if participantUsers == nil {
			participantUsers = make(map[int]struct{})
			currentParticipants[recipient.ParticipantID] = participantUsers
		}
		for index := range users {
			participantUsers[index] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(meeting.Attendees)+len(archived))
	for _, attendee := range meeting.Attendees {
		email := normalizedUserEmail(attendee.Email)
		if email != "" {
			seen[email] = struct{}{}
		}
	}
	restored := 0
	for _, recipient := range archived {
		users := currentParticipants[recipient.ParticipantID]
		if len(users) == 0 {
			continue
		}
		email := normalizedUserEmail(recipient.EmailAddress)
		if email == "" {
			continue
		}
		for index := range users {
			addAlias(index, email)
		}
		if _, ok := seen[email]; ok {
			continue
		}
		meeting.Attendees = append(meeting.Attendees, meetingarchive.Person{
			Name: strings.TrimSpace(recipient.DisplayName), Email: email,
		})
		seen[email] = struct{}{}
		restored++
	}
	if restored > 0 {
		meeting.Warnings = append(meeting.Warnings,
			"user directory lookup failed; preserved previously verified attendees from archived relationships")
	}
}

func coverageFor(result *QueryResult, limited bool) Coverage {
	coverage := Coverage{HasMore: result.HasMore, Returned: len(result.Results), Limited: limited}
	for _, meeting := range result.Results {
		value := meeting.LastEditedTime
		if value == "" {
			continue
		}
		if coverage.OldestEdit == "" || value < coverage.OldestEdit {
			coverage.OldestEdit = value
		}
		if coverage.NewestEdit == "" || value > coverage.NewestEdit {
			coverage.NewestEdit = value
		}
	}
	return coverage
}

func candidateTime(meeting MeetingNote) time.Time {
	for _, value := range []string{
		meeting.MeetingNotes.CalendarEvent.StartTime,
		meeting.MeetingNotes.Recording.StartTime,
		meeting.CreatedTime,
	} {
		if parsed := parseNotionTime(value); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

func schedulePending(meeting *HydratedMeeting, now time.Time, previous pendingTranscript, hasPrevious bool) (pendingTranscript, bool) {
	_, endedAt := meeting.times()
	retryUntil := time.Time{}
	if hasPrevious {
		retryUntil, _ = time.Parse(time.RFC3339, previous.RetryUntil)
	}
	if !endedAt.IsZero() {
		deadline := endedAt.Add(transcriptRetryWindow)
		if deadline.After(retryUntil) {
			retryUntil = deadline
		}
	} else if retryUntil.IsZero() {
		retryUntil = now.Add(unknownTranscriptRetrySpan)
	}
	if !retryUntil.After(now) {
		return pendingTranscript{}, false
	}
	next := now.Add(transcriptRetryCadence)
	if next.After(retryUntil) {
		next = retryUntil
	}
	return pendingTranscript{
		BlockID:       meeting.Discovery.ID,
		NextAttemptAt: next.UTC().Format(time.RFC3339),
		RetryUntil:    retryUntil.UTC().Format(time.RFC3339),
	}, true
}

func pendingDue(pending pendingTranscript, now time.Time) bool {
	next, err := time.Parse(time.RFC3339, pending.NextAttemptAt)
	return err != nil || !next.After(now)
}

func pendingExpired(pending pendingTranscript, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, pending.RetryUntil)
	return err == nil && !until.After(now)
}

func terminalStatus(status string) bool {
	normalized := strings.ToLower(strings.TrimSpace(status))
	return strings.Contains(normalized, "fail") || strings.Contains(normalized, "delete")
}

func checksumBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
