package x

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type sendFixture struct {
	st                     *store.Store
	source                 *store.Source
	draft                  store.ChatDraft
	policy                 config.XAccountConfig
	manager                *TokenManager
	server                 *httptest.Server
	posts, gets, refreshes atomic.Int32
	post                   func(http.ResponseWriter, *http.Request)
	get                    func(http.ResponseWriter, *http.Request)
}

func newSendFixture(t *testing.T) *sendFixture {
	t.Helper()
	f := &sendFixture{st: testutil.NewTestStore(t)}
	var err error
	f.source, err = f.st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Hour)
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "public parent", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: now}})
	require.NoError(t, err)
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "102")
	require.NoError(t, err)
	f.draft, err = f.st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, "reviewed reply https://example.com", func(string, string) error { return nil })
	require.NoError(t, err)
	generation := int64(1)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	require.NoError(t, err)
	f.policy, err = config.NewDefaultConfig().XAccount("1001")
	require.NoError(t, err)
	f.policy.AllowSend = true
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			f.refreshes.Add(1)
			fmt.Fprint(w, `{"access_token":"rotated-example","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read tweet.write"}`)
			return
		}
		assert.Equal(t, "Bearer example-access", r.Header.Get("Authorization"))
		if r.Method == http.MethodPost {
			f.posts.Add(1)
			assert.Equal(t, "/tweets", r.URL.Path)
			var body struct {
				Text  string `json:"text"`
				Reply struct {
					ID string `json:"in_reply_to_tweet_id"`
				} `json:"reply"`
			}
			require.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Equal(t, f.draft.Body, body.Text)
			assert.Equal(t, "102", body.Reply.ID)
			if f.post != nil {
				f.post(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"data":{"id":"9001"}}`)
			return
		}
		f.gets.Add(1)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "9001", r.URL.Query().Get("ids"))
		if f.get != nil {
			f.get(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":"provider reply","created_at":"2026-10-31T12:00:00Z","referenced_tweets":[{"type":"replied_to","id":"102"}],"edit_history_tweet_ids":["9001"]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]}}`)
	}))
	t.Cleanup(f.server.Close)
	f.manager = NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: f.server.URL + "/token", HTTPClient: f.server.Client()})
	r := NewTokenRecord("1001", "example-client", PublicClient, "example-access", "example-refresh", "")
	r.GrantedScopes = []string{"tweet.read", "users.read", "tweet.write", "offline.access"}
	r.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, f.manager.Save(t.Context(), r))
	return f
}

func (f *sendFixture) sender() *Sender {
	creds := f.manager.Credentials("1001", f.server.Client())
	return NewSender(f.st, NewClient(f.server.URL, creds.HTTPClient()), f.policy, creds)
}
func (f *sendFixture) request() SendRequest {
	return SendRequest{Owner: true, DraftID: f.draft.DraftID, Revision: f.draft.Revision}
}

func sendFailure(t *testing.T, st *store.Store, table, condition string) func() {
	t.Helper()
	require.Contains(t, []string{"message_bodies", "x_reply_attempts"}, table)
	name := "fail_send_" + table
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF " + condition + " THEN RAISE EXCEPTION 'synthetic send persistence failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER " + name + " BEFORE INSERT OR UPDATE ON " + table + " FOR EACH ROW EXECUTE FUNCTION " + name + "()")
		require.NoError(t, err)
	} else {
		_, err := st.DB().Exec("CREATE TRIGGER " + name + " BEFORE INSERT ON " + table + " WHEN " + condition + " BEGIN SELECT RAISE(ABORT,'synthetic send persistence failure'); END")
		if table == "x_reply_attempts" {
			_, err = st.DB().Exec("DROP TRIGGER " + name + "; CREATE TRIGGER " + name + " BEFORE UPDATE ON " + table + " WHEN " + condition + " BEGIN SELECT RAISE(ABORT,'synthetic send persistence failure'); END")
		}
		require.NoError(t, err)
	}
	return func() {
		var err error
		if st.IsPostgreSQL() {
			_, err = st.DB().Exec("DROP TRIGGER " + name + " ON " + table + "; DROP FUNCTION " + name + "()")
		} else {
			_, err = st.DB().Exec("DROP TRIGGER " + name)
		}
		require.NoError(t, err)
	}
}

func reopenSendStore(t *testing.T, st *store.Store) *store.Store {
	t.Helper()
	return openSendStore(t, st, true)
}
func openSendStore(t *testing.T, st *store.Store, closeOld bool) *store.Store {
	t.Helper()
	var path string
	if st.IsPostgreSQL() {
		var database, schema string
		require.NoError(t, st.DB().QueryRow("SELECT current_database(),current_schema()").Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		u.Path = "/" + database
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		path = u.String()
	} else {
		require.NoError(t, st.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path))
	}
	if closeOld {
		require.NoError(t, st.Close())
	}
	peer, err := store.OpenForTest(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, peer.Close()) })
	return peer
}

func TestXSendPostsAtMostOnceAfterLostSuccess(t *testing.T) {
	f := newSendFixture(t)
	undo := sendFailure(t, f.st, "message_bodies", "TRUE")
	result, err := f.sender().Send(t.Context(), f.request())
	require.Error(t, err)
	assert.Equal(t, "sent_archive_pending", result.Status)
	assert.Equal(t, "9001", result.RemoteID)
	a, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "remote_accepted", a.State)
	assert.Equal(t, "9001", a.RemoteID)
	undo()
	f.st = reopenSendStore(t, f.st)
	f.policy.AllowSend = false
	require.NoError(t, os.Remove(f.manager.tokenPath("1001")))
	prepared, err := f.sender().PrepareReply(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "repair", prepared.Mode)
	result, err = f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "sent", result.Status)
	assert.Equal(t, "9001", result.RemoteID)
	_, err = f.st.GetChatDraftContext(t.Context(), f.draft.DraftID)
	require.ErrorIs(t, err, store.ErrChatDraftNotFound)
	prepared, err = f.sender().PrepareReply(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "receipt", prepared.Mode)
	assert.Empty(t, prepared.Text)
	result, err = f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "9001", result.RemoteID)
	assert.Equal(t, int32(1), f.posts.Load())
	assert.Zero(t, f.gets.Load())
	assert.Zero(t, f.refreshes.Load())
}

func TestXSendUnknownRemainsFenced(t *testing.T) {
	for _, mode := range []string{"transport", "server_error", "malformed_success", "malformed_rejection", "redirect", "cancelled", "admitted_crash"} {
		t.Run(mode, func(t *testing.T) {
			f := newSendFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.post = func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "transport":
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				case "server_error":
					w.WriteHeader(503)
					fmt.Fprint(w, `{"title":"Unavailable"}`)
				case "malformed_success":
					w.WriteHeader(201)
					fmt.Fprint(w, `{"data":{}}`)
				case "malformed_rejection":
					w.WriteHeader(403)
					fmt.Fprint(w, `not-json`)
				case "redirect":
					w.Header().Set("Location", f.server.URL+"/again")
					w.WriteHeader(307)
				case "cancelled":
					cancel()
					<-r.Context().Done()
				}
			}
			if mode == "admitted_crash" {
				_, err := f.st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{SourceID: f.source.ID, OwnerID: "1001", DraftID: f.draft.DraftID, Revision: 1, TargetPostID: "102", Cost: store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: 5_000_000, CreateReservationMicros: 200_000, At: time.Now()}})
				require.NoError(t, err)
			} else {
				result, err := f.sender().Send(ctx, f.request())
				require.Error(t, err)
				assert.Equal(t, "remote_unknown", result.Status)
			}
			before := f.posts.Load()
			f.st = reopenSendStore(t, f.st)
			result, err := f.sender().Send(t.Context(), f.request())
			require.NoError(t, err)
			assert.Equal(t, "remote_unknown", result.Status)
			assert.Equal(t, before, f.posts.Load())
			assert.LessOrEqual(t, before, int32(1))
			_, err = f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "different")
			require.ErrorIs(t, err, store.ErrXReplyFrozen)
			state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			require.NoError(t, err)
			assert.Equal(t, int64(200_000), state.SpendBuckets[0].ReservedMicros)
		})
	}
}

func TestXSendDefiniteRejectionAllowsOnlyFreshExplicitAttempt(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 422, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newSendFixture(t)
			f.post = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"title":"Synthetic rejection"}`)
			}
			first, err := f.sender().Send(t.Context(), f.request())
			require.Error(t, err)
			assert.Equal(t, "rejected", first.Status)
			if status == 403 {
				assert.Equal(t, "provider_rejected", first.Code)
			}
			assert.Equal(t, int32(1), f.posts.Load())
			assert.Zero(t, f.refreshes.Load())
			receipt, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
			require.NoError(t, err)
			assert.Equal(t, "rejected", receipt.State)
			_, err = f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, f.draft.Body)
			require.NoError(t, err)
			// No implicit retry after rejection. A new owner invocation may retry
			// under a fresh attempt after the owner has repaired eligibility.
			state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			require.NoError(t, err)
			if state.CredentialError != "" {
				generation := state.Generation + 1
				_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: ptrEmptySendString()})
				require.NoError(t, err)
				r, err := f.manager.Load(t.Context(), "1001")
				require.NoError(t, err)
				require.NoError(t, f.manager.Save(t.Context(), r))
			}
			f.post = nil
			req := f.request()
			req.Revision = 2
			second, err := f.sender().Send(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, "sent", second.Status)
			assert.NotEqual(t, first.AttemptID, second.AttemptID)
			assert.Equal(t, int32(2), f.posts.Load())
		})
	}
}

func TestXSendOwnerAndEligibilityBeforeAdmission(t *testing.T) {
	for _, mode := range []string{"owner", "opt_out", "revision", "scope", "owner_mismatch", "budget", "target"} {
		t.Run(mode, func(t *testing.T) {
			f := newSendFixture(t)
			req := f.request()
			switch mode {
			case "owner":
				req.Owner = false
			case "opt_out":
				f.policy.AllowSend = false
			case "revision":
				req.Revision++
			case "scope", "owner_mismatch":
				r, err := f.manager.Load(t.Context(), "1001")
				require.NoError(t, err)
				if mode == "scope" {
					r.GrantedScopes = []string{"tweet.read", "users.read"}
				} else {
					f.policy.UserID = "2001"
				}
				require.NoError(t, f.manager.Save(t.Context(), r))
			case "budget":
				f.policy.MonthlyBudgetMicros = 199_999
			case "target":
				require.NoError(t, f.st.MarkXPostsUnverifiedContext(t.Context(), f.source.ID, []string{"102"}))
			}
			_, err := f.sender().Send(t.Context(), req)
			require.Error(t, err)
			assert.Zero(t, f.posts.Load())
			assert.Zero(t, f.gets.Load())
			state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			require.NoError(t, err)
			assert.Empty(t, state.Reservations)
		})
	}
	// Receipt existence and its native IDs remain owner-only too.
	f := newSendFixture(t)
	_, err := f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	req := f.request()
	req.Owner = false
	result, err := f.sender().Send(t.Context(), req)
	require.Error(t, err)
	assert.Empty(t, result.RemoteID)
	p, err := f.sender().PrepareReply(t.Context(), req)
	require.Error(t, err)
	assert.Empty(t, p.DraftID)
}

func TestXReplyPreparationIsNetworkFreeReview(t *testing.T) {
	f := newSendFixture(t)
	p, err := f.sender().PrepareReply(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "new_send", p.Mode)
	assert.Equal(t, f.draft.Body, p.Text)
	assert.Equal(t, "102", p.TargetPostID)
	assert.Equal(t, int64(200_000), p.ReservedMicros)
	assert.Zero(t, f.posts.Load())
	assert.Zero(t, f.gets.Load())
	assert.Zero(t, f.refreshes.Load())
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	assert.Empty(t, state.Reservations)
	encoded, err := json.Marshal(p)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "example-access")
}

func TestXSendConcurrentAdmissionHasOnePoster(t *testing.T) {
	f := newSendFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNetwork := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNetwork()
	f.post = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(201)
		fmt.Fprint(w, `{"data":{"id":"9001"}}`)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.sender().Send(ctx, f.request()); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	_, err := f.sender().Send(ctx, f.request())
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	assert.Equal(t, int32(1), f.posts.Load())
	// Source lifecycle consumers use exactly the same execution fence.
	_, err = f.st.AcquireSyncExecutionContext(ctx, f.source.ID)
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	_, err = f.st.AcquireSourceRemovalLockContext(ctx, f.source.ID)
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	// Return from the network before checking cleanup; no cleanup waits on an
	// expected barrier after an assertion fails.
	releaseNetwork()
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	_, err = f.sender().Send(ctx, f.request())
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.posts.Load())
}

func TestXSendResultContainsNoTextOrProviderError(t *testing.T) {
	f := newSendFixture(t)
	f.post = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"title":"example-access private provider text"}`)
	}
	result, err := f.sender().Send(t.Context(), f.request())
	require.Error(t, err)
	encoded, encodeErr := json.Marshal(result)
	require.NoError(t, encodeErr)
	for _, s := range []string{f.draft.Body, "example-access", "private provider text"} {
		assert.NotContains(t, string(encoded)+err.Error(), s)
	}
	assert.False(t, strings.Contains(err.Error(), "summon"))
}

func ptrEmptySendString() *string { s := ""; return &s }
