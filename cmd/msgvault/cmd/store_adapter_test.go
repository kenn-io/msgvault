package cmd

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStoreAPIAdapterImplementsCtxMessageStore is a compile-time guard that the
// production adapter wired into api.Server satisfies the optional context-aware
// read interface. If it drifts, api.Server silently falls back to the
// background-context read path.
func TestStoreAPIAdapterImplementsCtxMessageStore(t *testing.T) {
	var adapter api.MessageStore = &storeAPIAdapter{}
	_, ok := adapter.(api.CtxMessageStore)
	require.True(t, ok, "storeAPIAdapter must implement api.CtxMessageStore")
}

func TestStoreAPIAdapterServesAttributeDefinitions(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attribute-definitions", nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

var _ api.CtxMessageStore = (*storeAPIAdapter)(nil)
var _ api.MessageIdentityStore = (*storeAPIAdapter)(nil)
var _ api.MeetingImporter = (*storeAPIAdapter)(nil)

func TestStoreAPIAdapterPassesThroughMessageIdentityOperations(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "thread-identity", "Identity")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "message-identity",
		MessageType:     "email",
	})
	require.NoError(err)
	participantID, err := st.EnsureParticipant("masked-shop@example.test", "Masked", "example.test")
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(
		messageID, "to", []int64{participantID}, []string{"Masked"},
	))
	require.NoError(st.AddAccountIdentity(source.ID, "Masked-Shop@Example.test", "manual"))

	adapter := &storeAPIAdapter{store: st}
	resolved, err := adapter.ResolveAccountIdentityContext(
		t.Context(), source.ID, "masked-shop@example.test",
	)
	require.NoError(err)
	require.Equal("Masked-Shop@Example.test", resolved.Identifier)
	require.Equal([]int64{participantID}, resolved.ParticipantIDs)

	matches, err := adapter.MatchMessageIdentitiesContext(t.Context(), []int64{messageID})
	require.NoError(err)
	require.Equal([]string{"Masked-Shop@Example.test"}, matches[messageID].Recipients)
}

type scopedStatsProductionAdapter struct {
	*storeAPIAdapter

	source   *store.Source
	returned chan error
}

type quickFTSProductionAdapter struct {
	*storeAPIAdapter

	returned chan bool
}

func (a *quickFTSProductionAdapter) NeedsFTSBackfillQuickContext(ctx context.Context) bool {
	needs := a.storeAPIAdapter.NeedsFTSBackfillQuickContext(ctx)
	a.returned <- needs
	return needs
}

func (a *scopedStatsProductionAdapter) GetSourcesByIdentifierOrDisplayName(
	string,
) ([]*store.Source, error) {
	return []*store.Source{a.source}, nil
}

func (a *scopedStatsProductionAdapter) GetSourcesByIdentifierOrDisplayNameContext(
	context.Context,
	string,
) ([]*store.Source, error) {
	return []*store.Source{a.source}, nil
}

func (a *scopedStatsProductionAdapter) GetSourcesByTypeAndAccount(
	string,
	string,
) ([]*store.Source, error) {
	return nil, nil
}

func (a *scopedStatsProductionAdapter) GetSourcesByTypeAndAccountContext(
	context.Context,
	string,
	string,
) ([]*store.Source, error) {
	return nil, nil
}

func (a *scopedStatsProductionAdapter) GetStatsForScopeContext(
	ctx context.Context,
	sourceIDs []int64,
) (*store.Stats, error) {
	stats, err := a.storeAPIAdapter.GetStatsForScopeContext(ctx, sourceIDs)
	a.returned <- err
	return stats, err
}

// TestMarkedCLIScopedStatsCancellationReachesProductionAdapter guards the
// complete marked handler -> optional context extension -> daemon adapter ->
// database path. The wrapper only supplies deterministic scope resolution and
// observes the real production adapter returning; it does not replace the
// scoped statistics query.
func TestMarkedCLIScopedStatsCancellationReachesProductionAdapter(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	conn, err := st.DB().Conn(t.Context())
	require.NoError(err, "hold the only database connection")
	t.Cleanup(func() { _ = conn.Close() })

	returned := make(chan error, 1)
	adapter := &scopedStatsProductionAdapter{
		storeAPIAdapter: &storeAPIAdapter{store: st},
		source: &store.Source{
			ID:         42,
			SourceType: "gmail",
			Identifier: "alice@example.com",
		},
		returned: returned,
	}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIKey: "secret-key",
		}},
		Store:  adapter,
		Logger: slog.New(slog.DiscardHandler),
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/cli/stats?account=alice@example.com",
		nil,
	).WithContext(ctx)
	req.Header.Set("X-Api-Key", "secret-key")
	req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)

	requestDone := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(httptest.NewRecorder(), req)
		close(requestDone)
	}()

	require.Eventually(func() bool {
		return st.DB().Stats().WaitCount > 0
	}, 2*time.Second, 10*time.Millisecond, "scoped statistics waits for the production database")
	cancel()

	select {
	case err := <-returned:
		require.ErrorIs(err, context.Canceled, "production scoped statistics returns request cancellation")
	case <-time.After(2 * time.Second):
		require.FailNow("production scoped statistics continued after marked request cancellation")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		require.FailNow("marked scoped statistics handler did not return after cancellation")
	}
}

// TestMarkedCLISearchCancellationReturnsFromProductionQuickFTSProbe guards the
// foreground request -> context wrapper -> production adapter -> Store ->
// dialect query chain. The held real database connection makes the quick probe
// wait for a connection until the marked request is cancelled.
func TestMarkedCLISearchCancellationReturnsFromProductionQuickFTSProbe(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	require.True(st.FTS5Available(), "tagged test store has FTS5")
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })

	st.DB().SetMaxOpenConns(1)
	conn, err := st.DB().Conn(t.Context())
	require.NoError(err, "hold the only database connection")
	t.Cleanup(func() { _ = conn.Close() })

	returned := make(chan bool, 1)
	adapter := &quickFTSProductionAdapter{
		storeAPIAdapter: &storeAPIAdapter{store: st},
		returned:        returned,
	}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIKey: "secret-key",
		}},
		Store:  adapter,
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/cli/search?q=hello",
		nil,
	).WithContext(ctx)
	req.Header.Set("X-Api-Key", "secret-key")
	req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)

	waitCount := st.DB().Stats().WaitCount
	requestDone := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(httptest.NewRecorder(), req)
		close(requestDone)
	}()

	require.Eventually(func() bool {
		return st.DB().Stats().WaitCount > waitCount
	}, 2*time.Second, 10*time.Millisecond, "quick FTS probe waits for the production database")
	cancel()

	select {
	case <-returned:
	case <-time.After(500 * time.Millisecond):
		require.FailNow("production quick FTS probe continued after marked request cancellation")
	}
	select {
	case <-requestDone:
	case <-time.After(500 * time.Millisecond):
		require.FailNow("marked CLI search did not return after cancellation")
	}
}

func TestMarkedCLIRequestCancellationStopsProductionStoreWork(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "init database",
			method: http.MethodPost,
			path:   "/api/v1/cli/init-db",
		},
		{
			name:   "list accounts",
			method: http.MethodGet,
			path:   "/api/v1/cli/accounts",
		},
		{
			name:   "list collections",
			method: http.MethodGet,
			path:   "/api/v1/cli/collections",
		},
		{
			name:   "rebuild FTS",
			method: http.MethodPost,
			path:   "/api/v1/cli/rebuild-fts",
		},
		{
			name:   "update account",
			method: http.MethodPost,
			path:   "/api/v1/cli/account",
			body:   `{"email":"alice@example.com","display_name":"Alice"}`,
		},
		{
			name:   "create collection",
			method: http.MethodPost,
			path:   "/api/v1/cli/collections",
			body:   `{"name":"Work","accounts":["alice@example.com"]}`,
		},
		{
			name:   "add collection sources",
			method: http.MethodPatch,
			path:   "/api/v1/cli/collections/Work/sources",
			body:   `{"accounts":["alice@example.com"]}`,
		},
		{
			name:   "remove collection sources",
			method: http.MethodDelete,
			path:   "/api/v1/cli/collections/Work/sources",
			body:   `{"accounts":["alice@example.com"]}`,
		},
		{
			name:   "delete collection",
			method: http.MethodDelete,
			path:   "/api/v1/cli/collections/Work",
		},
		{
			name:   "list identities",
			method: http.MethodGet,
			path:   "/api/v1/cli/identities",
		},
		{
			name:   "add identity",
			method: http.MethodPost,
			path:   "/api/v1/cli/identities",
			body:   `{"account":"alice@example.com","identifier":"alias@example.com","signal":"manual"}`,
		},
		{
			name:   "remove identity",
			method: http.MethodDelete,
			path:   "/api/v1/cli/identities",
			body:   `{"account":"alice@example.com","identifier":"alias@example.com"}`,
		},
		{
			name:   "plan dedup deletion",
			method: http.MethodPost,
			path:   "/api/v1/cli/delete-deduped/plan",
			body:   `{"all_hidden":true}`,
		},
		{
			name:   "execute dedup deletion",
			method: http.MethodPost,
			path:   "/api/v1/cli/delete-deduped",
			body:   `{"all_hidden":true,"no_backup":true,"expected_total":0,"expected_batch_count":0,"expected_batches":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(1)
			conn, err := st.DB().Conn(t.Context())
			require.NoError(t, err, "hold the only database connection")
			t.Cleanup(func() { _ = conn.Close() })

			srv := api.NewServerWithOptions(api.ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{
					APIKey: "secret-key",
				}},
				Store:  &storeAPIAdapter{store: st},
				Logger: slog.New(slog.DiscardHandler),
			})

			ctx, cancel := context.WithCancel(context.Background())
			var body *strings.Reader
			if tt.body == "" {
				body = strings.NewReader("")
			} else {
				body = strings.NewReader(tt.body)
			}
			req := httptest.NewRequest(tt.method, tt.path, body).WithContext(ctx)
			req.Header.Set("X-Api-Key", "secret-key")
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			waitCount := st.DB().Stats().WaitCount
			requestDone := make(chan struct{})
			go func() {
				srv.Router().ServeHTTP(httptest.NewRecorder(), req)
				close(requestDone)
			}()

			require.Eventually(t, func() bool {
				return st.DB().Stats().WaitCount > waitCount
			}, 2*time.Second, 10*time.Millisecond, "handler reaches production database work")
			cancel()

			select {
			case <-requestDone:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "production store work continued after marked request cancellation")
			}
		})
	}
}

// TestStoreAPIAdapterContextReadsHonorCancellation verifies the adapter's
// context-aware read methods thread the caller's context into the underlying
// store, so an abandoned or timed-out request is cancelled instead of running
// to completion on a background context.
func TestStoreAPIAdapterContextReadsHonorCancellation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	_, err = st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")

	adapter := &storeAPIAdapter{store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = adapter.GetStatsContext(ctx)
	require.ErrorIs(err, context.Canceled, "GetStatsContext must honor a cancelled context")

	_, _, err = adapter.ListMessagesContext(ctx, 0, 10)
	require.ErrorIs(err, context.Canceled, "ListMessagesContext must honor a cancelled context")

	_, err = adapter.GetMessageContext(ctx, 1)
	require.ErrorIs(err, context.Canceled, "GetMessageContext must honor a cancelled context")

	_, err = adapter.GetMessagesSummariesByIDsContext(ctx, []int64{1})
	require.ErrorIs(err, context.Canceled, "GetMessagesSummariesByIDsContext must honor a cancelled context")
}

func TestStoreAPIAdapterMeetingImport(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	req, err := meetingimport.DecodeRequest(strings.NewReader(`{
		"source": {
			"identifier": "local-meetings",
			"account_email": "user@example.com"
		},
		"meeting": {
			"external_id": "42",
			"started_at": "2026-07-23T18:00:00Z",
			"transcript": "Speaker 1: synthetic transcript"
		}
	}`), meetingimport.MaxRequestBytes)
	require.NoError(err)
	adapter := &storeAPIAdapter{
		store:           st,
		meetingImporter: meetingimport.NewImporter(st, meetingimport.Hooks{}),
	}

	result, err := adapter.ImportMeeting(context.Background(), req)
	require.NoError(err)
	require.Equal(meetingimport.StatusCreated, result.Status)
	require.NotZero(result.MessageID)
}

var _ api.ConversationWindowStore = (*storeAPIAdapter)(nil)

// TestStoreAPIAdapterImplementsConversationWindowStore is a compile-time
// guard that the production adapter wired into api.Server satisfies the
// context-aware conversation reader. Without it, GET /conversations/{id}
// falls back to an unexported interface the adapter can never satisfy and
// returns 503 conversation_unavailable in daemon mode.
func TestStoreAPIAdapterImplementsConversationWindowStore(t *testing.T) {
	var adapter api.MessageStore = &storeAPIAdapter{}
	_, ok := adapter.(api.ConversationWindowStore)
	require.True(t, ok, "storeAPIAdapter must implement api.ConversationWindowStore")
}

// TestStoreAPIAdapterConversationWindowContext verifies the adapter's
// conversation-window pass-throughs return a real window from the
// underlying store, both unbounded and time-bounded.
func TestStoreAPIAdapterConversationWindowContext(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	conversationID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")

	sentAt := time.Date(2015, 1, 7, 12, 0, 0, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
	})
	require.NoError(err, "UpsertMessage")

	adapter := &storeAPIAdapter{store: st}
	ctx := context.Background()

	exists, err := adapter.ConversationExistsContext(ctx, conversationID)
	require.NoError(err, "ConversationExistsContext")
	require.True(exists, "conversation should exist")

	window, err := adapter.GetConversationWindowContext(ctx, conversationID, messageID, 5, 5, nil, nil)
	require.NoError(err, "GetConversationWindowContext unbounded")
	require.Len(window.Messages, 1, "unbounded window should contain the seeded message")
	require.Equal(messageID, window.Messages[0].ID, "unbounded window message ID")

	start := sentAt.Add(-time.Hour)
	end := sentAt.Add(time.Hour)
	bounded, err := adapter.GetConversationWindowContext(ctx, conversationID, messageID, 5, 5, &start, &end)
	require.NoError(err, "GetConversationWindowContext bounded")
	require.Len(bounded.Messages, 1, "bounded window should contain the seeded message")
	require.Equal(messageID, bounded.Messages[0].ID, "bounded window message ID")
}
