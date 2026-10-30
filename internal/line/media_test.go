package line

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func (f *inboxFixture) media(t *testing.T) store.AttachmentRef {
	t.Helper()
	row, err := f.st.LineMessageContext(context.Background(), f.source.ID, "1")
	require.NoError(t, err)
	refs, err := f.st.MessageProviderAttachments(row.ID, "line:")
	require.NoError(t, err)
	require.Len(t, refs, 1)
	return refs["line:1"]
}
func (f *inboxFixture) fileEvent() Event {
	e := f.message("1", "", f.now.UnixMilli())
	e.Message = &Message{ID: "1", Type: "file", FileName: "synthetic.bin"}
	return e
}

func TestMediaTranscodingRetry(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/message/1/content"
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: 202}, lineapi.Response{Status: 200, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: "synthetic bytes"})
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	checks.Equal(attachmentpolicy.StatePending, f.media(t).State)
	f.now = f.now.Add(time.Minute)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	stored := f.media(t)
	checks.Equal(attachmentpolicy.StateStored, stored.State)
	checks.Len(stored.ContentHash, 64)
	checks.NotEmpty(stored.StoragePath)
	checks.Equal(2, f.api.Count(http.MethodGet, path))
	f.run(t)
	checks.Equal(2, f.api.Count(http.MethodGet, path))
}

func TestMediaPreparingAtRetryHorizonFinishesAttachment(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: 202})
	f.enqueue(t, f.fileEvent(), f.now.Add(-retryHorizon))
	f.run(t)
	var state string
	require.NoError(t, f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("failed", state)
	checks.Equal(attachmentpolicy.StateFailed, f.media(t).State)
}

func TestMediaFailureAtRetryHorizonWaitsForConversationFinalization(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("SQLite trigger injects a real failed finalization; normal processor contracts run on both backends")
	}
	f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: 202}, lineapi.Response{Status: 202})
	f.enqueue(t, f.fileEvent(), f.now.Add(-retryHorizon))
	_, err := f.st.DB().Exec(`CREATE TRIGGER line_finalize_fails BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT,'synthetic finalization failure'); END`)
	must.NoError(err)
	must.Error(f.processor.Process(context.Background()))
	eventState := func() string {
		t.Helper()
		var state string
		must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
		return state
	}
	checks.Equal("pending", eventState(), "a failed event with a committed message waits for its conversation")

	_, err = f.st.DB().Exec(`DROP TRIGGER line_finalize_fails`)
	must.NoError(err)
	f.run(t)
	checks.Equal("failed", eventState())
	checks.Equal(attachmentpolicy.StateFailed, f.media(t).State)
	var messageCount int64
	must.NoError(f.st.DB().QueryRow(`SELECT message_count FROM conversations WHERE source_id=? AND source_conversation_id=?`, f.source.ID, "group:"+testGroup).Scan(&messageCount))
	checks.Equal(int64(1), messageCount)
}

func TestMediaLocalStorageFailureKeepsInboxWorkPending(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	const contentPath = "/v2/bot/message/1/content"
	f.api.Enqueue(http.MethodGet, contentPath,
		lineapi.Response{Status: http.StatusAccepted},
		lineapi.Response{Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: "synthetic bytes"},
		lineapi.Response{Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: "synthetic bytes"},
	)
	eventState := func() string {
		t.Helper()
		var state string
		must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
		return state
	}
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	checks.Equal("retry", eventState(), "HTTP 202 enters the bounded provider retry path")
	f.now = f.now.Add(time.Minute)

	// A regular file where the attachments directory belongs makes every write fail
	// locally while LINE keeps serving the content.
	blocked := filepath.Join(t.TempDir(), "attachments")
	must.NoError(os.WriteFile(blocked, []byte("not a directory"), 0o600))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, blocked, WithProcessorClock(func() time.Time { return f.now }))

	must.Error(f.processor.Process(context.Background()))
	checks.Equal("pending", eventState(), "a local write failure returns a retried event to unbounded pending work")
	checks.Equal(attachmentpolicy.StatePending, f.media(t).State)

	f.now = f.now.Add(retryHorizon + time.Hour)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	checks.Equal("processed", eventState())
	checks.Equal(attachmentpolicy.StateStored, f.media(t).State)
}

func TestMediaDroppedDownloadStillRetries(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/bot/message/1/content" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, err := io.WriteString(w, "short")
			checks.NoError(err)
			return
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)

	var state string
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("retry", state, "a download dropped by LINE retries until the horizon")
	checks.Equal(attachmentpolicy.StateFailed, f.media(t).State)
}

func TestMediaUnavailable(t *testing.T) {
	for _, status := range []int{404, 410} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newInboxFixture(t)
			f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: status})
			f.enqueue(t, f.fileEvent(), time.Time{})
			f.run(t)
			assert.Equal(t, attachmentpolicy.StateUnavailable, f.media(t).State)
		})
	}
}

func TestMediaExternalProvider(t *testing.T) {
	f := newInboxFixture(t)
	e := f.fileEvent()
	e.Message.Type = "image"
	e.Message.ContentProvider = &ContentProvider{Type: "external", OriginalContentURL: "https://media.example.com/external", PreviewImageURL: "https://media.example.com/preview"}
	f.enqueue(t, e, time.Time{})
	f.run(t)
	assert.Zero(t, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	row, err := f.st.LineMessageContext(context.Background(), f.source.ID, "1")
	require.NoError(t, err)
	assert.Contains(t, row.Metadata.String, "https://media.example.com/external")
}

func TestMediaCapAndEmpty(t *testing.T) {
	for _, body := range []string{"", "12345678", "123456789"} {
		t.Run(string(rune('a'+len(body))), func(t *testing.T) {
			checks := assert.New(t)

			f := newInboxFixture(t)
			f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }), WithProcessorMediaLimit(8))
			f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: 200, Body: body})
			f.enqueue(t, f.fileEvent(), time.Time{})
			f.run(t)
			ref := f.media(t)
			if len(body) > 8 {
				checks.Equal(attachmentpolicy.StateSkipped, ref.State)
				checks.Equal(attachmentpolicy.SkipSizeCap, ref.SkipReason)
				checks.Empty(ref.StoragePath)
			} else {
				checks.Equal(attachmentpolicy.StateStored, ref.State)
				checks.Equal(len(body), ref.Size)
				checks.Len(ref.ContentHash, 64)
			}
		})
	}
}

func TestUnsendCancelsMedia(t *testing.T) {
	f := newInboxFixture(t)
	path := "/v2/bot/message/1/content"
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: 202})
	base := f.fileEvent()
	f.enqueue(t, base, time.Time{})
	f.run(t)
	e := Event{Type: "unsend", WebhookEventID: "unsend-media", Timestamp: base.Timestamp + 1, Source: base.Source, Unsend: &Unsend{MessageID: "1"}}
	f.enqueue(t, e, time.Time{})
	f.run(t)
	assert.Equal(t, attachmentpolicy.StateUnavailable, f.media(t).State)
	f.now = f.now.Add(time.Hour)
	f.run(t)
	assert.Equal(t, 1, f.api.Count(http.MethodGet, path))
	deleted := f.archived(t, "1").deleted
	assert.True(t, deleted)
}

func TestMediaStoredContentSurvivesUnsend(t *testing.T) {
	f := newInboxFixture(t)
	base := f.fileEvent()
	f.enqueue(t, base, time.Time{})
	f.run(t)
	prior := f.media(t)
	f.enqueue(t, Event{Type: "unsend", WebhookEventID: "unsend", Timestamp: base.Timestamp + 1, Source: base.Source, Unsend: &Unsend{MessageID: "1"}}, time.Time{})
	f.run(t)
	after := f.media(t)
	assert.Equal(t, attachmentpolicy.StateStored, after.State)
	assert.Equal(t, prior.ContentHash, after.ContentHash)
	assert.Equal(t, prior.StoragePath, after.StoragePath)
}

func TestMediaUnsendBeforeOriginalNeverDownloads(t *testing.T) {
	f := newInboxFixture(t)
	base := f.fileEvent()
	f.enqueue(t, Event{Type: "unsend", WebhookEventID: "early-unsend", Timestamp: base.Timestamp + 1, Source: base.Source, Unsend: &Unsend{MessageID: "1"}}, time.Time{})
	f.run(t)
	f.enqueue(t, base, time.Time{})
	f.run(t)
	assert.Zero(t, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	assert.Equal(t, attachmentpolicy.StateUnavailable, f.media(t).State)
}

func TestMediaExpiryEndsPendingState(t *testing.T) {
	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: 202})
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	f.now = f.now.Add(25 * time.Hour)
	f.run(t)
	assert.Equal(t, attachmentpolicy.StateFailed, f.media(t).State)
	assert.Equal(t, 1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
}

func TestMediaProfileFailureDoesNotDelayDownload(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 503})
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	checks.Equal(attachmentpolicy.StateStored, f.media(t).State)
	requests := f.api.Requests()
	contentAt, profileAt := -1, -1
	for i, r := range requests {
		if r.Path == "/v2/bot/message/1/content" {
			contentAt = i
		}
		if r.Path == "/v2/bot/group/"+testGroup+"/member/"+testUser {
			profileAt = i
		}
	}
	must.NotEqual(-1, contentAt)
	must.NotEqual(-1, profileAt)
	checks.Less(contentAt, profileAt)
}

func TestMediaReplayDoesNotDuplicateAttachment(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	base := f.fileEvent()
	f.enqueue(t, base, time.Time{})
	f.run(t)
	first := f.media(t)
	base.WebhookEventID = "duplicate-original"
	f.enqueue(t, base, time.Time{})
	f.run(t)
	again := f.media(t)
	checks.Equal(first.ContentHash, again.ContentHash)
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	row, err := f.st.LineMessageContext(context.Background(), f.source.ID, "1")
	must.NoError(err)
	raw, err := f.st.GetMessageMetadataContext(context.Background(), row.ID)
	must.NoError(err)
	var metadata map[string]json.RawMessage
	must.NoError(json.Unmarshal([]byte(raw.String), &metadata))
	checks.Equal(`"file"`, string(metadata["type"]))
}

func TestMediaStreamingCapRejectsChunkedContent(t *testing.T) {
	f := newInboxFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/bot/message/1/content" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			assert.NoError(t, http.NewResponseController(w).Flush())
			_, err := io.WriteString(w, "123456789")
			assert.NoError(t, err)
			return
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }), WithProcessorMediaLimit(8))
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	ref := f.media(t)
	assert.Equal(t, attachmentpolicy.StateSkipped, ref.State)
	assert.Equal(t, attachmentpolicy.SkipSizeCap, ref.SkipReason)
	assert.Empty(t, ref.StoragePath)
}
