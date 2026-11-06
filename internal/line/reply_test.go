package line_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
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
	clock  atomic.Pointer[time.Time]
	to     string
}

func newReplyFixture(t *testing.T, kind string, st *store.Store) *replyFixture {
	t.Helper()
	if st == nil {
		st = testutil.NewTestStore(t)
	}
	f := &replyFixture{st: st, fake: lineapi.New()}
	f.setNow(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	f.http = httptest.NewServer(f.fake)
	t.Cleanup(f.http.Close)
	f.fake.SetClock(f.now)
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

func (f *replyFixture) now() time.Time {
	return *f.clock.Load()
}

func (f *replyFixture) setNow(now time.Time) {
	f.clock.Store(new(now))
}

func (f *replyFixture) resetClient() {
	f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(f.now))
}
func (f *replyFixture) sender() *line.ReplySender {
	return &line.ReplySender{Store: f.st, Now: f.now, Client: func(context.Context, store.LineReplyAttempt) (*line.Client, error) { return f.client, nil }}
}

func TestReplyAcceptanceWithoutMessageIDSurvivesExpiry(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newReplyFixture(t, "user", nil)
			header := http.Header{"X-Line-Request-Id": {"accepted-request"}}
			if status == http.StatusConflict {
				header.Set("X-Line-Accepted-Request-Id", "accepted-request")
				header.Set("X-Line-Request-Id", "retry-request")
			}
			f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Status: status, Header: header, Body: `{}`})
			sender := f.sender()
			receipt, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(store.LineReplyRemoteUnknown, receipt.Status)
			assert.True(receipt.Accepted)
			assert.Equal("accepted-request", receipt.RequestID)
			assert.Empty(receipt.RemoteMessageID)
			require.NotNil(receipt.RetryAfter)
			repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(receipt, repeated)
			f.setNow(f.now().Add(24 * time.Hour))
			expired, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(store.LineReplyExpired, expired.Status)
			assert.True(expired.Accepted)
			assert.Equal("accepted-request", expired.RequestID)
			assert.Nil(expired.RetryAfter)
			repeated, err = sender.Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(expired, repeated)
			assert.Equal(1, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
		})
	}
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
	f.setNow(f.now().Add(31 * time.Second))
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 400, Body: `untrusted provider detail`})
	unknown, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("remote_unknown", unknown.Status)
	f.setNow(f.now().Add(31 * time.Second))
	sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
	assert.Positive(sent.MessageID)
	archived, err := f.st.GetMessage(sent.MessageID)
	require.NoError(err)
	assert.Equal("900000000000000001", archived.SourceMessageID)
	_, err = f.st.GetChatDraftContext(t.Context(), f.draft.DraftID)
	require.ErrorIs(err, store.ErrChatDraftNotFound)
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

func TestReplyRejectsBeforeProviderIO(t *testing.T) {
	for _, kind := range []string{"invalid text", "changed conversation type", "disabled policy", "missing client"} {
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
			if kind == "changed conversation type" {
				_, err := f.st.EnsureConversationWithTypeContext(t.Context(), f.draft.SourceID, f.draft.SourceConversationID, "group_chat", "Example")
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
			if kind == "changed conversation type" {
				require.ErrorIs(err, store.ErrChatDraftInvalidDestination)
			}
			assert.Len(t, f.fake.Requests(), before)
		})
	}
}

func TestReplyKnownReceiptRecoveryIgnoresProviderGates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "reply.db")
	f := newReplyFixture(t, "user", testutil.NewSQLiteTestStoreAt(t, path))
	_, err := f.st.AdmitLineReplyContext(t.Context(), f.draft.DraftID, 1, f.now(), func(store.LineReplyAttempt) error { return nil })
	require.NoError(err)
	attempt, err := f.st.StartLineReplyRequestContext(t.Context(), f.draft.DraftID, f.now(), f.now().Add(30*time.Second))
	require.NoError(err)
	result, err := f.client.PushText(t.Context(), attempt.RetryKey, line.PushTextRequest{To: f.to, Text: attempt.Body, QuoteToken: attempt.QuoteToken})
	require.NoError(err)
	_, err = f.st.RecordLineReplyOutcomeContext(t.Context(), f.draft.DraftID, store.LineReplyOutcome{Accepted: true, StatusCode: result.StatusCode, MessageID: result.MessageID, QuoteToken: result.QuoteToken, RequestID: result.RequestID})
	require.NoError(err)
	require.NoError(f.st.Close())
	reopened, err := store.Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	f.st = reopened
	f.resetClient()
	sender := f.sender()
	f.setNow(f.now().Add(25 * time.Hour))
	sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) {
		return nil, errors.New("revoked policy and removed secret")
	}
	sent, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
}

func TestReplyExpiryPermanentlyStopsProviderIO(t *testing.T) {
	t.Run("elapsed deadline", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
		_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		f.setNow(f.now().Add(23 * time.Hour))
		sender := f.sender()
		sender.Client = nil
		expired, err := sender.Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal("expired", expired.Status)
		f.setNow(f.now().Add(2 * time.Hour))
		repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(expired, repeated)
		assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
		assert.Equal(1, f.fake.AcceptedPushes())
	})
	t.Run("deleted admitted attempt", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		f := newReplyFixture(t, "user", nil)
		f.resetClient()
		f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: http.StatusUnauthorized})
		sender := f.sender()
		receipt, err := sender.Send(t.Context(), f.draft.DraftID, 1)
		must.ErrorIs(err, line.ErrCredentialsUnavailable)
		must.Equal(store.LineReplyAdmitted, receipt.Status)
		must.NotNil(receipt.RetryAfter)
		checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
		must.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1, f.now()))
		_, err = f.st.GetChatDraftContext(t.Context(), f.draft.DraftID)
		must.ErrorIs(err, store.ErrChatDraftNotFound)
		f.setNow(receipt.RetryAfter.Add(time.Second))
		repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
		must.NoError(err)
		checks.Equal(store.LineReplyExpired, repeated.Status)
		checks.False(repeated.Accepted)
		checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
		attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
		must.NoError(err)
		checks.Equal(store.LineReplyExpired, attempt.State)
	})
	t.Run("admitted token cooldown past expiry", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		f := newReplyFixture(t, "user", nil)
		initial := f.now()
		f.resetClient()
		f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: http.StatusUnauthorized})
		sender := f.sender()
		pending, err := sender.Send(t.Context(), f.draft.DraftID, 1)
		must.ErrorIs(err, line.ErrCredentialsUnavailable)
		must.Equal(store.LineReplyAdmitted, pending.Status)
		f.setNow(initial.Add(22 * time.Hour))
		f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"90000"}}})
		pending, err = sender.Send(t.Context(), f.draft.DraftID, 1)
		must.ErrorIs(err, line.ErrCredentialsUnavailable)
		must.Equal(store.LineReplyAdmitted, pending.Status)
		must.NotNil(pending.RetryAfter)
		checks.True(pending.RetryAfter.After(initial.Add(23 * time.Hour)))
		f.setNow(initial.Add(23 * time.Hour))
		sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) {
			return nil, errors.New("synthetic revoked sending policy")
		}
		expired, err := sender.Send(t.Context(), f.draft.DraftID, 1)
		must.NoError(err)
		checks.Equal(store.LineReplyExpired, expired.Status)
		checks.False(expired.Accepted)
		attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
		must.NoError(err)
		checks.Equal(store.LineReplyExpired, attempt.State)
		checks.Equal(3, f.fake.Count(http.MethodPost, "/oauth2/v3/token"), "registration and two failed acquisitions; expiry makes no token request")
		checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
	})
}

func TestReplyDefiniteRejectionIsTerminal(t *testing.T) {
	t.Run("Bad Request", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: http.StatusBadRequest, Header: http.Header{"Retry-After": {"180"}}})
		rejected, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)

		assert.Equal("provider_rejected", rejected.Status)
		assert.Nil(rejected.RetryAfter)
		f.setNow(f.now().Add(3 * time.Minute))
		require.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1, f.now()))
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

	t.Run("403 then 400", func(t *testing.T) {
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
		f.setNow(f.now().Add(31 * time.Second))
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

		f.setNow(f.now().Add(31 * time.Second))
		replayed, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		must.NoError(err)
		checks.Equal(rejected, replayed)
		checks.Equal(2, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
	})
}

func TestReplyRateLimitClassification(t *testing.T) {
	t.Run("retry after lost response", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
		_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		f.setNow(f.now().Add(31 * time.Second))
		retryAt := f.now().Add(3 * time.Minute)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"180"}}})
		receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(store.LineReplyRemoteUnknown, receipt.Status)
		require.NotNil(receipt.RetryAfter)
		assert.Equal(retryAt, *receipt.RetryAfter)
		f.setNow(retryAt.Add(-time.Second))
		waiting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(receipt, waiting)
		assert.Equal(2, f.fake.Count("POST", "/v2/bot/message/push"))
		f.setNow(retryAt)
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
		initial := f.now()
		f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(f.now), line.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response, err := http.DefaultTransport.RoundTrip(request)
			if request.URL.Path == "/v2/bot/message/push" {
				f.setNow(f.now().Add(time.Minute))
			}
			return response, err
		})}))
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 503, Header: http.Header{"Retry-After": {"180"}}})
		receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		require.NotNil(receipt.RetryAfter)
		assert.Equal(initial.Add(4*time.Minute), *receipt.RetryAfter)
		f.setNow(initial.Add(2 * time.Minute))
		_, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
	})
	t.Run("first push", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newReplyFixture(t, "user", nil)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"180"}}})
		rejected, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(store.LineReplyAdmitted, rejected.Status)
		assert.False(rejected.Accepted)
		require.NotNil(rejected.RetryAfter)
		assert.Equal(f.now().Add(3*time.Minute), *rejected.RetryAfter)
		attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
		require.NoError(err)
		assert.Equal(store.LineReplyAdmitted, attempt.State)
		assert.Equal(*rejected.RetryAfter, attempt.NextRetryAt)
		assert.Zero(f.fake.AcceptedPushes())

		f.setNow(rejected.RetryAfter.Add(-time.Second))
		waiting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(rejected, waiting)
		assert.Equal(1, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))

		f.resetClient()
		f.setNow(f.now().Add(22 * time.Hour))
		sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal(store.LineReplySent, sent.Status)
		assert.Equal(1, f.fake.AcceptedPushes())
		completed, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
		require.NoError(err)
		assert.Equal(attempt.RetryKey, completed.RetryKey)
		assert.Equal(attempt.RetryUntil, completed.RetryUntil)
		var keys []string
		for _, request := range f.fake.Requests() {
			if request.Path == "/v2/bot/message/push" {
				keys = append(keys, request.Header.Get("X-Line-Retry-Key"))
			}
		}
		require.Len(keys, 2)
		assert.Equal(attempt.RetryKey, keys[0])
		assert.Equal(attempt.RetryKey, keys[1])
	})
	t.Run("admission deadline after refusal", func(t *testing.T) {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				f := newReplyFixture(t, "user", nil)
				admittedAt := f.now()
				f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Status: status})
				refused, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				if status == http.StatusForbidden {
					must.ErrorIs(err, line.ErrCredentialsUnavailable)
				} else {
					must.NoError(err)
				}
				checks.Equal(store.LineReplyAdmitted, refused.Status)
				checks.Zero(f.fake.AcceptedPushes())

				f.setNow(admittedAt.Add(22 * time.Hour))
				f.resetClient()
				f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
				uncertain, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				checks.Equal(store.LineReplyRemoteUnknown, uncertain.Status)
				checks.Equal(1, f.fake.AcceptedPushes())

				f.setNow(admittedAt.Add(25 * time.Hour))
				expired, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				checks.Equal(store.LineReplyExpired, expired.Status)
				checks.Equal(1, f.fake.AcceptedPushes(), "expiry must prevent a second accepted message")
				checks.Equal(2, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
				attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
				must.NoError(err)
				checks.Equal(admittedAt.Add(23*time.Hour), attempt.RetryUntil)
			})
		}
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
				assert.Equal("sent", result.receipt.Status)
			}
		}
		receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal("sent", receipt.Status)
		assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
		assert.Equal(1, f.fake.AcceptedPushes())
	})
	// SQLite authorization pauses the draft read before it can see the row.
	// A second store connection completes the send during that window.
	t.Run("completion between receipt and draft lookup", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		path := filepath.Join(t.TempDir(), "reply.db")
		f := newReplyFixture(t, "user", testutil.NewSQLiteTestStoreAt(t, path))
		other, err := store.Open(path)
		require.NoError(err)
		t.Cleanup(func() { _ = other.Close() })
		f.st.DB().SetMaxOpenConns(1)
		arrived, resume := make(chan struct{}), make(chan struct{})
		var pause, release sync.Once
		defer release.Do(func() { close(resume) })
		conn, err := f.st.DB().Conn(t.Context())
		require.NoError(err)
		err = conn.Raw(func(raw any) error {
			sqliteConn, ok := raw.(*sqlite3.SQLiteConn)
			require.True(ok)
			sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
				if action == sqlite3.SQLITE_READ && table == "chat_drafts" {
					pause.Do(func() { close(arrived); <-resume })
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		})
		closeErr := conn.Close()
		require.NoError(err)
		require.NoError(closeErr)
		type result struct {
			receipt line.ReplyReceipt
			err     error
		}
		completed := make(chan result, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			completed <- result{receipt, err}
		}()
		defer func() {
			release.Do(func() { close(resume) })
			<-finished
		}()
		select {
		case <-arrived:
		case <-time.After(30 * time.Second):
			require.FailNow("second sender did not reach draft lookup")
		}
		firstSender := f.sender()
		firstSender.Store = other
		first, err := firstSender.Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		release.Do(func() { close(resume) })
		second := <-completed
		require.NoError(second.err)
		assert.Equal(first, second.receipt)
		assert.Equal(1, f.fake.AcceptedPushes())
	})
}

func TestReplyAcceptanceSurvivesCallerCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(f.now), line.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "receipt.db")
	f := newReplyFixture(t, "user", testutil.NewSQLiteTestStoreAt(t, path))
	f.client = line.NewClient("2000000001", "synthetic-secret", line.WithBaseURLs(f.http.URL, f.http.URL), line.WithClock(f.now), line.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil && request.URL.Path == "/v2/bot/message/push" {
			response.Body = replyClosingBody{ReadCloser: response.Body, closed: func() { require.NoError(f.st.Close()) }}
		}
		return response, err
	})}))
	receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.Error(err)
	assert.Equal("remote_unknown", receipt.Status)
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
	f.setNow(f.now().Add(31 * time.Second))
	sender := f.sender()
	sent, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
}

func TestReplyExpiryCommitFailureKeepsPendingState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", testutil.NewTestStore(t))
	_, err := f.st.AdmitLineReplyContext(t.Context(), f.draft.DraftID, 1, f.now(), func(store.LineReplyAttempt) error { return nil })
	require.NoError(err)
	_, err = f.st.StartLineReplyRequestContext(t.Context(), f.draft.DraftID, f.now(), f.now().Add(30*time.Second))
	require.NoError(err)
	_, err = f.st.DB().Exec(`CREATE TABLE line_reply_expiry_guard (source_id BIGINT REFERENCES sources(id) DEFERRABLE INITIALLY DEFERRED)`)
	require.NoError(err)
	dropTrigger := `DROP TRIGGER fail_line_reply_expiry`
	if f.st.IsPostgreSQL() {
		_, err = f.st.DB().Exec(`CREATE FUNCTION fail_line_reply_expiry() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN INSERT INTO line_reply_expiry_guard(source_id) VALUES(-1); RETURN NEW; END; $$`)
		require.NoError(err)
		_, err = f.st.DB().Exec(`CREATE TRIGGER fail_line_reply_expiry AFTER UPDATE ON line_reply_attempts FOR EACH ROW WHEN (NEW.state = 'expired') EXECUTE FUNCTION fail_line_reply_expiry()`)
		dropTrigger += ` ON line_reply_attempts`
	} else {
		_, err = f.st.DB().Exec(`CREATE TRIGGER fail_line_reply_expiry AFTER UPDATE ON line_reply_attempts WHEN NEW.state = 'expired' BEGIN INSERT INTO line_reply_expiry_guard(source_id) VALUES(-1); END`)
	}
	require.NoError(err)
	f.setNow(f.now().Add(23 * time.Hour))
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
	assert.Equal(store.LineReplyExpired, expired.Status)
	assert.Equal(0, f.fake.Count("POST", "/v2/bot/message/push"))
}

func TestReplyConflictingReceiptKeepsDurableIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 200, Body: `{}`})
	unresolved, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("remote_unknown", unresolved.Status)
	assert.True(unresolved.Accepted)
	f.setNow(f.now().Add(31 * time.Second))
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 409, Header: http.Header{"X-Line-Accepted-Request-Id": {"synthetic-conflict-0"}}, Body: `{"sentMessages":[{"id":"` + f.draft.DraftID + `-0"}]}`})
	conflicting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.ErrorIs(err, store.ErrLineReplyReceiptConflict)
	assert.Equal("remote_unknown", conflicting.Status)
	assert.True(conflicting.Accepted)
	assert.Empty(conflicting.RemoteMessageID)
	assert.Equal(unresolved.RequestID, conflicting.RequestID)
	f.setNow(f.now().Add(31 * time.Second))
	sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
}

func TestReplyTokenFailuresHonorRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		response       lineapi.Response
		wait           time.Duration
		recovery       bool
		checkSentAt    bool
	}{
		{"initial/Bad Request", "/oauth2/v3/token", lineapi.Response{Status: 400, Header: http.Header{"Retry-After": {"180"}}}, 30 * time.Second, false, true},
		{"initial/Too Many Requests", "/oauth2/v3/token", lineapi.Response{Status: 429, Header: http.Header{"Retry-After": {"180"}}}, 3 * time.Minute, false, true},
		{"initial/Service Unavailable", "/oauth2/v3/token", lineapi.Response{Status: 503, Header: http.Header{"Retry-After": {"180"}}}, 3 * time.Minute, false, true},
		{"recovery/Bad Request", "/oauth2/v3/token", lineapi.Response{Status: 400, Header: http.Header{"Retry-After": {"180"}}}, 30 * time.Second, true, false},
		{"recovery/Too Many Requests", "/oauth2/v3/token", lineapi.Response{Status: 429, Header: http.Header{"Retry-After": {"180"}}}, 3 * time.Minute, true, false},
		{"recovery/Service Unavailable", "/oauth2/v3/token", lineapi.Response{Status: 503, Header: http.Header{"Retry-After": {"180"}}}, 3 * time.Minute, true, false},
		{"credentials/token 401", "/oauth2/v3/token", lineapi.Response{Status: 401}, 30 * time.Second, false, false},
		{"credentials/invalid token", "/oauth2/v3/token", lineapi.Response{Status: 200, Body: `{}`}, 30 * time.Second, false, false},
		{"credentials/token disconnect", "/oauth2/v3/token", lineapi.Response{Disconnect: true}, 30 * time.Second, false, false},
		{"credentials/push 401", "/v2/bot/message/push", lineapi.Response{Status: 401}, 30 * time.Second, false, false},
		{"credentials/push 403", "/v2/bot/message/push", lineapi.Response{Status: 403}, 30 * time.Second, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newReplyFixture(t, "user", nil)
			state := store.LineReplyAdmitted
			if tc.recovery {
				f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
				_, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				f.setNow(f.now().Add(31 * time.Second))
				state = store.LineReplyRemoteUnknown
			}
			f.resetClient()
			before := f.fake.Count(http.MethodPost, "/oauth2/v3/token")
			responses := []lineapi.Response{tc.response}
			if tc.endpoint == "/v2/bot/message/push" && tc.response.Status == http.StatusUnauthorized {
				responses = append(responses, tc.response)
			}
			f.fake.Enqueue(http.MethodPost, tc.endpoint, responses...)
			receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			must.ErrorIs(err, line.ErrCredentialsUnavailable)
			checks.Equal(state, receipt.Status)
			must.NotNil(receipt.RetryAfter)
			checks.Equal(f.now().Add(tc.wait), *receipt.RetryAfter)
			attempt, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
			must.NoError(err)
			checks.Equal(state, attempt.State)
			checks.Equal(*receipt.RetryAfter, attempt.NextRetryAt)
			if !tc.recovery {
				checks.Zero(attempt.StatusCode, "credential errors are not submission outcomes")
			}
			pushes := f.fake.Count(http.MethodPost, "/v2/bot/message/push")
			if tc.endpoint == "/oauth2/v3/token" && !tc.recovery {
				checks.Zero(pushes)
			}
			waiting, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			must.NoError(err)
			checks.Equal(receipt, waiting)
			f.setNow(f.now().Add(tc.wait - time.Second))
			_, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
			must.NoError(err)
			if tc.endpoint == "/oauth2/v3/token" {
				checks.Equal(before+1, f.fake.Count(http.MethodPost, "/oauth2/v3/token"))
			}
			checks.Equal(pushes, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
			f.setNow(f.now().Add(time.Second))
			sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			must.NoError(err)
			checks.Equal(store.LineReplySent, sent.Status)
			checks.Equal(1, f.fake.AcceptedPushes())
			completed, err := f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
			must.NoError(err)
			checks.Equal(attempt.RetryKey, completed.RetryKey)
			if tc.checkSentAt {
				checks.Equal(attempt.RetryUntil, completed.RetryUntil)
				message, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessage(t.Context(), sent.MessageID)
				must.NoError(err)
				checks.Equal(f.now(), message.SentAt.UTC())
			} else if tc.recovery {
				checks.Equal(attempt.RetryUntil, completed.RetryUntil)
			}
			var keys []string
			for _, request := range f.fake.Requests() {
				if request.Path == "/v2/bot/message/push" {
					keys = append(keys, request.Header.Get("X-Line-Retry-Key"))
				}
			}
			must.Len(keys, pushes+1)
			for _, key := range keys {
				checks.Equal(attempt.RetryKey, key)
			}
		})
	}
}

func TestReplyFromIngestedChat(t *testing.T) {
	for _, kind := range []string{"user", "group", "room"} {
		t.Run(kind, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newReplyFixture(t, kind, nil)
			if kind != "user" {
				must.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1, f.now()))
			}
			// Receiving starts after the bot joined, so there is no join event.
			event := line.Event{Type: "message", Timestamp: f.now().UnixMilli(), WebhookEventID: "incoming-group", Source: line.Source{Type: kind, UserID: replyUser}, Message: &line.Message{ID: "101", Type: "text", Text: "Incoming reply target"}}
			switch kind {
			case "group":
				event.Source.GroupID = f.to
			case "room":
				event.Source.RoomID = f.to
			case "user":
				event.WebhookEventID = "incoming-direct"
			}
			raw, err := json.Marshal(event)
			must.NoError(err)
			must.NoError(f.st.EnqueueLineEventsContext(t.Context(), f.draft.SourceID, "2000000001", []store.LineWebhookEvent{{WebhookEventID: event.WebhookEventID, EventType: event.Type, TargetMessageID: event.Message.ID, EventTimestamp: event.Timestamp, ReceivedAt: f.now(), RawJSON: string(raw)}}))
			processor := line.NewProcessor(f.st, f.client, f.draft.SourceID, "2000000001", t.TempDir(), line.WithProcessorClock(f.now))
			must.NoError(processor.Process(t.Context()))
			var count int
			if kind != "user" {
				must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT participant_count FROM conversations WHERE id=?`), f.draft.ConversationID).Scan(&count))
				checks.Equal(1, count)
			}
			parent, err := f.st.LineMessageContext(t.Context(), f.draft.SourceID, "101")
			must.NoError(err)
			body := "Group response"
			if kind == "user" {
				body = "Direct response"
			}
			draft, err := f.st.CreateChatDraftContext(t.Context(), f.draft.ConversationID, parent.ID, body, func(string, string) error { return nil })
			must.NoError(err)
			receipt, err := f.sender().Send(t.Context(), draft.DraftID, 1)
			must.NoError(err)
			if kind == "user" {
				message, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessage(t.Context(), receipt.MessageID)
				must.NoError(err)
				must.Len(message.To, 1)
				checks.Equal("Example User", message.To[0].Name)
				return
			}
			must.Equal(store.LineReplySent, receipt.Status)
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT participant_count FROM conversations WHERE id=?`), draft.ConversationID).Scan(&count))
			checks.Equal(2, count)
			var active bool
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT cp.left_at IS NULL FROM conversation_participants cp JOIN participant_identifiers i ON i.participant_id=cp.participant_id WHERE cp.conversation_id=? AND i.identifier_type='line_user_id' AND i.identifier_value=?`), draft.ConversationID, "channel:2000000001:user:U00000000000000000000000000000001").Scan(&active))
			checks.True(active)
		})
	}
}

func TestReplyPendingAttemptRechecksPolicy(t *testing.T) {
	for _, state := range []string{store.LineReplyAdmitted, store.LineReplyRemoteUnknown} {
		t.Run(state, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newReplyFixture(t, "user", nil)
			f.resetClient()
			if state == store.LineReplyAdmitted {
				f.fake.Enqueue(http.MethodPost, "/oauth2/v3/token", lineapi.Response{Status: http.StatusInternalServerError})
			} else {
				f.fake.Enqueue(http.MethodPost, "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
			}
			sender := f.sender()
			receipt, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			if state == store.LineReplyAdmitted {
				must.ErrorIs(err, line.ErrCredentialsUnavailable)
				checks.Zero(f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
			} else {
				must.NoError(err)
				must.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1, f.now()))
			}
			must.Equal(state, receipt.Status)
			pushes := f.fake.Count(http.MethodPost, "/v2/bot/message/push")
			sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) {
				return nil, errors.New("synthetic revoked sending policy")
			}
			must.NotNil(receipt.RetryAfter)
			f.setNow(receipt.RetryAfter.Add(time.Second))
			pending, err := sender.Send(t.Context(), f.draft.DraftID, 1)
			must.ErrorContains(err, "synthetic revoked sending policy")
			checks.Equal(state, pending.Status)
			checks.Equal(pushes, f.fake.Count(http.MethodPost, "/v2/bot/message/push"))
			if state == store.LineReplyRemoteUnknown {
				sender.Client = f.sender().Client
				must.NotNil(pending.RetryAfter)
				f.setNow(pending.RetryAfter.Add(time.Second))
				restored, err := sender.Send(t.Context(), f.draft.DraftID, 1)
				must.NoError(err)
				checks.Equal(store.LineReplySent, restored.Status)
			}
		})
	}
}
