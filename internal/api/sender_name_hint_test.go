package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCLISenderNameHintWithinGrant(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.com", "one", "Project plan", "")
	require.NoError(err)
	_, otherID, err := testutil.CreateIndexedSourceMessage(st, "other@example.com", "two", "Project plan", "")
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO participants(id,email_address,display_name) VALUES (1,'alex@example.com','Alex Kim'),(2,'alex@example.org','Alex Other');`)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES (?,1,'from'),(?,2,'from')`, id, otherID)
	require.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	require.NoError(err)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=from:alex@exam", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var response struct {
		Results []json.RawMessage `json:"results"`
		Hints   []struct {
			Message string `json:"message"`
		} `json:"hints"`
	}
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	assert.Empty(response.Results)
	require.Len(response.Hints, 1)
	assert.Contains(response.Hints[0].Message, "alex@example.com (1 message)")
	assert.NotContains(response.Hints[0].Message, "alex@example.org")
}
