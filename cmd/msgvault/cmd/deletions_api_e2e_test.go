package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestDeletionStagingEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	tmpDir := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: tmpDir}}

	s, err := store.Open(tmpDir + "/msgvault.db")
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	t.Cleanup(func() { _ = s.Close() })

	src, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	conv, err := s.EnsureConversation(src.ID, "c1", "")
	require.NoError(err, "create conversation")
	msgID, err := s.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv,
		SourceMessageID: "gm-e2e-1", MessageType: "email",
		Subject:      sql.NullString{String: "Stage me", Valid: true},
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

	// Stage by message ID.
	body, err := json.Marshal(map[string]any{
		"message_ids": []int64{msgID},
		"description": "e2e staging",
	})
	require.NoError(err, "marshal request")
	resp, err := http.Post(httpSrv.URL+"/api/v1/deletions", "application/json", bytes.NewReader(body))
	require.NoError(err, "POST /deletions")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(http.StatusCreated, resp.StatusCode, "stage status")
	var staged api.StageDeletionResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&staged), "decode stage response")
	assert.Equal(1, staged.MessageCount)
	assert.Equal("alice@example.com", staged.Account, "resolved account reported")
	require.NotEmpty(staged.ID, "manifest id")

	// The persisted manifest must carry the account delete-staged
	// executes against.
	adapter := &storeAPIAdapter{store: s}
	persisted, _, err := adapter.GetDeletionManifest(context.Background(), staged.ID)
	require.NoError(err, "load persisted manifest")
	assert.Equal("alice@example.com", persisted.Filters.Account, "manifest account")

	// List pending — the staged manifest appears.
	listResp, err := http.Get(httpSrv.URL + "/api/v1/deletions?status=pending")
	require.NoError(err, "GET /deletions")
	defer func() { _ = listResp.Body.Close() }()
	require.Equal(http.StatusOK, listResp.StatusCode, "list status")
	var listed api.ListDeletionsResponse
	require.NoError(json.NewDecoder(listResp.Body).Decode(&listed), "decode list response")
	require.Len(listed.Manifests, 1)
	assert.Equal(staged.ID, listed.Manifests[0].ID)
	assert.Equal("api", listed.Manifests[0].CreatedBy)

	// Cancel it.
	req, err := http.NewRequest(http.MethodDelete, httpSrv.URL+"/api/v1/deletions/"+staged.ID, nil)
	require.NoError(err, "build DELETE request")
	cancelResp, err := http.DefaultClient.Do(req)
	require.NoError(err, "DELETE /deletions/{id}")
	defer func() { _ = cancelResp.Body.Close() }()
	require.Equal(http.StatusOK, cancelResp.StatusCode, "cancel status")

	// Second cancel conflicts.
	req2, err := http.NewRequest(http.MethodDelete, httpSrv.URL+"/api/v1/deletions/"+staged.ID, nil)
	require.NoError(err, "build second DELETE request")
	conflictResp, err := http.DefaultClient.Do(req2)
	require.NoError(err, "second DELETE")
	defer func() { _ = conflictResp.Body.Close() }()
	assert.Equal(http.StatusConflict, conflictResp.StatusCode, "second cancel conflicts")

	// Pending list is empty again.
	list2, err := http.Get(httpSrv.URL + "/api/v1/deletions?status=pending")
	require.NoError(err, "GET /deletions after cancel")
	defer func() { _ = list2.Body.Close() }()
	require.Equal(http.StatusOK, list2.StatusCode, "list after cancel status")
	var listedAfter api.ListDeletionsResponse
	require.NoError(json.NewDecoder(list2.Body).Decode(&listedAfter), "decode second list")
	assert.Empty(listedAfter.Manifests, "no pending manifests after cancel")
}
