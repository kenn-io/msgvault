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

func assertXStoredSuppressionReason(t *testing.T, st *store.Store, sourceID int64, postID, want string) {
	t.Helper()
	var got string
	require.NoError(t, st.DB().QueryRowContext(
		t.Context(),
		st.Rebind(`SELECT COALESCE(suppression_reason,'') FROM x_post_versions WHERE source_id=? AND post_id=?`),
		sourceID,
		postID,
	).Scan(&got))
	assert.Equal(t, want, got)
}

func TestXReplyRoutesFinishProtectedAdoptionCleanup(t *testing.T) {
	for _, surface := range []string{"prepare", "send"} {
		for _, association := range []string{"accepted", "rejected", "cleanup failure"} {
			t.Run(surface+"/"+association, func(t *testing.T) {
				_testAssert := assert.New(t)
				_testRequire := require.New(t)

				f := newReplySurfaceFixture(t)
				foreign, err := f.st.GetOrCreateSource("x", "2001")
				_testRequire.NoError(err)
				now := time.Now().UTC()
				_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "2001", PostID: "500", AuthorID: "1001", ConversationID: "500", EditHistory: []string{"500"}, Text: "foreign protected copy", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", Username: "example_owner", FetchedAt: now}})
				_testRequire.NoError(err)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						f.posts.Add(1)
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = fmt.Fprint(w, `{"title":"Synthetic uncertainty"}`)
						return
					}
					assert.Equal(t, "9001", r.URL.Query().Get("ids"))
					parent := "102"
					if association == "rejected" {
						parent = "999"
					}
					_, _ = fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":%q,"note_tweet":{"text":%q},"created_at":"2026-10-31T06:00:00Z","edit_history_tweet_ids":["9001"],"referenced_tweets":[{"type":"replied_to","id":%q}]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":true}]}}`, f.draft.Body, f.draft.Body, parent)
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
				_testRequire.Contains(initial.Body.String(), "remote_unknown")
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
				_testRequire.NoError(err)
				adopt := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(encoded))
				adopt.Header.Set("Content-Type", "application/json")
				adopt.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
				out := httptest.NewRecorder()
				f.handler.ServeHTTP(out, adopt)
				switch association {
				case "cleanup failure":
					_testAssert.Contains(out.Body.String(), "x_content_cleanup_pending")
					if surface == "send" {
						_testAssert.Contains(out.Body.String(), `"type":"stdout"`)
						_testAssert.Contains(out.Body.String(), "9001")
						receipt, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
						_testRequire.NoError(err)
						_testAssert.Equal("sent", receipt.State)
						_testAssert.Equal("9001", receipt.RemoteID)
					}
					pending, err := f.st.XCleanupPendingContext(t.Context())
					_testRequire.NoError(err)
					_testAssert.True(pending)
					cleaner.beforePublish = nil
					_testRequire.NoError(cleaner.Resume(t.Context()))
				case "accepted":
					_testRequire.Equal(200, out.Code, out.Body.String())
					_testAssert.NotContains(out.Body.String(), `"type":"error"`)
				default:
					_testAssert.Contains(out.Body.String(), "adoption_mismatch")
				}
				_testAssert.Equal(int32(1), f.posts.Load())
				pending, err := f.st.XCleanupPendingContext(t.Context())
				_testRequire.NoError(err)
				_testAssert.False(pending, "reply owner boundary must finish queued foreign cleanup")
				target, err := f.st.ResolveXPostContext(t.Context(), foreign.ID, "500")
				_testRequire.NoError(err)
				_testRequire.Positive(target.MessageID, "removal retains the message tombstone")
				var bodies int
				_testRequire.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM message_bodies WHERE message_id=?`), target.MessageID).Scan(&bodies))
				_testAssert.Zero(bodies, "retain the tombstone row while purging its actual archived body")
				_testAssert.Equal("removed", target.Availability)
				_testRequire.ErrorIs(ctx.Err(), context.Canceled, "cleanup hook must run")
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", f.parent), nil)
				request.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
				read := httptest.NewRecorder()
				f.handler.ServeHTTP(read, request)
				_testAssert.Equal(200, read.Code, read.Body.String())
			})
		}
	}
}

func TestXReplyMismatchKeepsUnavailableAdoptionEvidence(t *testing.T) {
	for _, reason := range []string{"withheld", "deleted"} {
		t.Run(reason, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newReplySurfaceFixture(t)
			foreign, err := f.st.GetOrCreateSource("x", "2001")
			_testRequire.NoError(err)
			now := time.Now().UTC()
			copiedPost, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
				SourceID: foreign.ID, OwnerID: "2001", PostID: "9001", AuthorID: "1001",
				ConversationID: "100", EditHistory: []string{"9001"},
				Text: "synthetic copied reply body", CreatedAt: now, FetchedAt: now,
				Availability: "public", Raw: []byte(`{"post":{"id":"9001"}}`),
				Author: &store.XAuthorInput{UserID: "1001", Username: "example_owner", FetchedAt: now},
			})
			_testRequire.NoError(err)
			unrequestedCopy, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
				SourceID: foreign.ID, OwnerID: "2001", PostID: "9002", AuthorID: "1001",
				ConversationID: "100", EditHistory: []string{"9002"},
				Text: "synthetic unrelated reply body", CreatedAt: now, FetchedAt: now,
				Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: now},
			})
			_testRequire.NoError(err)

			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					f.posts.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprint(w, `{"title":"Synthetic uncertainty"}`)
					return
				}
				_testAssert.Equal("9001", r.URL.Query().Get("ids"))
				if reason == "deleted" {
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"9001","type":"https://api.x.com/2/problems/resource-not-found"},{"resource_type":"tweet","resource_id":"9002","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":"owner reply","created_at":"2026-10-31T06:00:00Z","edit_history_tweet_ids":["9001"],"referenced_tweets":[{"type":"replied_to","id":"102"}],"withheld":{"copyright":true}}]}`)
			}))
			t.Cleanup(provider.Close)
			f.adapter.xDeps.apiBaseURL = provider.URL + "/2"
			f.adapter.xDeps.httpClient = provider.Client()
			cleaner := NewXContentCleanup(f.st, f.cfg, nil)
			f.adapter.xCleanup = cleaner
			server := api.NewServerWithOptions(api.ServerOptions{
				Config: f.cfg, Store: f.adapter,
				Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()),
				Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead,
			})
			cleaner.SetServer(server)
			f.handler = server.Router()
			initial := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
			_testRequire.Contains(initial.Body.String(), "remote_unknown")

			body, err := json.Marshal(map[string]any{
				"draft_id": f.draft.DraftID, "revision": 1, "adopt_post": "9001",
			})
			_testRequire.NoError(err)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/x/replies/prepare", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
			out := httptest.NewRecorder()
			f.handler.ServeHTTP(out, request)
			_testAssert.Contains(out.Body.String(), "adoption_mismatch")
			_testAssert.Equal(int32(1), f.posts.Load(), "adoption lookup must never replay the uncertain create")
			pending, err := f.st.XCleanupPendingContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.False(pending, "the daemon boundary drains unavailable evidence after a mismatch")
			target, err := f.st.ResolveXPostContext(t.Context(), foreign.ID, "9001")
			_testRequire.NoError(err)
			_testAssert.Equal("removed", target.Availability,
				"suppressed targets stay unavailable to send resolution")
			assertXStoredSuppressionReason(t, f.st, foreign.ID, "9001", reason)
			_testAssert.Equal(copiedPost.MessageID, target.MessageID)
			unrequestedTarget, err := f.st.ResolveXPostContext(t.Context(), foreign.ID, "9002")
			_testRequire.NoError(err)
			_testAssert.Equal("public", unrequestedTarget.Availability,
				"a terminal result outside the requested-ID set must not suppress unrelated aliases")
			unrequestedBody, err := f.st.GetMessageBodyText(t.Context(), unrequestedCopy.MessageID)
			_testRequire.NoError(err)
			_testAssert.Equal("synthetic unrelated reply body", unrequestedBody)
			var bodies int
			_testRequire.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM message_bodies WHERE message_id=?`), copiedPost.MessageID).Scan(&bodies))
			_testAssert.Zero(bodies, "the mismatched provider evidence still purges known cross-source copies")
			var rawRows int
			_testRequire.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM message_raw WHERE message_id=?`), copiedPost.MessageID).Scan(&rawRows))
			_testAssert.Zero(rawRows, "the mismatched provider evidence purges retained raw content too")
		})
	}
}
