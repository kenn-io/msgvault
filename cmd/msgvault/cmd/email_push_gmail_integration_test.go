package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailpush"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	emailfixture "go.kenn.io/msgvault/internal/testutil/email"
)

const gmailReplayAccount = "reader@example.com"

type gmailReplayProvider struct {
	mu            sync.Mutex
	history       uint64
	address       string
	profileStatus int
	failFetch     bool
	delivered     bool
	generation    int
	raw           []byte
	ackIDs        []string
	acked         chan struct{}
	watched       chan struct{}
	pullBlocked   chan struct{}
	pullExited    chan struct{}
	requests      []string
}

func newGmailReplayProvider(t *testing.T) *gmailReplayProvider {
	t.Helper()
	p := &gmailReplayProvider{history: 1000, address: gmailReplayAccount, failFetch: true, generation: 1, acked: make(chan struct{}, 8), watched: make(chan struct{}, 8), pullBlocked: make(chan struct{}, 8), pullExited: make(chan struct{}, 8), raw: emailfixture.NewMessage().From("sender@example.com").To(gmailReplayAccount).Header("Message-ID", "<late-message@example.com>").Body("Synthetic delayed acquisition").Bytes()}
	originalClient, originalTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient = &http.Client{Transport: p}
	http.DefaultTransport = p
	t.Cleanup(func() { http.DefaultClient = originalClient; http.DefaultTransport = originalTransport })
	return p
}
func (p *gmailReplayProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer func() { _ = r.Body.Close() }()
	}
	p.mu.Lock()
	p.requests = append(p.requests, r.URL.Host+r.URL.Path)
	switch r.URL.Host {
	case "oauth2.googleapis.com":
		p.mu.Unlock()
		return gmailReplayResponse(r, http.StatusOK, map[string]any{"access_token": "synthetic-subscriber-access", "token_type": "Bearer", "expires_in": 3600})
	case "pubsub.googleapis.com":
		switch {
		case r.Method == http.MethodGet:
			p.mu.Unlock()
			return gmailReplayResponse(r, http.StatusOK, map[string]any{"name": "projects/example/subscriptions/mail", "topic": "projects/example/topics/mail", "state": "ACTIVE", "pushConfig": map[string]any{}})
		case strings.HasSuffix(r.URL.Path, ":pull"):
			if p.delivered {
				p.mu.Unlock()
				select {
				case p.pullBlocked <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				select {
				case p.pullExited <- struct{}{}:
				default:
				}
				return nil, r.Context().Err()
			}
			p.delivered = true
			p.history = 1001
			generation := p.generation
			p.mu.Unlock()
			// This hint and the watch result deliberately differ from the durable cursor.
			data, err := json.Marshal(map[string]string{"emailAddress": gmailReplayAccount, "historyId": "77777"})
			if err != nil {
				return nil, err
			}
			return gmailReplayResponse(r, http.StatusOK, map[string]any{"receivedMessages": []any{map[string]any{"ackId": fmt.Sprintf("delivery-%d", generation), "message": map[string]string{"data": base64.StdEncoding.EncodeToString(data)}}}})
		case strings.HasSuffix(r.URL.Path, ":acknowledge"):
			p.mu.Unlock()
			var body struct {
				IDs []string `json:"ackIds"`
			}
			if err := json.UnmarshalRead(r.Body, &body); err != nil {
				return nil, err
			}
			p.mu.Lock()
			p.ackIDs = append(p.ackIDs, body.IDs...)
			p.mu.Unlock()
			select {
			case p.acked <- struct{}{}:
			default:
			}
			return gmailReplayResponse(r, http.StatusOK, map[string]any{})
		}
	case "gmail.googleapis.com":
		history, address, status, failFetch := p.history, p.address, p.profileStatus, p.failFetch
		raw := p.raw
		p.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			if status != 0 {
				return gmailReplayResponse(r, status, map[string]any{"error": map[string]any{"code": status, "message": "synthetic-private-profile-marker"}})
			}
			return gmailReplayResponse(r, http.StatusOK, map[string]any{"emailAddress": address, "historyId": strconv.FormatUint(history, 10), "messagesTotal": 1})
		case strings.HasSuffix(r.URL.Path, "/watch"):
			select {
			case p.watched <- struct{}{}:
			default:
			}
			return gmailReplayResponse(r, http.StatusOK, map[string]string{"historyId": "99999", "expiration": strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)})
		case strings.HasSuffix(r.URL.Path, "/labels"):
			return gmailReplayResponse(r, http.StatusOK, map[string]any{"labels": []any{map[string]string{"id": "INBOX", "name": "INBOX", "type": "system"}}})
		case strings.HasSuffix(r.URL.Path, "/history"):
			return gmailReplayResponse(r, http.StatusOK, map[string]any{"historyId": strconv.FormatUint(history, 10), "history": []any{map[string]any{"id": "1001", "messagesAdded": []any{map[string]any{"message": map[string]string{"id": "late-message", "threadId": "thread-late"}}}}}})
		case strings.HasSuffix(r.URL.Path, "/messages/late-message"):
			if failFetch {
				return gmailReplayResponse(r, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "message": "synthetic temporary acquisition failure"}})
			}
			return gmailReplayResponse(r, http.StatusOK, map[string]any{"id": "late-message", "threadId": "thread-late", "historyId": strconv.FormatUint(history, 10), "internalDate": "1700000000000", "labelIds": []string{"INBOX"}, "raw": base64.RawURLEncoding.EncodeToString(raw)})
		}
		return nil, fmt.Errorf("unexpected synthetic Gmail endpoint %s", r.URL.Path)
	}
	p.mu.Unlock()
	return nil, fmt.Errorf("unexpected synthetic request %s %s", r.Method, r.URL.Host+r.URL.Path)
}
func gmailReplayResponse(r *http.Request, status int, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
}

type gmailReplayResult struct {
	errors, added int64
	err           error
}
type gmailReplaySession struct {
	t         *testing.T
	cancel    context.CancelFunc
	done      chan error
	failed    chan error
	summaries chan gmailReplayResult
	sched     *scheduler.Scheduler
	client    io.Closer
	once      sync.Once
}

type gmailReplayOAuthFactory func(context.Context, string) (*oauth.Manager, error)

func startGmailReplaySession(t *testing.T, st *store.Store, sourceID int64, manager *oauth.Manager, state *invocation, afterReceipt ...func(context.Context) error) *gmailReplaySession {
	t.Helper()
	requirements := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	session := &gmailReplaySession{t: t, cancel: cancel, done: make(chan error, 1), failed: make(chan error, 16), summaries: make(chan gmailReplayResult, 32)}
	source, token, err := st.BindEmailPushSource(ctx, sourceID)
	requirements.NoError(err)
	bindings := map[string]emailPushBinding{source.Identifier: {sourceID: source.ID, sourceType: source.SourceType, identifier: source.Identifier, token: token}}
	var getManager gmailReplayOAuthFactory = func(context.Context, string) (*oauth.Manager, error) { return manager, nil }
	session.sched = scheduler.New(func(context.Context, string) error { return errors.New("ordinary selector must not run") })
	requirements.NoError(session.sched.AddAccount(source.Identifier, "0 0 * * *"))
	session.sched.WithAccountWakeSyncFunc(emailPushWakeSync(bindings, st, func(ctx context.Context, selected *store.Source) (*gmail.SyncSummary, error) {
		summary, err := runEmailPushSource(ctx, selected, st, state, getManager)
		observed := gmailReplayResult{err: err}
		if summary != nil {
			observed.errors = summary.Errors
			observed.added = summary.MessagesAdded
		}
		select {
		case session.summaries <- observed:
		case <-ctx.Done():
		}
		return summary, err
	}))
	entry := config.GmailPushConfig{SourceID: source.ID, Topic: "projects/example/topics/mail", Subscription: "projects/example/subscriptions/mail", CredentialsFile: filepath.Join(state.cfg.HomeDir, "subscriber.json")}
	listener, closer, err := newGmailPushListener(ctx, entry, st, session.sched, state, getManager)
	requirements.NoError(err)
	session.client = closer
	if len(afterReceipt) > 0 {
		original := listener.Reconcile
		listener.Reconcile = func(ctx context.Context) error {
			if err := original(ctx); err != nil {
				return err
			}
			for _, hook := range afterReceipt {
				if err := hook(ctx); err != nil {
					return err
				}
			}
			return nil
		}
	}
	listener.OnError = func(err error) {
		select {
		case session.failed <- err:
		case <-ctx.Done():
		}
	}
	go func() { session.done <- listener.Run(ctx) }()
	t.Cleanup(session.Stop)
	return session
}
func (s *gmailReplaySession) Stop() {
	s.t.Helper()
	s.once.Do(func() {
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(15 * time.Second):
			assert.Fail(s.t, "Gmail listener did not join")
		}
		<-s.sched.Stop().Done()
		require.NoError(s.t, s.client.Close())
	})
}
func awaitGmailReplay[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(15 * time.Second):
		require.FailNow(t, "missing observable Gmail replay transition")
		var zero T
		return zero
	}
}

func TestGmailPushActualIngestionProfileBeforeMutation(t *testing.T) {
	for _, mode := range []string{"foreign", "missing", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			manager, state := setupCommandOAuth(t)
			putCommandToken(t, state, gmailReplayAccount, oauth.ScopesGmailReadonly)
			provider := newGmailReplayProvider(t)
			switch mode {
			case "foreign":
				provider.address = "foreign@example.com"
			case "missing":
				provider.address = ""
			case "unauthorized":
				provider.profileStatus = http.StatusUnauthorized
			}
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", gmailReplayAccount)
			requirements.NoError(err)
			requirements.NoError(st.UpdateSourceSyncCursor(source.ID, "1000"))
			source, err = st.GetSourceByID(source.ID)
			requirements.NoError(err)
			_, err = runEmailPushSource(t.Context(), source, st, state, func(context.Context, string) (*oauth.Manager, error) { return manager, nil })
			requirements.Error(err, "strict authenticated identity must reject before syncer invocation")
			requirements.ErrorIs(err, errEmailPushIncomplete)
			requirements.ErrorContains(err, "source "+strconv.FormatInt(source.ID, 10))
			requirements.ErrorContains(err, "Gmail profile verification failed")
			assertions.NotContains(err.Error(), "synthetic-private-profile-marker")
			assertions.NotContains(err.Error(), "foreign@example.com")
			assertions.NotContains(err.Error(), gmailReplayAccount)
			if mode == "unauthorized" {
				assertions.Contains(err.Error(), "expired or been revoked")
			}
			current, readErr := st.GetSourceByID(source.ID)
			requirements.NoError(readErr)
			assertions.Equal("1000", current.SyncCursor.String)
			var runs int
			requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sync_runs WHERE source_id = ?", source.ID).Scan(&runs))
			assertions.Zero(runs, "preflight identity failure must not create or fail a sync run")
			debt, readErr := st.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			requirements.NoError(readErr)
			assertions.Empty(debt)
			provider.mu.Lock()
			requests := append([]string(nil), provider.requests...)
			provider.mu.Unlock()
			for _, path := range requests {
				assertions.NotContains(path, "/history")
				assertions.NotContains(path, "/messages/")
			}
		})
	}
}

func TestGmailPushActualPartialDebtReopenReplayACK(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	manager, state := setupCommandOAuth(t)
	putCommandToken(t, state, gmailReplayAccount, oauth.ScopesGmailReadonly)
	provider := newGmailReplayProvider(t)
	dbPath := filepath.Join(t.TempDir(), "durable.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", gmailReplayAccount)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncCursor(source.ID, "1000"))
	state.cfg.Accounts = []config.AccountSchedule{{Email: source.Identifier, Schedule: "0 0 * * *", Enabled: true}}
	credentials := []byte(`{"type":"authorized_user","client_id":"synthetic-subscriber","client_secret":"synthetic-secret","refresh_token":"synthetic-refresh"}`)
	requirements.NoError(os.WriteFile(filepath.Join(state.cfg.HomeDir, "subscriber.json"), credentials, 0600))
	first := startGmailReplaySession(t, st, source.ID, manager, state)
	for {
		observed := awaitGmailReplay(t, first.summaries)
		requirements.NoError(observed.err)
		if observed.errors > 0 {
			assertions.Equal(int64(1), observed.errors)
			break
		}
	}
	requirements.Error(awaitGmailReplay(t, first.failed), "nil sync error with Errors1 must fail strict receipt")
	first.Stop()
	provider.mu.Lock()
	firstACKs := append([]string(nil), provider.ackIDs...)
	provider.mu.Unlock()
	assertions.Empty(firstACKs)
	current, err := st.GetSourceByID(source.ID)
	requirements.NoError(err)
	assertions.Equal("1001", current.SyncCursor.String, "only committed provider profile cursor advances")
	debt, err := st.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	requirements.NoError(err)
	requirements.Len(debt, 1)
	assertions.Equal("late-message", debt[0].SourceMessageID)
	present, err := st.MessageExistsBatchContext(t.Context(), source.ID, []string{"late-message"})
	requirements.NoError(err)
	assertions.Empty(present)
	requirements.NoError(st.Close())
	reopened, err := store.Open(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(reopened.Close()) })
	provider.mu.Lock()
	provider.failFetch = false
	provider.delivered = false
	provider.generation = 2
	provider.mu.Unlock()
	second := startGmailReplaySession(t, reopened, source.ID, manager, state)
	recovered := awaitGmailReplay(t, second.summaries)
	requirements.NoError(recovered.err)
	assertions.Zero(recovered.errors)
	assertions.Equal(int64(1), recovered.added, "startup replays debt even with equal history cursor")
	awaitGmailReplay(t, provider.acked)
	second.Stop()
	provider.mu.Lock()
	allACKs := append([]string(nil), provider.ackIDs...)
	provider.mu.Unlock()
	assertions.Equal([]string{"delivery-2"}, allACKs)
	debt, err = reopened.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	requirements.NoError(err)
	assertions.Empty(debt)
	current, err = reopened.GetSourceByID(source.ID)
	requirements.NoError(err)
	assertions.Equal("1001", current.SyncCursor.String)
	present, err = reopened.MessageExistsBatchContext(t.Context(), source.ID, []string{"late-message"})
	requirements.NoError(err)
	requirements.Contains(present, "late-message")
	raw, err := reopened.GetMessageRawContext(t.Context(), present["late-message"])
	requirements.NoError(err)
	assertions.Equal(provider.raw, raw)
	checkpoint, err := reopened.GetLatestCheckpointedSyncContext(t.Context(), source.ID)
	assertions.Nil(checkpoint)
	assertions.ErrorIs(err, store.ErrSyncRunNotFound)
}

func TestGmailPushRetryableBoundSourceRead(t *testing.T) {
	requirements := require.New(t)
	source := &store.Source{ID: 1, SourceType: "gmail", Identifier: "synthetic@example.com"}
	failure := errors.New("synthetic transient Store read")
	listener, err := newBoundGmailPushListener(source, gmailPushTestConfig(), &gmailPushTestClient{profile: &gmail.Profile{EmailAddress: source.Identifier}}, gmailPushTestSubscriber{}, func(context.Context) (*store.Source, error) { return nil, failure }, func(context.Context, string) error { return nil }, false, nil)
	requirements.NoError(err)
	err = listener.ValidateSource(t.Context())
	requirements.ErrorIs(err, failure)
	requirements.NotErrorIs(err, emailpush.ErrSourceUnavailable)
	requirements.False(gmailPushPermanentFailure(err))
}

func TestEmailPushGmailUnionBindings(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-type", "disabled", "alias", "duplicate-subscription"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			st, cfg, imapSource := emailPushFixture(t)
			gmailSource, err := st.GetOrCreateSource("gmail", gmailReplayAccount)
			requirements.NoError(err)
			selectedID := gmailSource.ID
			cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{Email: gmailSource.Identifier, Schedule: "0 0 * * *", Enabled: true})
			switch mode {
			case "wrong-type":
				selectedID = imapSource.ID
			case "disabled":
				cfg.Accounts[1].Enabled = false
			case "alias":
				cfg.Accounts[1].Email = "friendly"
			}
			text := fmt.Sprintf(`[[email_push.gmail]]
source_id = %d
topic = "projects/example/topics/mail"
subscription = "projects/example/subscriptions/mail"
credentials_file = "/synthetic/subscriber.json"
`, selectedID)
			if mode == "duplicate-subscription" {
				other, err := st.GetOrCreateSource("gmail", "other@example.com")
				requirements.NoError(err)
				cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{Email: other.Identifier, Schedule: "0 0 * * *", Enabled: true})
				text += fmt.Sprintf(`[[email_push.gmail]]
source_id = %d
topic = "projects/example/topics/mail"
subscription = "projects/example/subscriptions/mail"
credentials_file = "/synthetic/subscriber.json"
`, other.ID)
			}
			_, err = toml.Decode(text, cfg)
			requirements.NoError(err)
			sched := scheduler.New(func(context.Context, string) error { return errors.New("ordinary dispatcher must not run") }).WithLogger(slog.New(slog.DiscardHandler))
			t.Cleanup(func() { <-sched.Stop().Done() })
			_, scheduleErrors := sched.AddAccountsFromConfig(cfg)
			requirements.Empty(scheduleErrors)
			bindings, err := emailPushBindings(t.Context(), cfg, st, sched)
			if mode != "valid" {
				requirements.Error(err)
				return
			}
			requirements.NoError(err)
			requirements.Len(bindings, 2)
			requirements.Equal(imapSource.ID, bindings[imapSource.Identifier].sourceID)
			requirements.Equal(gmailSource.ID, bindings[gmailSource.Identifier].sourceID)
		})
	}
}

func TestEmailPushGmailRuntimeStartsAndJoinsPull(t *testing.T) {
	requirements := require.New(t)
	manager, state := setupCommandOAuth(t)
	putCommandToken(t, state, gmailReplayAccount, oauth.ScopesGmailReadonly)
	provider := newGmailReplayProvider(t)
	provider.failFetch = false
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", gmailReplayAccount)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncCursor(source.ID, "1000"))
	state.cfg.Accounts = []config.AccountSchedule{{Email: source.Identifier, Enabled: true, Schedule: "0 0 * * *"}}
	state.cfg.Analytics.AutoBuildCache = false
	credentialsPath := filepath.Join(state.cfg.HomeDir, "subscriber.json")
	requirements.NoError(os.WriteFile(credentialsPath, []byte(`{"type":"authorized_user","client_id":"synthetic","client_secret":"synthetic","refresh_token":"synthetic"}`), 0600))
	_, err = toml.Decode(fmt.Sprintf(`[[email_push.gmail]]
source_id = %d
topic = "projects/example/topics/mail"
subscription = "projects/example/subscriptions/mail"
credentials_file = %q
`, source.ID, filepath.ToSlash(credentialsPath)), state.cfg)
	requirements.NoError(err)
	sched := scheduler.New(func(context.Context, string) error { return errors.New("ordinary dispatcher must not run") }).WithLogger(slog.New(slog.DiscardHandler))
	_, scheduleErrors := sched.AddAccountsFromConfig(state.cfg)
	requirements.Empty(scheduleErrors)
	runtime, err := startEmailPush(t.Context(), state.cfg, st, sched, state, func(context.Context, string) (*oauth.Manager, error) { return manager, nil }, nil)
	requirements.NoError(err)
	stop := sync.OnceFunc(func() { runtime.Stop(); <-sched.Stop().Done() })
	t.Cleanup(stop)
	awaitGmailReplay(t, provider.watched)
	awaitGmailReplay(t, provider.acked)
	awaitGmailReplay(t, provider.pullBlocked)
	runtime.Stop()
	awaitGmailReplay(t, provider.pullExited)
	requirements.True(sched.IsScheduled(source.Identifier), "push shutdown preserves registered polling fallback")
	stop()
}

func TestGmailPushFactoryStoreReadFailureIsRetryable(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", gmailReplayAccount)
	requirements.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Accounts = []config.AccountSchedule{{Email: source.Identifier, Schedule: "0 0 * * *", Enabled: true}}
	state := testInvocationWithConfig(cfg)
	sched := scheduler.New(func(context.Context, string) error { return nil }).WithLogger(slog.New(slog.DiscardHandler))
	requirements.NoError(sched.AddAccount(source.Identifier, "0 0 * * *"))
	t.Cleanup(func() { <-sched.Stop().Done() })
	requirements.NoError(st.Close())
	entry := gmailPushTestConfig()
	entry.SourceID = source.ID
	listener, closer, err := newGmailPushListener(t.Context(), entry, st, sched, state, func(context.Context, string) (*oauth.Manager, error) {
		requirements.FailNow("credentials must not be read after Store failure")
		return nil, nil //nolint:nilnil // Unreachable after FailNow; preserve the unexpected-call guard.
	})
	requirements.Error(err)
	requirements.Nil(listener)
	requirements.Nil(closer)
	requirements.NotErrorIs(err, emailpush.ErrSourceUnavailable)
	requirements.False(gmailPushPermanentFailure(err))
}

func TestGmailPushActualSourceIncarnationBeforeACK(t *testing.T) {
	for _, change := range []string{"recreate", "retire"} {
		t.Run(change, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			manager, state := setupCommandOAuth(t)
			putCommandToken(t, state, gmailReplayAccount, oauth.ScopesGmailReadonly)
			provider := newGmailReplayProvider(t)
			provider.failFetch = false
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", gmailReplayAccount)
			requirements.NoError(err)
			requirements.NoError(st.UpdateSourceSyncCursor(source.ID, "1000"))
			state.cfg.Accounts = []config.AccountSchedule{{Email: source.Identifier, Enabled: true, Schedule: "0 0 * * *"}}
			requirements.NoError(os.WriteFile(filepath.Join(state.cfg.HomeDir, "subscriber.json"), []byte(`{"type":"authorized_user","client_id":"synthetic","client_secret":"synthetic","refresh_token":"synthetic"}`), 0600))
			changed := false
			session := startGmailReplaySession(t, st, source.ID, manager, state, func(ctx context.Context) error {
				provider.mu.Lock()
				delivered := provider.delivered
				provider.mu.Unlock()
				if !delivered || changed {
					return nil
				}
				changed = true
				if change == "recreate" {
					if _, _, err := st.RemoveSourceSerialized(ctx, source.ID); err != nil {
						return err
					}
					_, err := st.GetOrCreateSourceContext(ctx, "gmail", source.Identifier)
					return err
				}
				destination, err := st.GetOrCreateSourceContext(ctx, "gmail", "destination@example.com")
				if err != nil {
					return err
				}
				_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: destination.ID})
				return err
			})
			err = awaitGmailReplay(t, session.done)
			// Stop still owns scheduler/client cleanup, including after an assertion failure.
			session.done <- err
			requirements.ErrorIs(err, emailpush.ErrSourceUnavailable, "a new or retired row cannot inherit the completed receipt")
			session.Stop()
			provider.mu.Lock()
			acks := append([]string(nil), provider.ackIDs...)
			provider.mu.Unlock()
			assertions.Empty(acks)
		})
	}
}
