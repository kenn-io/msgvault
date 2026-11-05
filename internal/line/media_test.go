package line

import (
	"context"
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
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	path := "/v2/bot/message/1/content"
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusAccepted})
	f.enqueue(t, f.fileEvent(), f.now)
	f.run(t)
	checks.Equal(attachmentpolicy.StatePending, f.media(t).State)
	f.now = f.now.Add(25 * time.Hour)
	f.run(t)
	var state string
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("failed", state)
	checks.Equal(attachmentpolicy.StateFailed, f.media(t).State)
	checks.Equal(1, f.api.Count(http.MethodGet, path), "expiry must not fetch content again")
}

func TestMediaFailureAtRetryHorizonWaitsForConversationFinalization(t *testing.T) {
	t.Run("at receipt", func(t *testing.T) {
		assert := assert.New(t)
		f := newInboxFixture(t)
		path := "/v2/bot/message/1/content"
		f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusAccepted})
		f.enqueue(t, f.fileEvent(), f.now.Add(-retryHorizon))
		f.run(t)
		var state string
		require.NoError(t, f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
		assert.Equal("failed", state)
		assert.Equal(attachmentpolicy.StateFailed, f.media(t).State)
		assert.Equal(1, f.api.Count(http.MethodGet, path), "expiry must not fetch content again")
	})
	t.Run("finalization failure", func(t *testing.T) {
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
	})
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

func TestMediaLocalStorageFailureAllowsOtherChats(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: http.StatusOK, Body: "synthetic bytes"})
	blocked := filepath.Join(t.TempDir(), "attachments")
	must.NoError(os.WriteFile(blocked, []byte("not a directory"), 0o600))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, blocked, WithProcessorClock(func() time.Time { return f.now }))
	f.enqueue(t, f.fileEvent(), time.Time{})
	text := f.message("2", "text in another chat", f.now.UnixMilli()+1)
	text.Source.GroupID = "C00000000000000000000000000000002"
	f.enqueue(t, text, time.Time{})

	must.Error(f.processor.Process(context.Background()))
	var state string
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("pending", state)
	checks.Equal(attachmentpolicy.StatePending, f.media(t).State)
	checks.Equal("text in another chat", f.archived(t, "2").body)
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-2'`).Scan(&state))
	checks.Equal("processed", state)
	var messageCount int64
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT message_count FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, "group:"+text.Source.GroupID).Scan(&messageCount))
	checks.Equal(int64(1), messageCount)
	var runStatus, runError string
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT status,error_message FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&runStatus, &runError))
	checks.Equal("failed", runStatus)
	checks.NotEmpty(runError)
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
	for _, tc := range []struct {
		name, body             string
		declaredSize, maxBytes int64
		chunked                bool
		state                  attachmentpolicy.DownloadState
	}{
		{"empty", "", 0, 8, false, attachmentpolicy.StateStored},
		{"at limit", "12345678", 0, 8, false, attachmentpolicy.StateStored},
		{"content length", "123456789", 0, 8, false, attachmentpolicy.StateSkipped},
		{"declared size", "", 3, 2, false, attachmentpolicy.StateSkipped},
		{"chunked", "123456789", 0, 8, true, attachmentpolicy.StateSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			f := newInboxFixture(t)
			path := "/v2/bot/message/1/content"
			if tc.chunked {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == path {
						w.Header().Set("Content-Type", "application/octet-stream")
						w.WriteHeader(http.StatusOK)
						checks.NoError(http.NewResponseController(w).Flush())
						_, err := io.WriteString(w, tc.body)
						checks.NoError(err)
						return
					}
					f.api.ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
			} else {
				f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusOK, Body: tc.body})
			}
			f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }), WithProcessorMediaPolicy(attachmentpolicy.Policy{MaxBytes: tc.maxBytes}))
			event := f.fileEvent()
			event.Message.FileSize = tc.declaredSize
			f.enqueue(t, event, time.Time{})
			f.run(t)
			ref := f.media(t)
			checks.Equal(tc.state, ref.State)
			if tc.state == attachmentpolicy.StateSkipped {
				checks.Equal(attachmentpolicy.SkipSizeCap, ref.SkipReason)
				checks.Empty(ref.StoragePath)
			} else {
				checks.Equal(len(tc.body), ref.Size)
				checks.Len(ref.ContentHash, 64)
			}
			if tc.declaredSize > 0 {
				checks.Zero(f.api.Count(http.MethodGet, path))
				checks.NotZero(f.archived(t, "1").id)
			}
		})
	}
}

func TestUnsendCancelsMedia(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state attachmentpolicy.DownloadState
		calls int
	}{
		{"pending", attachmentpolicy.StateUnavailable, 1},
		{"stored", attachmentpolicy.StateStored, 1},
		{"before original", attachmentpolicy.StateUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			f := newInboxFixture(t)
			path := "/v2/bot/message/1/content"
			if tc.name == "pending" {
				f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusAccepted})
			}
			base := f.fileEvent()
			unsend := Event{Type: "unsend", WebhookEventID: "unsend-media", Timestamp: base.Timestamp + 1, Source: base.Source, Unsend: &Unsend{MessageID: "1"}}
			var prior store.AttachmentRef
			if tc.name == "before original" {
				f.enqueue(t, unsend, time.Time{})
				f.run(t)
				f.enqueue(t, base, time.Time{})
				f.run(t)
			} else {
				f.enqueue(t, base, time.Time{})
				f.run(t)
				prior = f.media(t)
				f.enqueue(t, unsend, time.Time{})
				f.run(t)
			}
			after := f.media(t)
			checks.Equal(tc.state, after.State)
			if tc.name == "stored" {
				checks.Equal(prior.ContentHash, after.ContentHash)
				checks.Equal(prior.StoragePath, after.StoragePath)
			}
			if tc.name == "pending" {
				f.now = f.now.Add(time.Hour)
				f.run(t)
				checks.True(f.archived(t, "1").deleted)
			}
			checks.Equal(tc.calls, f.api.Count(http.MethodGet, path))
		})
	}
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
}

func TestMediaChannelPolicy(t *testing.T) {
	checks := assert.New(t)
	f := newInboxFixture(t)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorMediaPolicy(attachmentpolicy.Policy{DisabledReason: attachmentpolicy.SkipAccountPolicy}))
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	media := f.media(t)
	checks.Equal(attachmentpolicy.StateSkipped, media.State)
	checks.Equal(attachmentpolicy.SkipAccountPolicy, media.SkipReason)
	checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	checks.NotZero(f.archived(t, "1").id)
}
