package omi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

type Source interface {
	ListConversations(ctx context.Context, params ListParams) ([]Conversation, error)
}
type Importer struct {
	store  *store.Store
	client Source
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
	Progress     func(string)
}
type ImportSummary struct {
	SourceID          int64
	MeetingsProcessed int64
	MeetingsAdded     int64
	MeetingsUpdated   int64
	Errors            int64
	Duration          time.Duration
}

// Import rescans the accessible history because Omi has no updated-since
// filter. Raw-snapshot equality suppresses unchanged writes; Full repairs all
// derived projections. A fixed end_date bounds shifts from newly created data.
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
			_ = st.FailSyncWithCheckpoint(syncID, retErr.Error(), checkpoint(sum))
		}
	}()
	archiver := meetingarchive.New(st)
	params := ListParams{CreatedBefore: start}
	seen := make(map[string]bool)
	var rowErr error
	for {
		pageSize := PageSize
		if opts.Limit > 0 {
			remaining := int64(opts.Limit) - sum.MeetingsProcessed
			if remaining <= 0 {
				break
			}
			pageSize = min(pageSize, int(remaining))
		}
		params.Limit = pageSize
		page, err := imp.client.ListConversations(ctx, params)
		if err != nil {
			sum.Errors++
			return sum, err
		}
		if len(page) == 0 {
			break
		}
		newIDs := 0
		reachedCreatedAfter := false
		for _, c := range page {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			// The Developer API orders conversation pages by created_at descending.
			// Apply the lower bound locally so it cannot be confused with the
			// meeting's started_at, then stop once the page crosses it.
			if !opts.CreatedAfter.IsZero() && c.CreatedAt.Before(opts.CreatedAfter) {
				reachedCreatedAfter = true
				break
			}
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			newIDs++
			if c.Locked || c.Discarded || (c.Status != "" && c.Status != "completed") {
				continue
			}
			if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
				break
			}
			sum.MeetingsProcessed++
			content := meetingcontent.Decode(RawFormat, c.Raw, nil)
			snapshot, err := snapshot(src.ID, c, content)
			if err != nil {
				sum.Errors++
				if rowErr == nil {
					rowErr = err
				}
				continue
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
						sum.Errors++
						if rowErr == nil {
							rowErr = fmt.Errorf("omi conversation %s has unavailable transcript evidence; preserved archived transcript", c.ID)
						}
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
				sum.Errors++
				return sum, err
			}
		}
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("processed %d Omi conversations", sum.MeetingsProcessed))
		}
		if opts.Limit > 0 && sum.MeetingsProcessed >= int64(opts.Limit) {
			break
		}
		if reachedCreatedAfter {
			break
		}
		if newIDs == 0 {
			sum.Errors++
			return sum, errors.New("omi pagination repeated a page without new conversation IDs")
		}
		// Locked/malformed documents are removed after upstream pagination. A
		// short page is not exhaustion; offsets count the requested source rows.
		params.Offset += pageSize
	}
	if err := st.UpdateSyncCheckpoint(syncID, checkpoint(sum)); err != nil {
		return sum, err
	}
	if rowErr != nil {
		return sum, fmt.Errorf("omi skipped %d conversations: %w", sum.Errors, rowErr)
	}
	// No timestamp watermark: late completions and old edits stay discoverable.
	if err := st.CompleteSync(syncID, "{}"); err != nil {
		return sum, err
	}
	return sum, nil
}

func checkpoint(sum *ImportSummary) *store.Checkpoint {
	return &store.Checkpoint{MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}
}

func snapshot(sourceID int64, c Conversation, content meetingcontent.Content) (meetingarchive.Snapshot, error) {
	if content.Transcript.State == meetingcontent.StateUnavailable {
		// DeveloperConversation allows null/missing transcripts. Useful notes
		// still belong in the archive; malformed evidence does not.
		if content.Transcript.Reason != "missing_field" || (content.Summary.State != meetingcontent.StateAvailable && content.Notes.State != meetingcontent.StateAvailable && len(content.Actions) == 0) {
			return meetingarchive.Snapshot{}, fmt.Errorf("omi conversation %s has unavailable transcript evidence (%s)", c.ID, content.Transcript.Reason)
		}
	}
	var body strings.Builder
	if content.Summary.Text != "" {
		body.WriteString(content.Summary.Text + "\n\n")
	}
	if content.Notes.Text != "" {
		body.WriteString(content.Notes.Text + "\n\n")
	}
	if len(content.Actions) > 0 {
		body.WriteString("Action items\n")
		for _, a := range content.Actions {
			fmt.Fprintf(&body, "- %s (%s)\n", a.Title, a.Status)
		}
		body.WriteString("\n")
	}
	for _, s := range content.Transcript.Segments {
		fmt.Fprintf(&body, "%s: %s\n", s.Speaker, s.Text)
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
	runes := []rune(strings.Join(strings.Fields(text), " "))
	snippet := string(runes[:min(len(runes), 200)])
	meta, err := json.Marshal(struct {
		Platform       string `json:"platform"`
		ConversationID string `json:"conversation_id"`
		SegmentCount   int    `json:"transcript_segments"`
	}{SourceType, c.ID, len(content.Transcript.Segments)})
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	var attendees []meetingarchive.Person
	for _, person := range content.SourceParticipants {
		if person.Email != "" {
			attendees = append(attendees, meetingarchive.Person{Name: person.Name, Email: person.Email})
		}
	}
	return meetingarchive.Snapshot{Attendees: attendees, SourceID: sourceID, SourceMessageID: c.ID, SourceConversationID: "meeting:" + c.ID, Title: title, StartedAt: started, Body: text, Snippet: snippet, Metadata: meta, Raw: c.Raw, RawFormat: RawFormat}, nil
}
