package omi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

type Source interface {
	ListConversations(ctx context.Context, params ListParams) ([]Conversation, error)
}
type Importer struct {
	store    *store.Store
	client   Source
	pageSize int
}

func NewImporter(st *store.Store, client Source) *Importer {
	return &Importer{store: st, client: client}
}

type ImportOptions struct {
	Identifier   string
	AccountEmail string
	Full         bool
	Limit        int
	CreatedAfter time.Time
	ExplicitScan bool
	Progress     func(string)
}
type ImportSummary struct {
	SourceID          int64
	MeetingsProcessed int64
	MeetingsAdded     int64
	MeetingsUpdated   int64
	Errors            int64
	CheckpointSaved   bool
	PauseReason       error
	Duration          time.Duration
}

// rescanOverlap reaches back before the creation watermark so conversations
// that finish processing late, and recent edits, are re-read. Omi has no
// updated-since filter, so edits to older conversations need Full.
const rescanOverlap = 48 * time.Hour
const syncStateVersion = 1

// syncState is the JSON cursor persisted in sync_runs.cursor_after.
type syncState struct {
	Version int `json:"version"`
	// CreatedAfter is the RFC3339Nano max created_at seen by the last
	// complete, unbounded run.
	CreatedAfter string `json:"created_after,omitempty"`
}

// scanCheckpoint fixes the creation frame and confirms each window after reading ahead.
type scanCheckpoint struct {
	Version      int       `json:"version"`
	Full         bool      `json:"full"`
	Limit        int       `json:"limit"`
	After        time.Time `json:"after"`
	AccountEmail string    `json:"account_email"`
	Lower        time.Time `json:"lower"`
	Ceiling      time.Time `json:"ceiling"`
	Offset       int       `json:"offset"`
	Width        int       `json:"width"`
	AheadWidth   int       `json:"ahead_width"`
	// Nil distinguishes an unread window from an empty response.
	Behind      *[]string `json:"behind"`
	Ahead       *[]string `json:"ahead"`
	Verified    time.Time `json:"verified"`
	BoundaryIDs []string  `json:"boundary_ids"`
	// CompletedIDs prevents forced rewrites when a pass pauses within a page.
	CompletedIDs []string  `json:"completed_ids,omitempty"`
	Processed    int64     `json:"processed"`
	MaxCreated   time.Time `json:"max_created"`
}

// Import fetches conversations created since the stored watermark, because
// Omi allows only 25 transcript list requests per hour. Raw-snapshot equality
// suppresses unchanged writes; Full rescans history and repairs all derived
// projections. A fixed end_date bounds shifts from newly created data.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if opts.Limit < 0 {
		return nil, errors.New("omi limit cannot be negative")
	}
	start := time.Now().UTC()
	src, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Identifier)
	if err != nil {
		return nil, fmt.Errorf("omi source is not registered; run msgvault add-omi %s: %w", opts.Identifier, err)
	}
	sum = &ImportSummary{SourceID: src.ID}
	var state syncState
	prev, err := imp.store.GetLastSuccessfulSync(src.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return sum, fmt.Errorf("load previous Omi sync cursor: %w", err)
	}
	if prev != nil && prev.CursorAfter.Valid && prev.CursorAfter.String != "" {
		if err := json.Unmarshal([]byte(prev.CursorAfter.String), &state); err != nil {
			return sum, fmt.Errorf("decode Omi sync cursor: %w", err)
		}
		if state.Version != syncStateVersion {
			return sum, fmt.Errorf("unsupported Omi sync cursor version %d", state.Version)
		}
	}
	var watermark time.Time
	if state.CreatedAfter != "" {
		if watermark, err = time.Parse(time.RFC3339Nano, state.CreatedAfter); err != nil {
			return sum, fmt.Errorf("decode Omi sync watermark: %w", err)
		}
	}
	createdAfter := opts.CreatedAfter
	if !opts.Full && !watermark.IsZero() && watermark.Add(-rescanOverlap).After(createdAfter) {
		createdAfter = watermark.Add(-rescanOverlap)
	}
	scan := scanCheckpoint{Version: syncStateVersion, Full: opts.Full, Limit: opts.Limit, After: opts.CreatedAfter, AccountEmail: opts.AccountEmail, Lower: createdAfter, Ceiling: start, MaxCreated: watermark}
	prior, err := imp.store.GetLatestCheckpointedSyncByType(src.ID, SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return sum, fmt.Errorf("load Omi scan checkpoint: %w", err)
	}
	if prior != nil {
		var saved scanCheckpoint
		if err := json.Unmarshal([]byte(prior.CursorBefore.String), &saved); err != nil {
			return sum, fmt.Errorf("decode Omi scan checkpoint: %w", err)
		}
		if saved.Version != syncStateVersion {
			return sum, fmt.Errorf("unsupported Omi scan checkpoint version %d", saved.Version)
		}
		explicit := opts.ExplicitScan || opts.Full || opts.Limit != 0 || !opts.CreatedAfter.IsZero()
		compatible := !explicit || (saved.Full == opts.Full && saved.Limit == opts.Limit && saved.After.Equal(opts.CreatedAfter))
		if compatible && saved.AccountEmail == opts.AccountEmail && !saved.Ceiling.IsZero() && saved.Offset >= 0 && saved.Width > 0 && saved.Width <= PageSize && saved.AheadWidth >= 0 && saved.AheadWidth <= PageSize && saved.Processed >= 0 && (saved.Ahead == nil || saved.AheadWidth > 0) {
			scan = saved
			opts.Full, opts.Limit, opts.CreatedAfter = saved.Full, saved.Limit, saved.After
		} else if opts.Progress != nil {
			opts.Progress("Requested scan options or account identity replace the unfinished Omi scan.")
		}
	}
	if err := imp.store.AddAccountIdentityContext(ctx, src.ID, opts.AccountEmail, "account-email"); err != nil {
		return sum, err
	}
	syncID, err := imp.store.StartSync(src.ID, SourceType)
	if err != nil {
		return sum, err
	}
	st := imp.store.ScopedToSync(src.ID, syncID)
	defer func() {
		sum.Duration = time.Since(start)
		if retErr != nil {
			paused := syncPaused(ctx, retErr)
			var checkpointErr error
			if paused {
				checkpointErr = st.PauseSyncWithCheckpoint(syncID, checkpoint(sum, scan))
			} else {
				checkpointErr = st.FailSyncWithCheckpoint(syncID, retErr.Error(), checkpoint(sum, scan))
			}
			sum.CheckpointSaved = checkpointErr == nil
			if paused && checkpointErr == nil {
				sum.PauseReason, retErr = retErr, nil
			} else {
				retErr = errors.Join(retErr, checkpointErr)
			}
			if checkpointErr == nil && sum.MeetingsAdded+sum.MeetingsUpdated > 0 {
				jobctx.RecordProgress(ctx)
			}
		}
	}()
	if opts.Progress != nil {
		opts.Progress("API requests may wait for shared pacing or a provider cooldown.")
	}
	archiver := meetingarchive.New(st)
	requestSize := imp.pageSize
	if requestSize == 0 {
		requestSize = PageSize
	}
	save := func() error {
		if err := st.UpdateSyncCheckpoint(syncID, checkpoint(sum, scan)); err != nil {
			return err
		}
		jobctx.RecordProgress(ctx)
		if jobctx.PreemptionRequested(ctx) {
			return context.Canceled
		}
		return nil
	}
	markCompleted := func(c Conversation) {
		scan.CompletedIDs = append(scan.CompletedIDs, c.ID)
		scan.Processed++
	}
	rebased := false
	for {
		remaining := requestSize
		if opts.Limit > 0 {
			remaining = int(int64(opts.Limit) - scan.Processed)
			if remaining <= 0 {
				break
			}
		}
		if scan.Width == 0 {
			scan.Width = min(requestSize, remaining)
		}
		if scan.AheadWidth == 0 && (opts.Limit == 0 || remaining > scan.Width) {
			scan.AheadWidth = min(requestSize, remaining-scan.Width)
			if opts.Limit == 0 {
				scan.AheadWidth = requestSize
			}
		}
		params := ListParams{Limit: scan.Width, Offset: scan.Offset, CreatedAfter: scan.Lower, CreatedBefore: scan.Ceiling}
		readingAhead := scan.AheadWidth > 0 && scan.Ahead == nil
		if readingAhead {
			params.Limit, params.Offset = scan.AheadWidth, scan.Offset+scan.Width
		}
		page, err := imp.client.ListConversations(ctx, params)
		if err != nil {
			if !syncPaused(ctx, err) {
				sum.Errors++
			}
			return sum, err
		}
		ids := make([]string, 0, len(page))
		for _, row := range page {
			ids = append(ids, row.ID)
		}
		if readingAhead {
			scan.Ahead = &ids
			if err := save(); err != nil {
				return sum, err
			}
			continue
		}
		if scan.Behind != nil && !slices.Equal(ids, *scan.Behind) {
			if !scan.Verified.IsZero() {
				scan.Ceiling = scan.Verified
			}
			scan.Offset, scan.Width, scan.AheadWidth, scan.Behind, scan.Ahead = 0, min(requestSize, remaining), 0, nil, nil
			if rebased {
				sum.Errors++
				return sum, errors.New("omi timestamp pagination is incomplete; retry the saved scan")
			}
			rebased = true
			if err := save(); err != nil {
				return sum, err
			}
			continue
		}
		unseen := func(c Conversation) bool {
			if slices.Contains(scan.CompletedIDs, c.ID) {
				return false
			}
			return scan.Verified.IsZero() || c.CreatedAt.Before(scan.Verified) || (c.CreatedAt.Equal(scan.Verified) && !slices.Contains(scan.BoundaryIDs, c.ID))
		}
		if opts.Limit > 0 && scan.Ahead == nil {
			distinct := 0
			for _, row := range page {
				if unseen(row) {
					distinct++
				}
			}
			if distinct < remaining {
				scan.Behind, scan.AheadWidth = &ids, min(requestSize, remaining-distinct)
				if err := save(); err != nil {
					return sum, err
				}
				continue
			}
		}
		for _, c := range page {
			if opts.Limit > 0 && scan.Processed >= int64(opts.Limit) {
				break
			}
			if !unseen(c) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			sum.MeetingsProcessed++
			content := meetingcontent.Decode(RawFormat, c.Raw, nil)
			snapshot, err := snapshot(src.ID, c, content)
			if err != nil {
				sum.Errors++
				return sum, err
			}
			if content.Transcript.State == meetingcontent.StateUnavailable {
				// A legitimate summary-only response must not erase previously
				// archived transcript evidence, even during a forced refresh.
				existing, err := st.MessageExistsBatch(src.ID, []string{c.ID})
				if err != nil {
					return sum, fmt.Errorf("omi find existing conversation: %w", err)
				}
				if id := existing[c.ID]; id != 0 {
					raw, err := st.GetMessageRawContext(ctx, id)
					if err != nil {
						return sum, fmt.Errorf("omi read existing evidence: %w", err)
					}
					prior := meetingcontent.Decode(RawFormat, raw, nil).Transcript
					if prior.State == meetingcontent.StateAvailable || prior.State == meetingcontent.StateEmpty {
						if opts.Progress != nil {
							opts.Progress(fmt.Sprintf("kept archived transcript for Omi conversation %s; the response omitted it", c.ID))
						}
						markCompleted(c)
						continue
					}
				}
			}
			// Omi does not provide organizer emails. Account ownership and speaker
			// labels are not evidence that the user organized a conversation.
			snapshot.AccountEmail = opts.AccountEmail
			result, err := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
			if result.Changed {
				if result.Created {
					sum.MeetingsAdded++
				} else {
					sum.MeetingsUpdated++
				}
			}
			if err != nil {
				if !syncPaused(ctx, err) {
					sum.Errors++
				}
				return sum, err
			}
			markCompleted(c)
		}
		for _, c := range page {
			if scan.Verified.IsZero() || c.CreatedAt.Before(scan.Verified) {
				scan.Verified, scan.BoundaryIDs = c.CreatedAt, nil
			}
			if c.CreatedAt.Equal(scan.Verified) && !slices.Contains(scan.BoundaryIDs, c.ID) {
				scan.BoundaryIDs = append(scan.BoundaryIDs, c.ID)
			}
			if c.CreatedAt.After(scan.MaxCreated) {
				scan.MaxCreated = c.CreatedAt
			}
		}
		scan.CompletedIDs = nil
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("processed %d Omi conversations", sum.MeetingsProcessed))
		}
		if scan.Ahead == nil || len(*scan.Ahead) == 0 {
			if err := save(); err != nil {
				return sum, err
			}
			break
		}
		scan.Offset, scan.Width, scan.Behind, scan.Ahead, scan.AheadWidth = scan.Offset+scan.Width, scan.AheadWidth, scan.Ahead, nil, 0
		if err := save(); err != nil {
			return sum, err
		}
	}

	if err := st.UpdateSyncCheckpoint(syncID, checkpoint(sum, scan)); err != nil {
		return sum, err
	}
	// A limited or creation-bounded run leaves older conversations unread, so
	// only a complete run may advance the watermark.
	if opts.Limit == 0 && opts.CreatedAfter.IsZero() && !scan.MaxCreated.IsZero() {
		state.CreatedAfter = scan.MaxCreated.UTC().Format(time.RFC3339Nano)
	}
	state.Version = syncStateVersion
	cursor, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return sum, err
	}
	if err := st.CompleteSync(syncID, string(cursor)); err != nil {
		return sum, err
	}
	return sum, nil
}

func checkpoint(sum *ImportSummary, scan scanCheckpoint) *store.Checkpoint {
	token, _ := json.Marshal(scan, json.Deterministic(true))
	return &store.Checkpoint{PageToken: string(token), MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}
}

func snapshot(sourceID int64, c Conversation, content meetingcontent.Content) (meetingarchive.Snapshot, error) {
	if content.Transcript.State == meetingcontent.StateUnavailable {
		// DeveloperConversation permits missing transcripts, including title-only records.
		if content.Transcript.Reason != "missing_field" {
			return meetingarchive.Snapshot{}, fmt.Errorf("omi conversation %s has unavailable transcript evidence (%s)", c.ID, content.Transcript.Reason)
		}
	}
	if content.Summary.State == meetingcontent.StateUnavailable && content.Summary.Reason != "missing_field" {
		return meetingarchive.Snapshot{}, fmt.Errorf("omi conversation %s has unavailable summary evidence (%s)", c.ID, content.Summary.Reason)
	}
	var body strings.Builder
	if content.Summary.Text != "" {
		body.WriteString(content.Summary.Text + "\n\n")
	}
	if len(content.Actions) > 0 {
		body.WriteString("Action items\n")
		for _, a := range content.Actions {
			fmt.Fprintf(&body, "- %s (%s)\n", a.Title, a.Status)
		}
		body.WriteString("\n")
	}
	for _, s := range content.Transcript.Segments {
		if s.OffsetSeconds == nil {
			fmt.Fprintf(&body, "%s: %s\n", s.Speaker, s.Text)
		} else {
			offset := time.Duration(*s.OffsetSeconds * float64(time.Second))
			body.WriteString(meetingarchive.FormatTranscriptLine(offset, s.Speaker, s.Text) + "\n")
		}
	}
	started := c.StartedAt
	if started.IsZero() {
		started = c.CreatedAt
	}
	title := c.Structured.Title
	if title == "" {
		title = "Omi conversation"
	}
	text := strings.TrimSpace(body.String())
	snippet := meetingarchive.Snippet(text)
	meta, err := json.Marshal(struct {
		Platform       string `json:"platform"`
		ConversationID string `json:"conversation_id"`
		SegmentCount   int    `json:"transcript_segments"`
	}{SourceType, c.ID, len(content.Transcript.Segments)})
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	return meetingarchive.Snapshot{SourceID: sourceID, SourceMessageID: c.ID, SourceConversationID: "meeting:" + c.ID, Title: title, StartedAt: started, Body: text, Snippet: snippet, Metadata: meta, Raw: c.Raw, RawFormat: RawFormat}, nil
}

// Every independent cause must be a resumable stop from this pass.
//
//nolint:errorlint // Exact unwrapped leaf identity keeps independent HTTP timeouts fatal.
func syncPaused(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !syncPaused(ctx, cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return syncPaused(ctx, wrapped.Unwrap())
	}
	if _, ok := err.(*CooldownError); ok {
		return ctx.Err() == nil || context.Cause(ctx) == jobctx.ErrRunBudgetExceeded || jobctx.YieldedToWaiter(ctx)
	}
	if context.Cause(ctx) == jobctx.ErrRunBudgetExceeded {
		return err == context.DeadlineExceeded || err == context.Canceled || err == jobctx.ErrRunBudgetExceeded
	}
	if jobctx.YieldedToWaiter(ctx) {
		return err == context.Canceled || err == jobctx.ErrYieldedToWaiter
	}
	return ctx.Err() == nil && jobctx.PreemptionRequested(ctx) && err == context.Canceled
}
