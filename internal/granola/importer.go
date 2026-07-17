package granola

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

// Importer ingests Granola meeting notes into the msgvault store.
type Importer struct {
	store  *store.Store
	client *Client
}

// NewImporter creates an Importer backed by the given store and API client.
func NewImporter(s *store.Store, c *Client) *Importer {
	return &Importer{store: s, client: c}
}

// ImportOptions controls a sync run.
type ImportOptions struct {
	// Identifier names the source row (the configured account label/email).
	Identifier string
	// AccountEmail is the configured primary identity for organizer
	// attribution. Stored aliases for the source are included automatically.
	AccountEmail string
	// Full ignores the stored updated_after watermark and re-fetches
	// everything (bounded by CreatedAfter when set).
	Full bool
	// Limit caps the number of notes processed this run (0 = unlimited).
	Limit int
	// CreatedAfter bounds a full sync to notes created after this time.
	CreatedAfter time.Time
	// Progress, when set, receives one-line status updates.
	Progress func(string)
}

// ImportSummary reports what a run did.
type ImportSummary struct {
	SourceID       int64
	NotesProcessed int64
	NotesAdded     int64
	NotesUpdated   int64
	Errors         int64
	Duration       time.Duration
}

// syncState is the JSON cursor persisted in sync_runs.cursor_after.
type syncState struct {
	// UpdatedAfter is the RFC3339Nano max updated_at across all notes
	// ingested by the last fully-successful run.
	UpdatedAfter    string   `json:"updated_after"`
	UpdatedAfterIDs []string `json:"updated_after_ids,omitempty"`
}

func (s syncState) marshal() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Import runs a full or incremental import for the configured account.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	src, err := imp.store.GetOrCreateSource(SourceType, opts.Identifier)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}
	accountIdentities, err := meetingidentity.ForSource(imp.store, src.ID, opts.AccountEmail)
	if err != nil {
		return nil, err
	}

	var state syncState
	if prev, perr := imp.store.GetLastSuccessfulSync(src.ID); perr == nil && prev != nil && prev.CursorAfter.Valid {
		_ = json.Unmarshal([]byte(prev.CursorAfter.String), &state)
	}

	syncID, err := imp.store.StartSync(src.ID, SourceType)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = imp.store.FailSyncWithCheckpoint(syncID, err.Error(), &store.Checkpoint{
				MessagesProcessed: sum.NotesProcessed,
				MessagesAdded:     sum.NotesAdded,
				MessagesUpdated:   sum.NotesUpdated,
				ErrorsCount:       sum.Errors,
			})
		}
	}()

	var cursorUpdatedAt time.Time
	if state.UpdatedAfter != "" {
		cursorUpdatedAt, _ = time.Parse(time.RFC3339Nano, state.UpdatedAfter)
	}
	params := ListNotesParams{PageSize: maxPageSize}
	if !opts.Full && !cursorUpdatedAt.IsZero() {
		// Granola's updated_after bound is strict. Overlap the exact boundary
		// so notes first exposed later with the same timestamp remain visible;
		// the boundary ID set below suppresses notes already covered there.
		params.UpdatedAfter = cursorUpdatedAt.Add(-time.Nanosecond)
	}
	if opts.Full && !opts.CreatedAfter.IsZero() {
		params.CreatedAfter = opts.CreatedAfter
	}

	// maxUpdated tracks the new watermark. It only advances past notes that
	// were actually ingested, and the cursor is only persisted when the run
	// had zero fetch errors — a failed note would otherwise be skipped
	// forever. IDs at the exact boundary avoid rewriting unchanged overlap.
	maxUpdated := cursorUpdatedAt
	previousBoundaryIDs := make(map[string]struct{}, len(state.UpdatedAfterIDs))
	cursorBoundaryIDs := make(map[string]struct{}, len(state.UpdatedAfterIDs))
	maxBoundaryIDs := make(map[string]struct{}, len(state.UpdatedAfterIDs))
	for _, id := range state.UpdatedAfterIDs {
		previousBoundaryIDs[id] = struct{}{}
		cursorBoundaryIDs[id] = struct{}{}
		maxBoundaryIDs[id] = struct{}{}
	}

	err = imp.forEachNote(ctx, src.ID, accountIdentities, params, opts, sum, func(n NoteSummary) bool {
		if opts.Full || cursorUpdatedAt.IsZero() || !n.UpdatedAt.Equal(cursorUpdatedAt) {
			return false
		}
		_, covered := previousBoundaryIDs[n.ID]
		return covered
	}, func(n *Note) {
		if !cursorUpdatedAt.IsZero() && n.UpdatedAt.Equal(cursorUpdatedAt) {
			cursorBoundaryIDs[n.ID] = struct{}{}
		}
		if n.UpdatedAt.After(maxUpdated) {
			maxUpdated = n.UpdatedAt
			clear(maxBoundaryIDs)
			maxBoundaryIDs[n.ID] = struct{}{}
		} else if n.UpdatedAt.Equal(maxUpdated) {
			maxBoundaryIDs[n.ID] = struct{}{}
		}
	})
	if err != nil {
		return sum, err
	}

	if err = imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: sum.NotesProcessed,
		MessagesAdded:     sum.NotesAdded,
		MessagesUpdated:   sum.NotesUpdated,
		ErrorsCount:       sum.Errors,
	}); err != nil {
		return sum, err
	}
	if err = imp.store.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}
	if sum.Errors > 0 {
		err = fmt.Errorf("partial Granola sync: %d note(s) failed", sum.Errors)
		return sum, err
	}

	// A limited or created-after full run deliberately leaves notes
	// unprocessed, so it cannot establish a safe incremental baseline even
	// when every processed note succeeded. The next unbounded run must
	// traverse from the prior cursor.
	cursor := state.UpdatedAfter
	cursorIDs := state.UpdatedAfterIDs
	boundedFull := opts.Full && !opts.CreatedAfter.IsZero()
	if opts.Limit == 0 && !boundedFull && !maxUpdated.IsZero() {
		cursor = maxUpdated.UTC().Format(time.RFC3339Nano)
		cursorIDs = make([]string, 0, len(maxBoundaryIDs))
		for id := range maxBoundaryIDs {
			cursorIDs = append(cursorIDs, id)
		}
		slices.Sort(cursorIDs)
	} else if !cursorUpdatedAt.IsZero() {
		// A bounded traversal cannot advance the timestamp, but remembering
		// successfully ingested IDs exactly at that timestamp lets repeated
		// limited runs make progress through a shared boundary.
		cursorIDs = make([]string, 0, len(cursorBoundaryIDs))
		for id := range cursorBoundaryIDs {
			cursorIDs = append(cursorIDs, id)
		}
		slices.Sort(cursorIDs)
	}
	if err = imp.store.CompleteSync(syncID, syncState{
		UpdatedAfter: cursor, UpdatedAfterIDs: cursorIDs,
	}.marshal()); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

// forEachNote pages through the list endpoint, fetches each note in full,
// ingests it, and reports successfully-ingested notes to onIngested. Summaries
// already covered at the cursor boundary do not consume the processing limit.
func (imp *Importer) forEachNote(ctx context.Context, sourceID int64, accountIdentities meetingidentity.Set, params ListNotesParams, opts ImportOptions, sum *ImportSummary, alreadyCovered func(NoteSummary) bool, onIngested func(*Note)) error {
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	seenCursors := make(map[string]struct{})
	for {
		page, err := imp.client.ListNotes(ctx, params)
		if err != nil {
			return fmt.Errorf("list notes: %w", err)
		}
		for _, ns := range page.Notes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if alreadyCovered(ns) {
				continue
			}
			if opts.Limit > 0 && sum.NotesProcessed >= int64(opts.Limit) {
				return nil
			}
			sum.NotesProcessed++
			note, err := imp.client.GetNote(ctx, ns.ID)
			if err != nil {
				sum.Errors++
				progress(fmt.Sprintf("note %s: fetch failed: %v", ns.ID, err))
				continue
			}
			if note.ID == "" || note.ID != ns.ID {
				sum.Errors++
				progress(fmt.Sprintf("note %s: fetch returned invalid ID %q", ns.ID, note.ID))
				continue
			}
			added, err := imp.ingestNote(sourceID, opts.Identifier, accountIdentities, note)
			if err != nil {
				sum.Errors++
				progress(fmt.Sprintf("note %s: ingest failed: %v", ns.ID, err))
				continue
			}
			if added {
				sum.NotesAdded++
			} else {
				sum.NotesUpdated++
			}
			onIngested(note)
			progress(fmt.Sprintf("imported %q (%s)", noteTitle(note), ns.ID))
		}
		if !page.HasMore {
			return nil
		}
		if page.Cursor == "" {
			return errors.New("list notes: response has more notes but no cursor")
		}
		if _, ok := seenCursors[page.Cursor]; ok {
			return fmt.Errorf("list notes: repeated page cursor %q", page.Cursor)
		}
		seenCursors[page.Cursor] = struct{}{}
		params.Cursor = page.Cursor
	}
}

// meetingMetadata is the structured JSON stored in messages.metadata.
type meetingMetadata struct {
	Platform        string   `json:"platform"`
	NoteID          string   `json:"note_id"`
	WebURL          string   `json:"web_url,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
	UpdatedAt       string   `json:"updated_at,omitempty"`
	ScheduledStart  string   `json:"scheduled_start,omitempty"`
	ScheduledEnd    string   `json:"scheduled_end,omitempty"`
	DurationSeconds int64    `json:"duration_seconds,omitempty"`
	OrganizerEmail  string   `json:"organizer_email,omitempty"`
	CalendarEventID string   `json:"calendar_event_id,omitempty"`
	Folders         []string `json:"folders,omitempty"`
	SegmentCount    int      `json:"transcript_segments,omitempty"`
	AccountID       string   `json:"account_identifier,omitempty"`
}

// ingestNote persists one note through the canonical write path. Idempotent
// via UpsertMessage's ON CONFLICT(source_id, source_message_id). Returns
// whether the message row was newly inserted.
func (imp *Importer) ingestNote(sourceID int64, identifier string, accountIdentities meetingidentity.Set, n *Note) (bool, error) {
	existing, err := imp.store.MessageExistsBatch(sourceID, []string{n.ID})
	if err != nil {
		return false, fmt.Errorf("lookup existing note: %w", err)
	}
	_, existed := existing[n.ID]

	organizerEmail, organizerName := organizer(n)

	var senderID int64
	if organizerEmail != "" {
		id, err := imp.store.EnsureParticipant(organizerEmail, organizerName, emailDomain(organizerEmail))
		if err != nil {
			return false, fmt.Errorf("organizer participant: %w", err)
		}
		senderID = id
	}

	var attendeeIDs []int64
	var attendeeNames []string
	var attendeeEmails []string
	for _, a := range attendees(n) {
		pid, err := imp.store.EnsureParticipant(a.Email, a.Name, emailDomain(a.Email))
		if err != nil {
			return false, fmt.Errorf("attendee participant: %w", err)
		}
		attendeeIDs = append(attendeeIDs, pid)
		attendeeNames = append(attendeeNames, a.Name)
		attendeeEmails = append(attendeeEmails, a.Email)
	}

	title := noteTitle(n)
	participants := make([]store.ConversationParticipantRef, 0, len(attendeeIDs))
	for _, participantID := range attendeeIDs {
		participants = append(participants, store.ConversationParticipantRef{ParticipantID: participantID, Role: "member"})
	}

	body := buildBody(n)
	fromMe := organizerEmail != "" && accountIdentities.Contains(organizerEmail)
	sentAt := noteStartTime(n).UTC()

	message := &store.Message{
		SourceID:        sourceID,
		SourceMessageID: n.ID,
		MessageType:     MessageType,
		SentAt:          sql.NullTime{Time: sentAt, Valid: !sentAt.IsZero()},
		SenderID:        sql.NullInt64{Int64: senderID, Valid: senderID != 0},
		IsFromMe:        fromMe,
		Subject:         sql.NullString{String: title, Valid: title != ""},
		Snippet:         sql.NullString{String: snippet(body), Valid: body != ""},
		SizeEstimate:    int64(len(body)),
	}

	metaJSON, err := json.Marshal(buildMetadata(n, identifier, organizerEmail))
	if err != nil {
		return false, fmt.Errorf("marshal metadata: %w", err)
	}
	metadata := sql.NullString{String: string(metaJSON), Valid: true}

	raw := []byte(n.Raw)
	if len(raw) == 0 {
		if raw, err = json.Marshal(n); err != nil {
			return false, fmt.Errorf("marshal raw note: %w", err)
		}
	}
	// Replace recipients unconditionally (even with empty sets) so a re-sync
	// that lost its organizer or attendees clears the stale rows (calsync
	// precedent).
	var fromIDs []int64
	var fromNames []string
	if senderID != 0 {
		fromIDs = []int64{senderID}
		fromNames = []string{organizerName}
	}
	// FTS: raw attendee emails go ONLY through the toAddrs column, never the
	// body, so ranking doesn't double-count them.
	fts := &store.FTSDoc{
		Subject:  title,
		Body:     body,
		FromAddr: organizerEmail,
		ToAddrs:  strings.Join(attendeeEmails, " "),
	}
	if _, err := imp.store.PersistMessage(&store.MessagePersistData{
		Message: message,
		Conversation: &store.ConversationPersistData{
			SourceConversationID: "meeting:" + n.ID,
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
		return false, fmt.Errorf("persist note: %w", err)
	}

	return !existed, nil
}

// organizer resolves the meeting organizer: the calendar event's organiser
// email when present, else the note owner. The display name comes from the
// matching attendee (the calendar payload carries emails only).
func organizer(n *Note) (email, name string) {
	if n.CalendarEvent != nil && n.CalendarEvent.Organiser != "" {
		email = normalizeEmail(n.CalendarEvent.Organiser)
		for _, a := range n.Attendees {
			if strings.EqualFold(a.Email, email) {
				return email, a.Name
			}
		}
		if strings.EqualFold(n.Owner.Email, email) {
			return email, n.Owner.Name
		}
		return email, ""
	}
	return normalizeEmail(n.Owner.Email), n.Owner.Name
}

// attendees returns the recipient list: note attendees when present, else the
// calendar invitees (email-only). Entries without an email are dropped.
func attendees(n *Note) []User {
	var out []User
	for _, a := range n.Attendees {
		if e := normalizeEmail(a.Email); e != "" {
			out = append(out, User{Name: a.Name, Email: e})
		}
	}
	if len(out) > 0 || n.CalendarEvent == nil {
		return out
	}
	for _, inv := range n.CalendarEvent.Invitees {
		if e := normalizeEmail(inv.Email); e != "" {
			out = append(out, User{Email: e})
		}
	}
	return out
}

func buildMetadata(n *Note, identifier, organizerEmail string) meetingMetadata {
	m := meetingMetadata{
		Platform:       SourceType,
		NoteID:         n.ID,
		WebURL:         n.WebURL,
		CreatedAt:      n.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      n.UpdatedAt.UTC().Format(time.RFC3339),
		OrganizerEmail: organizerEmail,
		SegmentCount:   len(n.Transcript),
		AccountID:      identifier,
	}
	if ce := n.CalendarEvent; ce != nil {
		m.CalendarEventID = ce.CalendarEventID
		if !ce.ScheduledStartTime.IsZero() {
			m.ScheduledStart = ce.ScheduledStartTime.UTC().Format(time.RFC3339)
		}
		if !ce.ScheduledEndTime.IsZero() {
			m.ScheduledEnd = ce.ScheduledEndTime.UTC().Format(time.RFC3339)
		}
		if !ce.ScheduledStartTime.IsZero() && ce.ScheduledEndTime.After(ce.ScheduledStartTime) {
			m.DurationSeconds = int64(ce.ScheduledEndTime.Sub(ce.ScheduledStartTime).Seconds())
		}
	}
	if m.DurationSeconds == 0 && len(n.Transcript) > 0 {
		var transcriptEnd time.Time
		for _, segment := range slices.Backward(n.Transcript) {
			if !segment.EndTime.IsZero() {
				transcriptEnd = segment.EndTime
				break
			}
		}
		span := transcriptEnd.Sub(transcriptStartTime(n))
		if span > 0 {
			m.DurationSeconds = int64(span.Seconds())
		}
	}
	for _, f := range n.FolderMembership {
		m.Folders = append(m.Folders, f.Name)
	}
	return m
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
