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
	for _, recurring := range []bool{false, true} {
		name := "channels"
		if recurring {
			name = "recurring sync"
		}
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			cfg, st, fake := lineWiringFixture(t)
			channels := []string{"2000000001"}
			events := 1
			if !recurring {
				channels = append(channels, "2000000002")
				events = 3
			}
			for _, id := range channels {
				_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", id, "--secret-env", "EXAMPLE_LINE_SECRET")
				must.NoError(err)
			}
			loaded, err := config.Load(cfg.ConfigFilePath(), "")
			must.NoError(err)
			for i := range loaded.Line {
				loaded.Line[i].MaxMediaMB = 1
			}
			a, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
			must.NoError(err)
			enqueue := func(t *testing.T) {
				t.Helper()
				for i := range events {
					id := strconv.Itoa(i + 1)
					eventID := "media-" + id
					if recurring {
						eventID = "slow-media"
					}
					raw := fmt.Sprintf(`{"type":"message","webhookEventId":%q,"timestamp":%d,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"%s","type":"file","fileName":"example.txt"}}`, eventID, 1000+i, id)
					require.NoError(t, st.EnqueueLineEventsContext(t.Context(), a.ID, a.Identifier, []store.LineWebhookEvent{{WebhookEventID: eventID, EventType: "message", TargetMessageID: id, EventTimestamp: int64(1000 + i), RawJSON: raw}}))
				}
			}
			if recurring {
				enqueue(t)
			}
			synctest.Test(t, func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				var contentRequests atomic.Int64
				started := make(chan struct{}, 1)
				lineClientFactory = func(id, secret string) *line.Client {
					return line.NewClient(id, secret, line.WithHTTPClient(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/content") {
							contentRequests.Add(1)
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
				var otherRuns atomic.Int64
				var ranAfterMedia atomic.Bool
				if recurring {
					must.NoError(sched.AddJob(scheduler.Job{Name: "recurring sync", Interval: time.Minute, Run: func(ctx context.Context) error {
						otherRuns.Add(1)
						row, err := st.LineMessageContext(ctx, a.ID, "1")
						if err != nil {
							return err
						}
						refs, err := st.MessageProviderAttachments(row.ID, "line:")
						if err == nil && refs["line:1"].State == attachmentpolicy.StateStored {
							ranAfterMedia.Store(true)
						}
						return err
					}}))
					sched.Start()
				} else {
					enqueue(t)
				}
				runtime.wake(a.Identifier)
				select {
				case <-started:
				case <-time.After(time.Minute):
					must.FailNow("channel A did not start downloading")
				}
				var b *store.Source
				if !recurring {
					b, err = st.GetSourceByTypeAndIdentifier("line", "2000000002")
					must.NoError(err)
					must.NoError(st.EnqueueLineEventsContext(t.Context(), b.ID, b.Identifier, []store.LineWebhookEvent{{WebhookEventID: "text-b", EventType: "message", TargetMessageID: "text-b", EventTimestamp: 1000, RawJSON: `{"type":"message","timestamp":1000,"webhookEventId":"text-b","source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"text-b","type":"text","text":"channel B text"}}`}}))
					runtime.wake(b.Identifier)
					synctest.Wait()
				}
				time.Sleep(12 * time.Minute)
				synctest.Wait()
				row, err := st.LineMessageContext(t.Context(), a.ID, "1")
				must.NoError(err)
				refs, err := st.MessageProviderAttachments(row.ID, "line:")
				must.NoError(err)
				checks.Equal(attachmentpolicy.StateStored, refs["line:1"].State, "the first bounded download must finish before the pass budget")
				if recurring {
					checks.Equal(int64(1), contentRequests.Load())
					checks.Positive(otherRuns.Load())
					checks.True(ranAfterMedia.Load(), "queued sync must run after LINE records the completed download")
				} else {
					var count int
					must.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id='text-b'"), b.ID).Scan(&count))
					checks.Equal(1, count, "the second channel must drain while the first still has pending media")
				}
			})
		})
	}
}
