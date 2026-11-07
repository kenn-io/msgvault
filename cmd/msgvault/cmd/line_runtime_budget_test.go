package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func TestLineChannelsDrainWithoutCancelingSlowMedia(t *testing.T) {
	must := require.New(t)
	cfg, st, fake := lineWiringFixture(t)
	for _, id := range []string{"2000000001", "2000000002"} {
		_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", id, "--secret-env", "EXAMPLE_LINE_SECRET")
		must.NoError(err)
	}
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	for i := range loaded.Line {
		loaded.Line[i].MaxMediaMB = 1
	}
	a, err := st.GetSourceByTypeAndIdentifier(t.Context(), "line", "2000000001")
	must.NoError(err)
	synctest.Test(t, func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		started := make(chan struct{}, 1)
		lineClientFactory = func(id, secret string) *line.Client {
			return line.NewClient(id, secret, line.WithHTTPClient(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/content") {
					select {
					case started <- struct{}{}:
					default:
					}
					timer := time.NewTimer(8 * time.Minute)
					defer timer.Stop()
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-timer.C:
					}
				}
				response := httptest.NewRecorder()
				fake.ServeHTTP(response, r)
				return response.Result(), nil
			})}))
		}
		prior := rebuildLineCacheAfterScheduledSync
		rebuildLineCacheAfterScheduledSync = func(context.Context, string) error { return nil }
		defer func() { rebuildLineCacheAfterScheduledSync = prior }()
		gate := api.NewSerialOperationGate()
		sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "LINE inbox")).WithLogger(testLoggerValue())
		runtime, err := newLineRuntime(t.Context(), st, testInvocationWithConfig(loaded), sched, nil, testLoggerValue())
		must.NoError(err)
		defer func() { <-sched.Stop().Done() }()
		for i := range 3 {
			id := strconv.Itoa(i + 1)
			eventID := "media-" + id
			raw := fmt.Sprintf(`{"type":"message","webhookEventId":%q,"timestamp":%d,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"%s","type":"file","fileName":"example.txt"}}`, eventID, 1000+i, id)
			must.NoError(st.EnqueueLineEventsContext(t.Context(), a.ID, a.Identifier, []store.LineWebhookEvent{{WebhookEventID: eventID, EventType: "message", TargetMessageID: id, EventTimestamp: int64(1000 + i), RawJSON: raw}}))
		}
		runtime.wake(a.Identifier)
		select {
		case <-started:
		case <-time.After(time.Minute):
			must.FailNow("channel A did not start downloading")
		}
		b, err := st.GetSourceByTypeAndIdentifier(t.Context(), "line", "2000000002")
		must.NoError(err)
		must.NoError(st.EnqueueLineEventsContext(t.Context(), b.ID, b.Identifier, []store.LineWebhookEvent{{WebhookEventID: "text-b", EventType: "message", TargetMessageID: "text-b", EventTimestamp: 1000, RawJSON: `{"type":"message","timestamp":1000,"webhookEventId":"text-b","source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"text-b","type":"text","text":"channel B text"}}`}}))
		runtime.wake(b.Identifier)
		synctest.Wait()
		time.Sleep(12 * time.Minute)
		synctest.Wait()
		row, err := st.LineMessageContext(t.Context(), a.ID, "1")
		must.NoError(err)
		refs, err := st.MessageProviderAttachments(t.Context(), row.ID, "line:")
		must.NoError(err)
		checks.Equal(attachmentpolicy.StateStored, refs["line:1"].State, "the first bounded download must finish before the pass budget")
		var count int
		must.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id='text-b'"), b.ID).Scan(&count))
		checks.Equal(1, count, "the second channel must drain while the first still has pending media")
	})
}

func TestLineQueuedSyncRunsAfterSlowMedia(t *testing.T) {
	must := require.New(t)
	cfg, st, fake := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	for i := range loaded.Line {
		loaded.Line[i].MaxMediaMB = 1
	}
	a, err := st.GetSourceByTypeAndIdentifier(t.Context(), "line", "2000000001")
	must.NoError(err)
	raw := `{"type":"message","webhookEventId":"slow-media","timestamp":1000,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"file","fileName":"example.txt"}}`
	must.NoError(st.EnqueueLineEventsContext(t.Context(), a.ID, a.Identifier, []store.LineWebhookEvent{{WebhookEventID: "slow-media", EventType: "message", TargetMessageID: "1", EventTimestamp: 1000, RawJSON: raw}}))
	synctest.Test(t, func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		started := make(chan struct{}, 1)
		lineClientFactory = func(id, secret string) *line.Client {
			return line.NewClient(id, secret, line.WithHTTPClient(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/content") {
					select {
					case started <- struct{}{}:
					default:
					}
					timer := time.NewTimer(8 * time.Minute)
					defer timer.Stop()
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-timer.C:
					}
				}
				response := httptest.NewRecorder()
				fake.ServeHTTP(response, r)
				return response.Result(), nil
			})}))
		}
		prior := rebuildLineCacheAfterScheduledSync
		rebuildLineCacheAfterScheduledSync = func(context.Context, string) error { return nil }
		defer func() { rebuildLineCacheAfterScheduledSync = prior }()
		gate := api.NewSerialOperationGate()
		sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "LINE inbox")).WithLogger(testLoggerValue())
		runtime, err := newLineRuntime(t.Context(), st, testInvocationWithConfig(loaded), sched, nil, testLoggerValue())
		must.NoError(err)
		defer func() { <-sched.Stop().Done() }()
		var ranAfterMedia atomic.Bool
		must.NoError(sched.AddJob(scheduler.Job{Name: "recurring sync", Interval: time.Minute, Run: func(ctx context.Context) error {
			row, err := st.LineMessageContext(ctx, a.ID, "1")
			if err != nil {
				return err
			}
			refs, err := st.MessageProviderAttachments(t.Context(), row.ID, "line:")
			if err == nil && refs["line:1"].State == attachmentpolicy.StateStored {
				ranAfterMedia.Store(true)
			}
			return err
		}}))
		sched.Start()
		runtime.wake(a.Identifier)
		select {
		case <-started:
		case <-time.After(time.Minute):
			must.FailNow("channel A did not start downloading")
		}

		time.Sleep(12 * time.Minute)
		synctest.Wait()
		row, err := st.LineMessageContext(t.Context(), a.ID, "1")
		must.NoError(err)
		refs, err := st.MessageProviderAttachments(t.Context(), row.ID, "line:")
		must.NoError(err)
		checks.Equal(attachmentpolicy.StateStored, refs["line:1"].State, "the first bounded download must finish before the pass budget")
		checks.True(ranAfterMedia.Load(), "queued sync must run after LINE records the completed download")
	})
}
