package line_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

type replyFixture struct {
	st     *store.Store
	client *line.Client
	fake   *lineapi.Server
	http   *httptest.Server
	draft  store.ChatDraft
	parent int64
	now    time.Time
	to     string
}

func newReplyFixture(t *testing.T, kind string, st *store.Store) *replyFixture {
	t.Helper()
	if st == nil {
		st = testutil.NewTestStore(t)
	}
	f := &replyFixture{st: st, fake: lineapi.New(), now: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)}
	f.http = httptest.NewServer(f.fake)
	t.Cleanup(f.http.Close)
	f.fake.SetClock(func() time.Time { return f.now })
	f.resetClient()
	source, err := line.Register(t.Context(), st, f.client, "2000000001")
	require.NoError(t, err)
	native, conversationType := "user:"+replyUser, "direct_chat"
	f.to = replyUser
	if kind == "group" {
		f.to = "C00000000000000000000000000000003"
		native = "group:" + f.to
		conversationType = "group_chat"
	}
	if kind == "room" {
		f.to = "R00000000000000000000000000000004"
		native = "room:" + f.to
		conversationType = "group_chat"
	}
	chat, err := st.EnsureConversationWithType(source.ID, native, conversationType, "Example")
	require.NoError(t, err)
	metadata := sql.NullString{String: `{"quoteToken":"synthetic-parent-quote"}`, Valid: true}
	f.parent, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "100", MessageType: "line"}, BodyText: sql.NullString{String: "Incoming", Valid: true}, Metadata: &metadata})
	require.NoError(t, err)
	f.draft, err = st.CreateChatDraftContext(t.Context(), chat, f.parent, "Thanks!", func(string, string) error { return nil })
	require.NoError(t, err)
	return f
}

func (f *replyFixture) resetClient() {
	f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(func() time.Time { return f.now }))
}
func (f *replyFixture) sender() *line.ReplySender {
	return &line.ReplySender{Store: f.st, Now: func() time.Time { return f.now }, Client: func(context.Context, store.LineReplyAttempt) (*line.Client, error) { return f.client, nil }}
}

func TestReplySendAndReceiptReplay(t *testing.T) {
	for _, kind := range []string{"user", "group", "room"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newReplyFixture(t, kind, nil)
			receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal("sent", receipt.Status)
			assert.True(receipt.Accepted)
			require.Positive(receipt.MessageID)
			assert.Equal("900000000000000001", receipt.RemoteMessageID)
			message, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessage(t.Context(), receipt.MessageID)
			require.NoError(err)
			if kind == "user" {
				assert.Len(message.To, 1)
			} else {
				assert.Empty(message.To)
			}
			target, err := f.st.GetOrCreateSource("line", "2000000002")
			require.NoError(err)
			_, err = f.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: f.draft.SourceID, IntoSourceID: target.ID})
			require.NoError(err)
			sender := f.sender()
			sender.Client = nil
			repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(receipt, repeated)
			assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
			assert.Equal(1, f.fake.AcceptedPushes())
			for _, request := range f.fake.Requests() {
				if request.Path == "/v2/bot/message/push" {
					var payload struct {
						To       string `json:"to"`
						Messages []struct {
							QuoteToken string `json:"quoteToken"`
						} `json:"messages"`
					}
					require.NoError(json.Unmarshal(request.Body, &payload))
					assert.Equal(f.to, payload.To)
					require.Len(payload.Messages, 1)
					assert.Equal("synthetic-parent-quote", payload.Messages[0].QuoteToken)
				}
			}
		})
	}
}

func TestReplyLostResponseThenRejectionKeepsOriginalKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
	unknown, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("remote_unknown", unknown.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
	_, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE messages SET metadata=? WHERE id=?`), `{"quoteToken":"changed-parent-quote"}`, f.parent)
	require.NoError(err)
	f.now = f.now.Add(31 * time.Second)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 400, Body: `untrusted provider detail`})
	unknown, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("remote_unknown", unknown.Status)
	_, err = f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "changed")
	require.ErrorIs(err, store.ErrChatDraftFrozen)
	f.now = f.now.Add(31 * time.Second)
	sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
	keys := map[string]bool{}
	var first []byte
	for _, request := range f.fake.Requests() {
		if request.Path == "/v2/bot/message/push" {
			keys[request.Header.Get("X-Line-Retry-Key")] = true
			if first == nil {
				first = request.Body
			} else {
				assert.Equal(first, request.Body)
			}
		}
	}
	assert.Len(keys, 1)
}

func TestReplyAdmissionFailuresDoNotFreeze(t *testing.T) {
	for _, kind := range []string{"invalid text", "invalid recipient", "disabled policy", "missing client"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			f := newReplyFixture(t, "user", nil)
			sender := f.sender()
			denied := errors.New("synthetic disabled policy")
			if kind == "invalid text" {
				edited, err := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "")
				require.NoError(err)
				f.draft = edited
			}
			if kind == "invalid recipient" {
				_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE conversations SET source_conversation_id='user:invalid' WHERE id=?`), f.draft.ConversationID)
				require.NoError(err)
				_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE chat_drafts SET source_conversation_id='user:invalid' WHERE draft_id=?`), f.draft.DraftID)
				require.NoError(err)
			}
			if kind == "disabled policy" {
				sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) { return nil, denied }
			}
			if kind == "missing client" {
				sender.Client = nil
			}
			before := len(f.fake.Requests())
			_, err := sender.Send(t.Context(), f.draft.DraftID, f.draft.Revision)
			require.Error(err)
			if kind == "invalid recipient" {
				require.ErrorIs(err, store.ErrChatDraftInvalidDestination)
			}
			assert.Len(t, f.fake.Requests(), before)
		})
	}
}

func TestReplyKnownReceiptRecoveryIgnoresProviderGates(t *testing.T) {
	for _, phase := range []string{"admitted", "accepted", "lost response after parent GC", "known receipt after parent GC"} {
		t.Run(phase, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "reply.db")
			f := newReplyFixture(t, "user", testutil.NewSQLiteTestStoreAt(t, path))
			gc := strings.HasSuffix(phase, "after parent GC")
			if gc {
				if phase == "lost response after parent GC" {
					f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
				} else {
					_, err := f.st.DB().Exec(`CREATE TRIGGER fail_reply_archive BEFORE INSERT ON messages WHEN NEW.is_from_me=1 BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`)
					require.NoError(err)
				}
				pending, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				if phase == "lost response after parent GC" {
					require.NoError(err)
					assert.Equal(store.LineReplyRemoteUnknown, pending.Status)
				} else {
					require.Error(err)
					assert.Equal("sent_archive_pending", pending.Status)
					assert.True(pending.Accepted)
					assert.Equal("900000000000000001", pending.RemoteMessageID)
					assert.Zero(pending.MessageID)
					assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
					_, err = f.st.DB().Exec(`DROP TRIGGER fail_reply_archive`)
					require.NoError(err)
				}
				require.NoError(f.st.MarkMessageDeletedContext(t.Context(), f.draft.SourceID, "100"))
				plan, err := f.st.PlanGCContext(t.Context())
				require.NoError(err)
				assert.Equal(int64(1), plan.SourceDeleted)
				deleted, err := f.st.ExecuteGCContext(t.Context(), plan)
				require.NoError(err)
				assert.Equal(int64(1), deleted)
			} else {
				_, err := f.st.AdmitLineReplyContext(t.Context(), f.draft.DraftID, 1, f.now, func(store.LineReplyAttempt) error { return nil })
				require.NoError(err)
				if phase == "accepted" {
					attempt, err := f.st.StartLineReplyRequestContext(t.Context(), f.draft.DraftID, f.now, f.now.Add(30*time.Second))
					require.NoError(err)
					result, err := f.client.PushText(t.Context(), attempt.RetryKey, line.PushTextRequest{To: f.to, Text: attempt.Body, QuoteToken: attempt.QuoteToken})
					require.NoError(err)
					_, err = f.st.RecordLineReplyOutcomeContext(t.Context(), f.draft.DraftID, store.LineReplyOutcome{Accepted: true, StatusCode: result.StatusCode, MessageID: result.MessageID, QuoteToken: result.QuoteToken, RequestID: result.RequestID})
					require.NoError(err)
				}
			}
			require.NoError(f.st.Close())
			reopened, err := store.Open(path)
			require.NoError(err)
			t.Cleanup(func() { _ = reopened.Close() })
			f.st = reopened
			f.resetClient()
			sender := f.sender()
			if phase == "accepted" {
				f.now = f.now.Add(25 * time.Hour)
				sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) {
					return nil, errors.New("revoked policy and removed secret")
				}
			} else {
				f.now = f.now.Add(31 * time.Second)
			}
			sent, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal("sent", sent.Status)
			assert.Equal(1, f.fake.AcceptedPushes())
			pushes := 1
			if phase == "lost response after parent GC" {
				pushes = 2
			}
			assert.Equal(pushes, f.fake.Count("POST", "/v2/bot/message/push"))
			if gc {
				var parent sql.NullInt64
				require.NoError(f.st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE id=?`, sent.MessageID).Scan(&parent))
				assert.False(parent.Valid)
				body, err := f.st.GetMessageBodyText(sent.MessageID)
				require.NoError(err)
				assert.Equal("Thanks!", body)
			}
		})
	}
}

func TestReplyExpiryPermanentlyStopsProviderIO(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
	_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	f.now = f.now.Add(23 * time.Hour)
	sender := f.sender()
	sender.Client = nil
	expired, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("remote_unknown_expired", expired.Status)
	f.now = f.now.Add(2 * time.Hour)
	repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(expired, repeated)
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
	assert.Equal(1, f.fake.AcceptedPushes())
}

func TestReplyDefiniteRejectionIsTerminal(t *testing.T) {
	for _, status := range []int{400, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newReplyFixture(t, "user", nil)
			f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: status, Header: http.Header{"Retry-After": {"180"}}})
			rejected, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal("provider_rejected", rejected.Status)
			assert.Nil(rejected.RetryAfter)
			f.now = f.now.Add(3 * time.Minute)
			require.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1))
			sender := f.sender()
			sender.Client = nil
			repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(rejected, repeated)
			_, err = sender.Send(t.Context(), f.draft.DraftID, 2)
			require.ErrorIs(err, store.ErrChatDraftRevisionConflict)
			assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
			assert.Zero(f.fake.AcceptedPushes())
		})
	}
}

func TestReplyRateLimitClassification(t *testing.T) {
	t.Run("retry after lost response", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
		_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		f.now = f.now.Add(31 * time.Second)
		retryAt := f.now.Add(3 * time.Minute)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"180"}}})
		receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(store.LineReplyRemoteUnknown, receipt.Status)
		require.NotNil(receipt.RetryAfter)
		assert.Equal(retryAt, *receipt.RetryAfter)
		f.now = retryAt.Add(-time.Second)
		waiting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(receipt, waiting)
		assert.Equal(2, f.fake.Count("POST", "/v2/bot/message/push"))
		f.now = retryAt
		sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(store.LineReplySent, sent.Status)
		assert.Equal(1, f.fake.AcceptedPushes())
		var keys []string
		for _, request := range f.fake.Requests() {
			if request.Path == "/v2/bot/message/push" {
				keys = append(keys, request.Header.Get("X-Line-Retry-Key"))
			}
		}
		require.Len(keys, 3)
		assert.Equal(keys[0], keys[1])
		assert.Equal(keys[0], keys[2])
	})
	t.Run("deadline from response time", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		initial := f.now
		f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(func() time.Time { return f.now }), line.WithHTTPClient(&http.Client{Transport: replyRoundTrip(func(request *http.Request) (*http.Response, error) {
			response, err := http.DefaultTransport.RoundTrip(request)
			if request.URL.Path == "/v2/bot/message/push" {
				f.now = f.now.Add(time.Minute)
			}
			return response, err
		})}))
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 503, Header: http.Header{"Retry-After": {"180"}}})
		receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		require.NotNil(receipt.RetryAfter)
		assert.Equal(initial.Add(4*time.Minute), *receipt.RetryAfter)
		f.now = initial.Add(2 * time.Minute)
		_, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
	})
}

func TestReplyConcurrentCallsAcceptOnce(t *testing.T) {
	t.Run("concurrent admission", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		type result struct {
			receipt line.ReplyReceipt
			err     error
		}
		results := make(chan result, 8)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				results <- result{receipt, err}
			})
		}
		wg.Wait()
		close(results)
		for result := range results {
			if result.err != nil {
				require.ErrorIs(result.err, store.ErrSyncAlreadyActive)
			} else {
				assert.Contains([]string{"sent", "remote_unknown"}, result.receipt.Status)
			}
		}
		receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal("sent", receipt.Status)
		assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
		assert.Equal(1, f.fake.AcceptedPushes())
	})
	// SQL tracing pauses the draft lookup after the receipt lookup has completed.
	// Actual Store queries and the fake HTTP API execute both sends.
	t.Run("completion between receipt and draft lookup", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", testutil.NewSQLiteTestStore(t))
		handler := &replyLookupHandler{arrived: make(chan struct{}), release: make(chan struct{})}
		prior := slog.Default()
		slog.SetDefault(slog.New(handler))
		store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
		t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}); slog.SetDefault(prior) })
		var release sync.Once
		defer release.Do(func() { close(handler.release) })
		type result struct {
			receipt line.ReplyReceipt
			err     error
		}
		completed := make(chan result, 1)
		go func() {
			receipt, err := f.sender().Send(store.WithRequestID(t.Context(), "reply-lookup-race"), f.draft.DraftID, 1)
			completed <- result{receipt, err}
		}()
		select {
		case <-handler.arrived:
		case <-time.After(30 * time.Second):
			require.FailNow("second sender did not reach draft lookup")
		}
		first, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		release.Do(func() { close(handler.release) })
		second := <-completed
		require.NoError(second.err)
		assert.Equal(first, second.receipt)
		assert.Equal(1, f.fake.AcceptedPushes())
	})
}

type replyRoundTrip func(*http.Request) (*http.Response, error)

func (f replyRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestReplyAcceptanceSurvivesCallerCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(func() time.Time { return f.now }), line.WithHTTPClient(&http.Client{Transport: replyRoundTrip(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil && request.URL.Path == "/v2/bot/message/push" {
			response.Body = replyClosingBody{ReadCloser: response.Body, closed: cancel}
		}
		return response, err
	})}))
	receipt, err := f.sender().Send(ctx, f.draft.DraftID, 1)
	require.NoError(err)
	require.ErrorIs(ctx.Err(), context.Canceled)
	assert.Equal("sent", receipt.Status)
	repeated, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(receipt, repeated)
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
}

type replyClosingBody struct {
	io.ReadCloser

	closed func()
}

func (b replyClosingBody) Close() error { err := b.ReadCloser.Close(); b.closed(); return err }

func TestReplyKnownRemoteIDSurvivesReceiptStoreFailure(t *testing.T) {
	for _, recovery := range []string{"same-key retry", "revoked policy"} {
		t.Run(recovery, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "receipt.db")
			f := newReplyFixture(t, "user", testutil.NewSQLiteTestStoreAt(t, path))
			f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(func() time.Time { return f.now }), line.WithHTTPClient(&http.Client{Transport: replyRoundTrip(func(request *http.Request) (*http.Response, error) {
				response, err := http.DefaultTransport.RoundTrip(request)
				if err == nil && request.URL.Path == "/v2/bot/message/push" {
					response.Body = replyClosingBody{ReadCloser: response.Body, closed: func() { require.NoError(f.st.Close()) }}
				}
				return response, err
			})}))
			receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			require.Error(err)
			assert.Equal("accepted_receipt_pending", receipt.Status)
			assert.True(receipt.Accepted)
			assert.Equal("synthetic-request-1", receipt.RequestID)
			assert.Equal(f.draft.DraftID, receipt.DraftID)
			assert.Equal(int64(1), receipt.Revision)
			assert.Equal(f.draft.SourceID, receipt.SourceID)
			assert.Equal(f.draft.ConversationID, receipt.ConversationID)
			assert.Equal("900000000000000001", receipt.RemoteMessageID)
			reopened, err := store.Open(path)
			require.NoError(err)
			t.Cleanup(func() { _ = reopened.Close() })
			f.st = reopened
			durable, err := reopened.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Equal(store.LineReplyRemoteUnknown, durable.State)
			assert.Empty(durable.RemoteMessageID)
			f.resetClient()
			f.now = f.now.Add(31 * time.Second)
			sender := f.sender()
			if recovery == "revoked policy" {
				sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) {
					return nil, errors.New("synthetic revoked policy")
				}
				pending, err := sender.Send(t.Context(), f.draft.DraftID, 1)
				require.ErrorContains(err, "synthetic revoked policy")
				assert.Equal(store.LineReplyRemoteUnknown, pending.Status)
				assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
				return
			}
			sent, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal("sent", sent.Status)
			assert.Equal(1, f.fake.AcceptedPushes())
		})
	}
}

func TestReplyExpiryCommitFailureKeepsPendingState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", testutil.NewTestStore(t))
	_, err := f.st.AdmitLineReplyContext(t.Context(), f.draft.DraftID, 1, f.now, func(store.LineReplyAttempt) error { return nil })
	require.NoError(err)
	_, err = f.st.StartLineReplyRequestContext(t.Context(), f.draft.DraftID, f.now, f.now.Add(30*time.Second))
	require.NoError(err)
	_, err = f.st.DB().Exec(`CREATE TABLE line_reply_expiry_guard (source_id BIGINT REFERENCES sources(id) DEFERRABLE INITIALLY DEFERRED)`)
	require.NoError(err)
	dropTrigger := `DROP TRIGGER fail_line_reply_expiry`
	if f.st.IsPostgreSQL() {
		_, err = f.st.DB().Exec(`CREATE FUNCTION fail_line_reply_expiry() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN INSERT INTO line_reply_expiry_guard(source_id) VALUES(-1); RETURN NEW; END; $$`)
		require.NoError(err)
		_, err = f.st.DB().Exec(`CREATE TRIGGER fail_line_reply_expiry AFTER UPDATE ON line_reply_attempts FOR EACH ROW WHEN (NEW.state IN ('remote_unknown_expired','accepted_unresolved_expired')) EXECUTE FUNCTION fail_line_reply_expiry()`)
		dropTrigger += ` ON line_reply_attempts`
	} else {
		_, err = f.st.DB().Exec(`CREATE TRIGGER fail_line_reply_expiry AFTER UPDATE ON line_reply_attempts WHEN NEW.state IN ('remote_unknown_expired','accepted_unresolved_expired') BEGIN INSERT INTO line_reply_expiry_guard(source_id) VALUES(-1); END`)
	}
	require.NoError(err)
	f.now = f.now.Add(23 * time.Hour)
	pending, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.Error(err)
	require.NotErrorIs(err, store.ErrLineReplyExpired)
	assert.Equal(store.LineReplyRemoteUnknown, pending.Status)
	durable, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal(store.LineReplyRemoteUnknown, durable.State)
	assert.Equal(0, f.fake.Count("POST", "/v2/bot/message/push"))
	_, err = f.st.DB().Exec(dropTrigger)
	require.NoError(err)
	expired, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(store.LineReplyRemoteUnknownExpired, expired.Status)
	assert.Equal(0, f.fake.Count("POST", "/v2/bot/message/push"))
}

func TestReplyConflictingReceiptKeepsDurableIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 200, Body: `{}`})
	unresolved, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("accepted_unresolved", unresolved.Status)
	assert.True(unresolved.Accepted)
	for _, status := range []int{503, 400} {
		f.now = f.now.Add(31 * time.Second)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: status, Body: `untrusted provider detail`})
		replayed, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal("accepted_unresolved", replayed.Status)
		assert.True(replayed.Accepted)
	}
	f.now = f.now.Add(31 * time.Second)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 409, Header: http.Header{"X-Line-Accepted-Request-Id": {"synthetic-conflict-0"}}, Body: `{"sentMessages":[{"id":"` + f.draft.DraftID + `-0"}]}`})
	conflicting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.ErrorIs(err, store.ErrLineReplyReceiptConflict)
	assert.Equal("accepted_unresolved", conflicting.Status)
	assert.True(conflicting.Accepted)
	assert.Empty(conflicting.RemoteMessageID)
	assert.Equal(unresolved.RequestID, conflicting.RequestID)
	f.now = f.now.Add(31 * time.Second)
	sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
}

type replyLookupHandler struct {
	arrived, release chan struct{}
	once             sync.Once
}

func (*replyLookupHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *replyLookupHandler) Handle(_ context.Context, record slog.Record) error {
	tagged, draft := false, false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "request_id" && attr.Value.String() == "reply-lookup-race" {
			tagged = true
		}
		if attr.Key == "stmt" && strings.Contains(attr.Value.String(), "FROM chat_drafts d") {
			draft = true
		}
		return true
	})
	if tagged && draft {
		h.once.Do(func() { close(h.arrived); <-h.release })
	}
	return nil
}
func (h *replyLookupHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *replyLookupHandler) WithGroup(string) slog.Handler      { return h }

func TestReplyTokenFailuresHonorRetryAfter(t *testing.T) {
	for _, phase := range []string{"initial", "recovery"} {
		for _, status := range []int{400, 429, 503} {
			t.Run(phase+"/"+http.StatusText(status), func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				f := newReplyFixture(t, "user", nil)
				state := store.LineReplyAdmitted
				pushes := 0
				if phase == "recovery" {
					f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
					_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
					must.NoError(err)
					f.now = f.now.Add(31 * time.Second)
					state = store.LineReplyRemoteUnknown
					pushes = 1
				}
				f.resetClient()
				before := f.fake.Count(http.MethodPost, "/oauth2/v3/token")
				f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: status, Header: http.Header{"Retry-After": {"180"}}})
				wait := 3 * time.Minute
				if status == http.StatusBadRequest {
					wait = 30 * time.Second
				}
				receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.ErrorIs(err, line.ErrCredentialsUnavailable)
				checks.Equal(state, receipt.Status)
				must.NotNil(receipt.RetryAfter)
				checks.Equal(f.now.Add(wait), *receipt.RetryAfter)
				attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
				must.NoError(err)
				checks.Equal(state, attempt.State)
				checks.Equal(*receipt.RetryAfter, attempt.NextRetryAt)
				waiting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				checks.Equal(receipt, waiting)
				f.now = f.now.Add(wait - time.Second)
				_, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				checks.Equal(before+1, f.fake.Count(http.MethodPost, "/oauth2/v3/token"))
				checks.Equal(pushes, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
				f.now = f.now.Add(time.Second)
				sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				checks.Equal(store.LineReplySent, sent.Status)
				completed, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
				must.NoError(err)
				checks.Equal(attempt.RetryKey, completed.RetryKey)
				if phase == "initial" {
					checks.Equal(f.now.Add(23*time.Hour), completed.RetryUntil)
				} else {
					checks.Equal(attempt.RetryUntil, completed.RetryUntil)
				}
			})
		}
	}
}

func TestReplyCredentialFailuresRetrySameKey(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		response       lineapi.Response
	}{
		{"token 400", "/oauth2/v3/token", lineapi.Response{Status: 400}},
		{"token 401", "/oauth2/v3/token", lineapi.Response{Status: 401}},
		{"token 403", "/oauth2/v3/token", lineapi.Response{Status: 403}},
		{"token 500", "/oauth2/v3/token", lineapi.Response{Status: 500}},
		{"invalid token", "/oauth2/v3/token", lineapi.Response{Status: 200, Body: `{}`}},
		{"token disconnect", "/oauth2/v3/token", lineapi.Response{Disconnect: true}},
		{"push 401", "/v2/bot/message/push", lineapi.Response{Status: 401}},
		{"push 403", "/v2/bot/message/push", lineapi.Response{Status: 403}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newReplyFixture(t, "user", nil)
			f.resetClient()
			responses := []lineapi.Response{tc.response}
			if tc.endpoint == "/v2/bot/message/push" && tc.response.Status == http.StatusUnauthorized {
				responses = append(responses, tc.response)
			}
			f.fake.Enqueue("POST", tc.endpoint, responses...)
			pending, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			require.ErrorIs(err, line.ErrCredentialsUnavailable)
			pushes := f.fake.Count("POST", "/v2/bot/message/push")
			if tc.endpoint == "/oauth2/v3/token" {
				assert.Zero(pushes)
			}
			attempt, readErr := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
			require.NoError(readErr)
			assert.Zero(attempt.StatusCode, "credential errors are not submission outcomes")
			if tc.endpoint == "/oauth2/v3/token" {
				assert.Equal(store.LineReplyAdmitted, pending.Status)
			} else {
				assert.Equal(store.LineReplyRemoteUnknown, pending.Status)
			}
			f.now = f.now.Add(31 * time.Second)
			sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(store.LineReplySent, sent.Status)
			assert.Equal(1, f.fake.AcceptedPushes())
			var keys []string
			for _, request := range f.fake.Requests() {
				if request.Path == "/v2/bot/message/push" {
					keys = append(keys, request.Header.Get("X-Line-Retry-Key"))
				}
			}
			require.Len(keys, pushes+1)
			for _, key := range keys {
				assert.Equal(attempt.RetryKey, key)
			}
		})
	}
}

func TestReplyPushForbiddenThenBadRequestIsTerminal(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Status: http.StatusForbidden})
	_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	must.ErrorIs(err, line.ErrCredentialsUnavailable)
	attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
	must.NoError(err)
	checks.Zero(f.fake.AcceptedPushes())

	f.resetClient()
	f.now = f.now.Add(31 * time.Second)
	f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Status: http.StatusBadRequest})
	rejected, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	must.NoError(err)
	checks.Equal(store.LineReplyProviderRejected, rejected.Status)
	checks.False(rejected.Accepted)
	completed, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
	must.NoError(err)
	checks.Equal(store.LineReplyProviderRejected, completed.State)
	checks.Equal(http.StatusBadRequest, completed.StatusCode)
	checks.Equal(attempt.RetryKey, completed.RetryKey)
	checks.Equal(2, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
	checks.Zero(f.fake.AcceptedPushes())

	f.now = f.now.Add(31 * time.Second)
	replayed, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	must.NoError(err)
	checks.Equal(rejected, replayed)
	checks.Equal(2, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
}

func TestReplyDirectRecipientFromIngestedChat(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newReplyFixture(t, "user", nil)
	event := line.Event{Type: "message", Timestamp: f.now.UnixMilli(), WebhookEventID: "incoming-direct", Source: line.Source{Type: "user", UserID: replyUser}, Message: &line.Message{ID: "101", Type: "text", Text: "Incoming reply target", QuoteToken: "synthetic-direct-quote"}}
	raw, err := json.Marshal(event)
	must.NoError(err)
	bot, err := f.st.LineBotUserIDContext(t.Context(), f.draft.SourceID)
	must.NoError(err)
	must.NoError(f.st.EnqueueLineEventsContext(t.Context(), f.draft.SourceID, "2000000001", bot, []store.LineWebhookEvent{{WebhookEventID: event.WebhookEventID, EventType: event.Type, TargetMessageID: event.Message.ID, EventTimestamp: event.Timestamp, ReceivedAt: f.now, RawJSON: string(raw)}}))
	processor := line.NewProcessor(f.st, f.client, f.draft.SourceID, "2000000001", t.TempDir(), line.WithProcessorClock(func() time.Time { return f.now }))
	must.NoError(processor.Process(t.Context()))
	parent, err := f.st.LineMessageContext(t.Context(), f.draft.SourceID, "101")
	must.NoError(err)
	draft, err := f.st.CreateChatDraftContext(t.Context(), f.draft.ConversationID, parent.ID, "Direct response", func(string, string) error { return nil })
	must.NoError(err)
	receipt, err := f.sender().Send(t.Context(), draft.DraftID, 1)
	must.NoError(err)
	message, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessage(t.Context(), receipt.MessageID)
	must.NoError(err)
	must.Len(message.To, 1)
	checks.Equal("Example User", message.To[0].Name)
}

func TestReplyTokenFailureDatesArchiveAtFirstPush(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newReplyFixture(t, "user", nil)
	f.resetClient()
	f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: http.StatusUnauthorized})
	_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	must.ErrorIs(err, line.ErrCredentialsUnavailable)
	checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
	f.now = f.now.Add(2 * time.Hour)
	receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	must.NoError(err)
	checks.Equal(store.LineReplySent, receipt.Status)
	message, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessage(t.Context(), receipt.MessageID)
	must.NoError(err)
	checks.Equal(f.now, message.SentAt.UTC())
	attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
	must.NoError(err)
	checks.Equal(f.now.Add(23*time.Hour), attempt.RetryUntil)
}

func TestReplyGroupMessageProjectsBotMembership(t *testing.T) {
	for _, kind := range []string{"group", "room"} {
		t.Run(kind, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newReplyFixture(t, kind, nil)
			must.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1))
			// Receiving starts after the bot joined, so there is no join event.
			event := line.Event{Type: "message", Timestamp: f.now.UnixMilli(), WebhookEventID: "incoming-group", Source: line.Source{Type: kind, UserID: replyUser}, Message: &line.Message{ID: "101", Type: "text", Text: "Incoming reply target"}}
			if kind == "group" {
				event.Source.GroupID = f.to
			} else {
				event.Source.RoomID = f.to
			}
			raw, err := json.Marshal(event)
			must.NoError(err)
			bot, err := f.st.LineBotUserIDContext(t.Context(), f.draft.SourceID)
			must.NoError(err)
			must.NoError(f.st.EnqueueLineEventsContext(t.Context(), f.draft.SourceID, "2000000001", bot, []store.LineWebhookEvent{{WebhookEventID: event.WebhookEventID, EventType: event.Type, TargetMessageID: event.Message.ID, EventTimestamp: event.Timestamp, ReceivedAt: f.now, RawJSON: string(raw)}}))
			processor := line.NewProcessor(f.st, f.client, f.draft.SourceID, "2000000001", t.TempDir(), line.WithProcessorClock(func() time.Time { return f.now }))
			must.NoError(processor.Process(t.Context()))
			var count int
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT participant_count FROM conversations WHERE id=?`), f.draft.ConversationID).Scan(&count))
			checks.Equal(1, count)
			parent, err := f.st.LineMessageContext(t.Context(), f.draft.SourceID, "101")
			must.NoError(err)
			draft, err := f.st.CreateChatDraftContext(t.Context(), f.draft.ConversationID, parent.ID, "Group response", func(string, string) error { return nil })
			must.NoError(err)
			receipt, err := f.sender().Send(t.Context(), draft.DraftID, 1)
			must.NoError(err)
			must.Equal(store.LineReplySent, receipt.Status)
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT participant_count FROM conversations WHERE id=?`), draft.ConversationID).Scan(&count))
			checks.Equal(2, count)
			var active bool
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT cp.left_at IS NULL FROM conversation_participants cp JOIN participant_identifiers i ON i.participant_id=cp.participant_id WHERE cp.conversation_id=? AND i.identifier_type='line_user_id' AND i.identifier_value=?`), draft.ConversationID, "channel:2000000001:user:"+bot).Scan(&active))
			checks.True(active)
		})
	}
}

func TestReplyUnsubmittedDeletedDraftAllowsSourceMerge(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newReplyFixture(t, "user", nil)
	f.resetClient()
	f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: http.StatusInternalServerError})
	sender := f.sender()
	receipt, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	must.ErrorIs(err, line.ErrCredentialsUnavailable)
	must.Equal(store.LineReplyAdmitted, receipt.Status)
	checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
	must.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1))
	sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) {
		return nil, errors.New("synthetic revoked sending policy")
	}
	must.NotNil(receipt.RetryAfter)
	f.now = receipt.RetryAfter.Add(time.Second)
	_, err = sender.Send(t.Context(), f.draft.DraftID, 1)
	must.ErrorContains(err, "synthetic revoked sending policy")
	checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
	target, err := f.st.GetOrCreateSource("line", "2000000002")
	must.NoError(err)
	_, err = f.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: f.draft.SourceID, IntoSourceID: target.ID, DryRun: true})
	must.NoError(err)
	previewed, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
	must.NoError(err)
	checks.Equal(store.LineReplyAdmitted, previewed.State)
	_, err = f.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: f.draft.SourceID, IntoSourceID: target.ID})
	must.NoError(err)
	attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
	must.NoError(err)
	checks.Equal(store.LineReplyRemoteUnknownExpired, attempt.State)
}
