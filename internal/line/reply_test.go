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
			_, err = f.st.GetChatDraftContext(t.Context(), f.draft.DraftID)
			require.ErrorIs(err, store.ErrChatDraftNotFound)
			repeated, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
			require.NoError(err)
			assert.Equal(receipt, repeated)
			assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
			assert.Equal(1, f.fake.AcceptedPushes())
			var fromMe bool
			var parent, chat int64
			require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT is_from_me,reply_to_message_id,conversation_id FROM messages WHERE id=?`), receipt.MessageID).Scan(&fromMe, &parent, &chat))
			assert.True(fromMe)
			assert.Equal(f.parent, parent)
			assert.Equal(f.draft.ConversationID, chat)
			body, err := f.st.GetMessageBodyText(receipt.MessageID)
			require.NoError(err)
			assert.Equal("Thanks!", body)
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

func TestReplyAcceptanceCannotRegressOnReplayFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 200, Body: `{}`})
	receipt, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("accepted_unresolved", receipt.Status)
	assert.True(receipt.Accepted)
	for _, status := range []int{503, 400} {
		f.now = f.now.Add(31 * time.Second)
		f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: status, Body: `untrusted provider detail`})
		receipt, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal("accepted_unresolved", receipt.Status)
		assert.True(receipt.Accepted)
	}
	f.now = f.now.Add(31 * time.Second)
	receipt, err = f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", receipt.Status)
	assert.Equal(1, f.fake.AcceptedPushes())
}

func TestReplyAdmissionFailuresDoNotFreeze(t *testing.T) {
	for _, kind := range []string{"invalid text", "disabled policy", "missing client"} {
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
			if kind == "disabled policy" {
				sender.Client = func(context.Context, store.LineReplyAttempt) (*line.Client, error) { return nil, denied }
			}
			if kind == "missing client" {
				sender.Client = nil
			}
			before := len(f.fake.Requests())
			_, err := sender.Send(t.Context(), f.draft.DraftID, f.draft.Revision)
			require.Error(err)
			assert.Len(t, f.fake.Requests(), before)
			_, err = f.st.GetLineReplyAttemptContext(t.Context(), f.draft.DraftID)
			require.ErrorIs(err, sql.ErrNoRows)
			edited, err := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, f.draft.Revision, "editable")
			require.NoError(err)
			require.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, edited.Revision))
		})
	}
}

func TestReplyKnownReceiptRecoveryIgnoresProviderGates(t *testing.T) {
	for _, phase := range []string{"admitted", "possibly submitted", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "reply.db")
			f := newReplyFixture(t, "user", testutil.NewSQLiteTestStoreAt(t, path))
			_, err := f.st.AdmitLineReplyContext(t.Context(), f.draft.DraftID, 1, f.now, func(store.LineReplyAttempt) error { return nil })
			require.NoError(err)
			if phase != "admitted" {
				attempt, err := f.st.StartLineReplyRequestContext(t.Context(), f.draft.DraftID, f.now, f.now.Add(30*time.Second))
				require.NoError(err)
				result, err := f.client.PushText(t.Context(), attempt.RetryKey, line.PushTextRequest{To: f.to, Text: attempt.Body, QuoteToken: attempt.QuoteToken})
				require.NoError(err)
				if phase == "accepted" {
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
			wantPushes := 1
			if phase == "possibly submitted" {
				wantPushes = 2
			}
			assert.Equal(wantPushes, f.fake.Count("POST", "/v2/bot/message/push"))
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
	initial := f.now
	f.now = f.now.Add(23 * time.Hour)
	sender := f.sender()
	sender.Client = nil
	expired, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("remote_unknown_expired", expired.Status)
	for _, now := range []time.Time{initial.Add(25 * time.Hour), initial.Add(time.Minute)} {
		f.now = now
		expired, err = sender.Send(t.Context(), f.draft.DraftID, 1)
		require.NoError(err)
		assert.Equal("remote_unknown_expired", expired.Status)
	}
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
	assert.Equal(1, f.fake.AcceptedPushes())
}

func TestReplyDefiniteRejectionIsTerminal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 400})
	rejected, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("provider_rejected", rejected.Status)
	edited, err := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "new text")
	require.NoError(err)
	require.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, edited.Revision))
	sender := f.sender()
	sender.Client = nil
	repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(rejected, repeated)
	_, err = sender.Send(t.Context(), f.draft.DraftID, edited.Revision)
	require.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
	assert.Zero(f.fake.AcceptedPushes())
}

func TestReplyConcurrentCallsAcceptOnce(t *testing.T) {
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
}

func TestReplyRetryDeadlineUsesResponseTime(t *testing.T) {
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

func TestReplyArchiveFailureRecoversWithoutPush(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", testutil.NewSQLiteTestStore(t))
	_, err := f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_reply_archive BEFORE INSERT ON messages WHEN NEW.is_from_me=1 BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`)
	require.NoError(err)
	pending, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.Error(err)
	assert.Equal("sent_archive_pending", pending.Status)
	assert.True(pending.Accepted)
	assert.Equal("900000000000000001", pending.RemoteMessageID)
	assert.Zero(pending.MessageID)
	_, err = f.st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_reply_archive`)
	require.NoError(err)
	sender := f.sender()
	sender.Client = nil
	sent, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("sent", sent.Status)
	assert.Equal(1, f.fake.Count("POST", "/v2/bot/message/push"))
}

func TestReplyKnownRemoteIDSurvivesReceiptStoreFailure(t *testing.T) {
	for _, recovery := range []string{"same-key retry", "revoked policy", "elapsed cutoff"} {
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
			if recovery == "elapsed cutoff" {
				f.now = f.now.Add(24 * time.Hour)
				pending, err := sender.Send(t.Context(), f.draft.DraftID, 1)
				require.NoError(err)
				assert.Equal(store.LineReplyRemoteUnknownExpired, pending.Status)
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
	f.now = f.now.Add(-23 * time.Hour)
	replay, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(expired, replay)
	assert.Equal(0, f.fake.Count("POST", "/v2/bot/message/push"))
}

func TestReplyTerminalReceiptSurvivesSourceRetirement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	sent, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	target, err := f.st.GetOrCreateSource("line", "2000000002")
	require.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`INSERT INTO source_settings(source_id,history_only,merged_into_source_id,retired_at) VALUES (?,TRUE,?,?)`), f.draft.SourceID, target.ID, f.now)
	require.NoError(err)
	sender := f.sender()
	sender.Client = nil
	repeated, err := sender.Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(sent, repeated)
	assert.Equal(1, f.fake.AcceptedPushes())
}

func TestReplyConflictingReceiptKeepsDurableIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReplyFixture(t, "user", nil)
	f.fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 200, Body: `{}`})
	unresolved, err := f.sender().Send(t.Context(), f.draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("accepted_unresolved", unresolved.Status)
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

// SQL tracing pauses the draft lookup after the receipt lookup has completed.
// Actual Store queries and the fake HTTP API execute both sends.
func TestReplyConcurrentCompletionReturnsReceipt(t *testing.T) {
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
