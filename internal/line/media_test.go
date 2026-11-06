package line

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/search"
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

func TestMediaPolicyChangeRecoversRetryingEvent(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments,
		WithProcessorClock(func() time.Time { return f.now }),
		WithProcessorMediaPolicy(attachmentpolicy.Policy{DisabledReason: attachmentpolicy.SkipAccountPolicy}))
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: http.StatusInternalServerError})
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	checks.Equal(attachmentpolicy.StateSkipped, f.media(t).State)
	checks.Equal(attachmentpolicy.SkipAccountPolicy, f.media(t).SkipReason)
	checks.NotZero(f.archived(t, "1").id)
	var state string
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	must.Equal("retry", state)
	checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	f.now = f.now.Add(time.Hour)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	checks.Equal(attachmentpolicy.StateStored, f.media(t).State)
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("processed", state)
}

func TestMediaConversationPreview(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		filename string
		body     string
	}{
		{"image", "", "[image]"},
		{"video", "", "[video]"},
		{"audio", "", "[audio]"},
		{"file", "synthetic.bin", "synthetic.bin"},
		{"file", "", "[file]"},
	} {
		t.Run(tc.kind+"/"+tc.filename, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			e := f.fileEvent()
			e.Message.Type = tc.kind
			e.Message.FileName = tc.filename
			f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: http.StatusOK, Body: "synthetic bytes"})
			f.enqueue(t, e, time.Time{})
			f.run(t)
			checks.Equal(tc.body, f.archived(t, "1").body)
			var snippet, preview sql.NullString
			must.NoError(f.st.DB().QueryRow(`SELECT snippet FROM messages WHERE source_message_id='1'`).Scan(&snippet))
			must.NoError(f.st.DB().QueryRow(`SELECT last_message_preview FROM conversations`).Scan(&preview))
			checks.Equal(sql.NullString{String: tc.body, Valid: true}, snippet)
			checks.Equal(sql.NullString{String: tc.body, Valid: true}, preview)
		})
	}
}

func TestMediaTranscodingRetry(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/message/1/content"
	content := lineapi.Response{Status: http.StatusOK, Body: "synthetic bytes"}
	content.Header = http.Header{"Content-Type": []string{"application/octet-stream"}}
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusAccepted}, content)
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.run(t)
	checks.Equal(attachmentpolicy.StatePending, f.media(t).State)
	_, total, err := f.st.SearchMessagesQuery(search.Parse("larger:13"), 0, 10)
	must.NoError(err)
	checks.Zero(total)
	f.now = f.now.Add(time.Minute)
	f.run(t)
	var updated int64
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
	checks.Equal(int64(1), updated)
	stored := f.media(t)
	checks.Equal(attachmentpolicy.StateStored, stored.State)
	checks.Len(stored.ContentHash, 64)
	checks.NotEmpty(stored.StoragePath)
	checks.Equal(2, f.api.Count(http.MethodGet, path))
}

func TestMediaFailureAtRetryHorizon(t *testing.T) {
	t.Run("after preparing", func(t *testing.T) {
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
	})
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
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	e := f.fileEvent()
	e.Message.Type = "image"
	must.NoError(json.Unmarshal([]byte(fmt.Sprintf(`{"type":"message","timestamp":%d,"webhookEventId":"event-1","source":{"type":"group","groupId":"C00000000000000000000000000000001","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"image","contentProvider":{"type":"external","originalContentUrl":"https://media.example.com/external","previewImageUrl":"https://media.example.com/preview"}}}`, e.Timestamp)), &e))
	f.enqueue(t, e, time.Time{})
	f.run(t)
	checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	row, err := f.st.LineMessageContext(context.Background(), f.source.ID, "1")
	must.NoError(err)
	checks.Contains(row.Metadata.String, "https://media.example.com/external")
}

func TestMediaCapAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, body, messageType string
		declaredSize, maxBytes  int64
		chunked                 bool
		state                   attachmentpolicy.DownloadState
	}{
		{"empty", "", "file", 0, 8, false, attachmentpolicy.StateStored},
		{"at limit", "12345678", "file", 0, 8, false, attachmentpolicy.StateStored},
		{"content length", "123456789", "file", 0, 8, false, attachmentpolicy.StateSkipped},
		{"image content length", "123456789", "image", 0, 8, false, attachmentpolicy.StateSkipped},
		{"declared size", "", "file", 3, 2, false, attachmentpolicy.StateSkipped},
		{"chunked", "123456789", "file", 0, 8, true, attachmentpolicy.StateSkipped},
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
			if tc.messageType == "image" {
				event.Message = &Message{ID: "1", Type: "image"}
			}
			f.enqueue(t, event, time.Time{})
			f.run(t)
			ref := f.media(t)
			checks.Equal(tc.state, ref.State)
			if tc.state == attachmentpolicy.StateSkipped {
				checks.Equal(attachmentpolicy.SkipSizeCap, ref.SkipReason)
				checks.Empty(ref.StoragePath)
				if tc.declaredSize == 0 {
					checks.Equal(9, ref.Size)
				}
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
	var count int
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=?`), f.archived(t, "1").id).Scan(&count))
	checks.Equal(1, count)
	checks.Equal(first.ContentHash, again.ContentHash)
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
}

func TestMediaLocalFailureSurvivesYieldToWaiter(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	blocked := filepath.Join(t.TempDir(), "attachments")
	must.NoError(os.WriteFile(blocked, []byte("blocked"), 0o600))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, blocked, WithProcessorClock(func() time.Time { return f.now }))
	f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: http.StatusOK, Body: "synthetic bytes"})
	f.enqueue(t, f.fileEvent(), time.Time{})
	second := f.fileEvent()
	second.WebhookEventID = "event-2"
	second.Message.ID = "2"
	second.Timestamp++
	f.enqueue(t, second, time.Time{})
	ctx, request := jobctx.WithPreemption(t.Context())
	request()

	passErr := f.processor.Process(ctx)
	must.ErrorIs(passErr, jobctx.ErrYieldedToWaiter)
	must.ErrorContains(passErr, "store LINE media")
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/message/2/content"))
	run, err := f.st.GetLatestSync(f.source.ID)
	must.NoError(err)
	checks.Equal(store.SyncStatusFailed, run.Status)
	checks.Contains(run.ErrorMessage.String, "store LINE media")
}

func TestMediaLocalFailureDoesNotStarveLaterBatch(t *testing.T) {
	t.Run("other chat in same batch", func(t *testing.T) {
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
	})
	t.Run("later batch", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		f := newInboxFixture(t)
		blocked := filepath.Join(t.TempDir(), "attachments")
		must.NoError(os.WriteFile(blocked, []byte("blocked"), 0o600))
		f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, blocked, WithProcessorClock(func() time.Time { return f.now }))
		for i := range 501 {
			e := f.fileEvent()
			e.Message.ID = strconv.Itoa(i + 1)
			e.WebhookEventID = "media-" + e.Message.ID
			e.Timestamp = f.now.UnixMilli() + int64(i)
			f.api.Enqueue(http.MethodGet, "/v2/bot/message/"+e.Message.ID+"/content", lineapi.Response{Status: http.StatusOK, Body: "synthetic bytes"})
			f.enqueue(t, e, time.Time{})
		}
		f.enqueue(t, f.message("later-text", "later text", f.now.UnixMilli()+501), time.Time{})
		for range 2 {
			must.Error(f.processor.Process(t.Context()))
		}
		checks.Equal("later text", f.archived(t, "later-text").body)
		var state string
		must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='media-1'`).Scan(&state))
		checks.Equal("pending", state)
		f.now = f.now.Add(retryHorizon + time.Hour)
		must.Error(f.processor.Process(t.Context()))
		must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='media-1'`).Scan(&state))
		checks.Equal("pending", state)
		var next time.Time
		must.NoError(f.st.DB().QueryRow(`SELECT next_attempt_at FROM line_webhook_events WHERE webhook_event_id='media-1'`).Scan(&next))
		checks.True(next.After(f.now))
	})
}

func TestMediaPreemptionPreservesRetrySchedule(t *testing.T) {
	for _, state := range []string{"pending", "retry"} {
		t.Run(state, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			f.enqueue(t, f.fileEvent(), time.Time{})
			rows, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
			must.NoError(err)
			must.Len(rows, 1)
			if state == "retry" {
				must.NoError(f.st.FinishLineEventContext(t.Context(), rows[0].ID, "retry", "provider failure", f.now.Add(-time.Minute)))
			}
			before, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
			must.NoError(err)
			must.Len(before, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/bot/message/1/content" {
					cancel()
					<-r.Context().Done()
					return
				}
				f.api.ServeHTTP(w, r)
			}))
			defer server.Close()
			client := NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL))
			processor := NewProcessor(f.st, client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
			must.ErrorIs(processor.Process(ctx), context.Canceled)
			after, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now.Add(time.Hour), 10)
			must.NoError(err)
			must.Len(after, 1)
			checks.Equal(before[0].State, after[0].State)
			checks.Equal(before[0].Attempts, after[0].Attempts)
			checks.Equal(before[0].NextAttemptAt, after[0].NextAttemptAt)
			f.run(t)
			checks.Equal(attachmentpolicy.StateStored, f.media(t).State)
		})
	}
}
