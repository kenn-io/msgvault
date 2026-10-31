package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestXReplyRoutesFinishProtectedAdoptionCleanup(t *testing.T) {
	for _, surface := range []string{"prepare", "send"} {
		for _, association := range []string{"accepted", "rejected", "cleanup failure"} {
			t.Run(surface+"/"+association, func(t *testing.T) {
				f := newReplySurfaceFixture(t)
				foreign, err := f.st.GetOrCreateSource("x", "2001")
				require.NoError(t, err)
				now := time.Now().UTC()
				_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "2001", PostID: "500", AuthorID: "1001", ConversationID: "500", EditHistory: []string{"500"}, Text: "foreign protected copy", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", Username: "example_owner", FetchedAt: now}})
				require.NoError(t, err)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						f.posts.Add(1)
						w.WriteHeader(503)
						fmt.Fprint(w, `{"title":"Synthetic uncertainty"}`)
						return
					}
					assert.Equal(t, "9001", r.URL.Query().Get("ids"))
					parent := "102"
					if association == "rejected" {
						parent = "999"
					}
					fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":%q,"note_tweet":{"text":%q},"created_at":"2026-10-31T06:00:00Z","edit_history_tweet_ids":["9001"],"referenced_tweets":[{"type":"replied_to","id":%q}]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":true}]}}`, f.draft.Body, f.draft.Body, parent)
				}))
				t.Cleanup(provider.Close)
				f.adapter.xDeps.apiBaseURL = provider.URL + "/2"
				f.adapter.xDeps.httpClient = provider.Client()
				cleaner := NewXContentCleanup(f.st, f.cfg, nil)
				f.adapter.xCleanup = cleaner
				server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
				cleaner.SetServer(server)
				f.handler = server.Router()
				initial := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
				require.Contains(t, initial.Body.String(), "remote_unknown")
				// No scheduler is running. Only the actual reply route can drain this work.
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cleaner.beforeCacheLock = func() error { cancel(); return nil }
				if association == "cleanup failure" {
					cleaner.beforePublish = func() error { return errors.New("synthetic cleanup failure") }
				}
				var body any
				path := "/api/v1/cli/run"
				if surface == "prepare" {
					path = "/api/v1/x/replies/prepare"
					body = map[string]any{"draft_id": f.draft.DraftID, "revision": 1, "adopt_post": "9001"}
				} else {
					body = map[string]any{"args": append(f.args(), "--adopt-post=9001")}
				}
				encoded, err := json.Marshal(body)
				require.NoError(t, err)
				adopt := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(encoded))
				adopt.Header.Set("Content-Type", "application/json")
				adopt.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
				out := httptest.NewRecorder()
				f.handler.ServeHTTP(out, adopt)
				if association == "cleanup failure" {
					assert.Contains(t, out.Body.String(), "x_content_cleanup_pending")
					if surface == "send" {
						assert.Contains(t, out.Body.String(), `"type":"stdout"`)
						assert.Contains(t, out.Body.String(), "9001")
						receipt, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
						require.NoError(t, err)
						assert.Equal(t, "sent", receipt.State)
						assert.Equal(t, "9001", receipt.RemoteID)
					}
					pending, err := f.st.XCleanupPendingContext(t.Context())
					require.NoError(t, err)
					assert.True(t, pending)
					cleaner.beforePublish = nil
					require.NoError(t, cleaner.Resume(t.Context()))
				} else if association == "accepted" {
					require.Equal(t, 200, out.Code, out.Body.String())
					assert.NotContains(t, out.Body.String(), `"type":"error"`)
				} else {
					assert.Contains(t, out.Body.String(), "adoption_mismatch")
				}
				assert.Equal(t, int32(1), f.posts.Load())
				pending, err := f.st.XCleanupPendingContext(t.Context())
				require.NoError(t, err)
				assert.False(t, pending, "reply owner boundary must finish queued foreign cleanup")
				target, err := f.st.ResolveXPostContext(t.Context(), foreign.ID, "500")
				require.NoError(t, err)
				require.Positive(t, target.MessageID, "removal retains the message tombstone")
				var bodies int
				require.NoError(t, f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM message_bodies WHERE message_id=?`), target.MessageID).Scan(&bodies))
				assert.Zero(t, bodies, "retain the tombstone row while purging its actual archived body")
				assert.Equal(t, "removed", target.Availability)
				assert.ErrorIs(t, ctx.Err(), context.Canceled, "cleanup hook must run")
				request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", f.parent), nil)
				request.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
				read := httptest.NewRecorder()
				f.handler.ServeHTTP(read, request)
				assert.Equal(t, 200, read.Code, read.Body.String())
			})
		}
	}
}
