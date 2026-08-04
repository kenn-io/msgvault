package cmd

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The daemon serves the API through storeAPIAdapter, never through *store.Store.
// An adapter that does not implement api.ChangedMessageLister makes the route
// answer 503 in production while every test that injects a real store passes,
// so this assertion is on the type production actually passes.
var _ api.ChangedMessageLister = (*storeAPIAdapter)(nil)

// changesPage serves one feed request and decodes a successful response. The
// status assertion fires on the first call, so a route that reports itself
// unavailable is caught before any settling happens.
func changesPage(t *testing.T, baseURL string) api.ChangesResponse {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/v1/messages/changes?limit=100")
	require.NoError(t, err, "GET /messages/changes")
	defer func() { _ = resp.Body.Close() }()
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"the daemon's own adapter must serve the feed, not report it unavailable: %s",
		resp.Status)
	var page api.ChangesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&page), "decode feed page")
	return page
}

func changesFeedTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	require.NoErrorf(t, err, "feed timestamp %q must parse as RFC3339", value)
	return at
}

// settleChangesFeed returns the first page whose bound has left every watermark
// written so far. The feed withholds the instant it is bounded at -- that instant
// can still receive commits -- so a test that seeds a row and then expects to see
// it has to let the bound move past it. Mirrors settleChangesClock in
// internal/api/changes_test.go, over HTTP, because that helper is unexported in
// package api and this test is package cmd. Named rather than pointed at by
// line, so the reference cannot rot.
func settleChangesFeed(t *testing.T, baseURL string) api.ChangesResponse {
	t.Helper()
	start := changesFeedTime(t, changesPage(t, baseURL).ServerTime)
	deadline := time.Now().Add(30 * time.Second)
	for {
		page := changesPage(t, baseURL)
		if changesFeedTime(t, page.CompleteThrough).After(start) {
			return page
		}
		if time.Now().After(deadline) {
			require.Failf(t, "the change feed stopped advancing",
				"complete_through never moved past %s; something is holding a write "+
					"transaction open", start)
			return page
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestChangesEndpointServesThroughTheProductionAdapter pins that the change feed
// is reachable in the daemon. It builds the server exactly as serve.go does --
// Store: &storeAPIAdapter{...} -- and asserts a real message comes back over
// HTTP. Before the adapter carried ListChangedMessages this returned
// 503 feature_unavailable no matter what the archive held.
func TestChangesEndpointServesThroughTheProductionAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmpDir := t.TempDir()
	s := testutil.NewTestStore(t)

	src, err := s.GetOrCreateSource("gmail", "archive-owner@example.com")
	require.NoError(err, "create source")
	conv, err := s.EnsureConversation(src.ID, "c1", "")
	require.NoError(err, "create conversation")
	msgID, err := s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv,
		SourceMessageID: "gm-changes-e2e-1", MessageType: "email",
		Subject:      sql.NullString{String: "In the feed", Valid: true},
		SizeEstimate: 100,
	})
	require.NoError(err, "insert message")

	engine := query.NewEngine(s.DB(), s.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Data: config.DataConfig{DataDir: tmpDir}},
		Store:  &storeAPIAdapter{store: s},
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	page := settleChangesFeed(t, httpSrv.URL)

	require.Len(page.Messages, 1, "the archive's one message must appear in the feed")
	assert.Equal(msgID, page.Messages[0].ID, "and it must be the message that was inserted")
	assert.NotEmpty(page.NextCursor, "a page carrying rows must hand back a cursor")
}
