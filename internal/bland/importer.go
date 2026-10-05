package bland

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type Importer struct {
	store  *store.Store
	client *Client
	now    func() time.Time
}

func NewImporter(s *store.Store, c *Client) *Importer {
	return &Importer{store: s, client: c, now: time.Now}
}

type (
	ImportOptions = callsync.Options
	ImportSummary = callsync.Summary
)

var spec = callsync.Spec{SourceType: SourceType, Label: "Bland", Command: "sync-bland", RowPrefix: "bland:recording:"}

// pageSize is Bland's default and largest call-list page.
const pageSize = 1000

// importRun is the Bland side of one engine run.
type importRun struct {
	*callsync.Run

	imp *Importer
}

func (imp *Importer) Import(ctx context.Context, o ImportOptions) (*ImportSummary, error) {
	if imp == nil || imp.store == nil || imp.client == nil {
		return nil, errors.New("bland importer unavailable")
	}
	return callsync.Import(ctx, imp.store, spec, imp.now(), o, func(run *callsync.Run) callsync.Provider[*Call] {
		return &importRun{Run: run, imp: imp}
	})
}

// ListPage reads one page of calls created on or after after, oldest first.
// The cursor is the next offset; calls created since the listing started
// sort after it, so they don't shift it.
func (r *importRun) ListPage(ctx context.Context, after time.Time, cursor string) ([]callsync.Item[*Call], string, error) {
	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 {
			return nil, "", fmt.Errorf("invalid Bland listing cursor %q", cursor)
		}
		offset = parsed
	}
	start := ""
	if !after.IsZero() {
		start = after.UTC().Format(time.RFC3339)
	}
	page, err := r.imp.client.ListCalls(ctx, ListOptions{Offset: offset, Limit: pageSize, CreatedAfter: start})
	if err != nil {
		return nil, "", err
	}
	items := make([]callsync.Item[*Call], 0, len(page.Calls))
	for _, c := range page.Calls {
		items = append(items, callsync.Item[*Call]{ID: c.ID, Listed: c})
	}
	offset += len(page.Calls)
	if len(page.Calls) == 0 || page.Total != nil && offset >= *page.Total {
		return items, "", nil
	}
	return items, strconv.Itoa(offset), nil
}

// Handle fetches one listed call again and archives what Bland has for it now.
func (r *importRun) Handle(ctx context.Context, id string, listed *Call) (callErr, err error) {
	return r.archiveCall(ctx, id, listed)
}

func (r *importRun) archivedEvidence(ctx context.Context, messageID int64) (*Evidence, error) {
	raw, err := r.St.GetMessageRawContext(ctx, messageID)
	if err != nil {
		return nil, err
	}
	var e Evidence
	if err := json.Unmarshal(raw, &e); err != nil || e.Version != 1 {
		return nil, errors.New("invalid archived Bland evidence")
	}
	return &e, nil
}

// archiveCall archives a listed call with whatever Bland has for it now; later
// relists update it as the call ends and its artifacts arrive. A read Bland
// refuses is noted and the call archives without it; any other failed read,
// transient or malformed, is callErr, which the next run retries. err is an
// archive failure that stops the run. A call deleted since it was listed is
// skipped.
func (r *importRun) archiveCall(ctx context.Context, id string, listed *Call) (callErr, err error) {
	c, callErr := r.imp.client.GetCall(ctx, id)
	switch {
	case errors.Is(callErr, ErrNotFound):
		return nil, nil
	case refused(callErr):
		r.Sum.Diagnostics = append(r.Sum.Diagnostics, fmt.Sprintf("call %s: details unavailable: %v", id, callErr))
	case callErr != nil:
		return callErr, nil
	}
	detailsRefused := callErr != nil
	// unread names why a transcript-bearing read failed: provider_refused when
	// Bland refused it, fetch_failed when it may succeed later.
	unread := ""
	if detailsRefused {
		unread = "provider_refused"
	}
	// A failed postcall read still archives the call and its recording.
	hook, hookErr := r.imp.client.GetPostCall(ctx, id)
	switch {
	case errors.Is(hookErr, ErrNotFound):
		hook, hookErr = nil, nil
	case refused(hookErr):
		r.Sum.Diagnostics = append(r.Sum.Diagnostics, fmt.Sprintf("call %s: postcall data unavailable: %v", id, hookErr))
		hook, hookErr, unread = nil, nil, cmp.Or(unread, "provider_refused")
	case hookErr != nil:
		hook, unread = nil, "fetch_failed"
	}
	existing, err := r.St.MessageExistsBatch(r.SourceID, []string{id})
	if err != nil {
		return nil, err
	}
	var previous *Evidence
	if messageID := existing[id]; messageID != 0 {
		if previous, err = r.archivedEvidence(ctx, messageID); err != nil {
			return nil, err
		}
	}
	if detailsRefused {
		// The listed row stands in for the details, on top of those archived.
		if c, err = fallbackCall(listed, previous); err != nil {
			return err, nil
		}
	}
	ev, c, err := buildEvidence(c, hook, previous, unread)
	if err != nil {
		return err, nil
	}
	raw, err := marshalEvidence(ev)
	if err != nil {
		return nil, err
	}
	metadata, err := json.Marshal(struct {
		Provider   string `json:"provider"`
		CallID     string `json:"call_id"`
		From       string `json:"from"`
		To         string `json:"to"`
		Inbound    bool   `json:"inbound"`
		Status     string `json:"status"`
		AnsweredBy string `json:"answered_by"`
	}{SourceType, id, c.From, c.To, c.Inbound, c.Status, c.AnsweredBy})
	if err != nil {
		return nil, err
	}
	body := "Bland call"
	if ev.Content.Summary.Text != "" {
		body += "\n\nSummary:\n" + ev.Content.Summary.Text
	}
	if ev.Content.Transcript.Text != "" {
		body += "\n\nTranscript:\n" + ev.Content.Transcript.Text
	}
	attendees := []meetingarchive.Person{}
	for _, person := range ev.Content.SourceParticipants {
		attendees = append(attendees, meetingarchive.Person{Phone: person.Phone})
	}
	other := c.To
	if c.Inbound {
		other = c.From
	}
	snapshot := meetingarchive.Snapshot{SourceID: r.SourceID, AccountEmail: r.Opts.AccountEmail, SourceMessageID: id, SourceConversationID: id, Title: meetingarchive.CallTitle("Bland call", other, c.Inbound), StartedAt: c.started(), Body: body, Snippet: snippet(ev.Content), Metadata: metadata, Raw: raw, RawFormat: RawFormat, Attendees: attendees}
	result, err := meetingarchive.New(r.St).Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: r.Opts.Full})
	if result.Changed {
		if result.Created {
			r.Sum.MeetingsAdded++
		} else {
			r.Sum.MeetingsUpdated++
		}
	}
	if err != nil {
		return nil, err
	}
	// A recording is coming only once Bland reports its URL.
	mediaErr, err := r.record(ctx, result.MessageID, c, c.RecordingURL != "", result.Changed)
	if err != nil {
		return nil, err
	}
	return errors.Join(hookErr, mediaErr), nil
}

// refused reports an answer Bland would give again, such as an auth failure or
// a rejected request, so the call archives without what failed.
func refused(err error) bool {
	httpErr, ok := errors.AsType[*HTTPError](err)
	return ok && !httpErr.Retryable()
}

func snippet(c meetingcontent.Content) string {
	return textutil.TruncateRunes(strings.TrimSpace(cmp.Or(c.Summary.Text, c.Transcript.Text)), 200)
}
