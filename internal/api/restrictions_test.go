package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type restrictionTestStore struct {
	*stubIdentityCacheStore

	calls int
}

func (s *restrictionTestStore) RunCLICommand(_ context.Context, _ CLIRunRequest, _ func(CLIRunEvent) error) error {
	s.calls++
	return nil
}

func TestRestrictionsOwnerAndGrantBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "rules@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(err)
	var messageID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO messages(conversation_id,source_id,message_type) VALUES(?,?,?) RETURNING id`), conv, source.ID, "email").Scan(&messageID)
	require.NoError(err)
	reg := agentgrant.NewRegistry()
	fixture := &restrictionTestStore{stubIdentityCacheStore: &stubIdentityCacheStore{Store: st}}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}, Store: fixture, Logger: testLogger(), Scheduler: newMockScheduler()})
	srv.agentGrants = reg
	_, token, _, err := reg.Issue("synthetic", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	require.NoError(err)
	request := func(method, path, body string, owner bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if owner {
			req.Header.Set("Authorization", "Bearer owner-key")
		} else {
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	body := fmt.Sprintf(`{"kind":"source","source_id":%d}`, source.ID)
	w := request(http.MethodPost, "/api/v1/restrictions", body, true)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.Contains(w.Body.String(), "do not yet restrict reads")
	var response generated.RestrictionResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(response.Rules, 1)
	assert.Empty(response.Rules[0].Note)
	require.NoError(response.Validate(), "the generated client must accept a rule without an optional note")
	w = request(http.MethodGet, "/api/v1/restrictions", "", true)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.Contains(w.Body.String(), `"kind":"source"`)
	w = request(http.MethodGet, fmt.Sprintf("/api/v1/restrictions/check/%d", messageID), "", true)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	for _, tc := range []struct{ method, path, body string }{{http.MethodGet, "/api/v1/restrictions", ""}, {http.MethodPost, "/api/v1/restrictions", body}, {http.MethodDelete, "/api/v1/restrictions/1", ""}, {http.MethodGet, fmt.Sprintf("/api/v1/restrictions/check/%d", messageID), ""}, {http.MethodPut, "/api/v1/restrictions/taint", `{"enabled":false}`}, {http.MethodPost, "/api/v1/cli/run", `{"args":["restrict","list"]}`}, {http.MethodPost, "/api/v1/cli/run", `{"args":["restrict","taint","off"]}`}} {
		w = request(tc.method, tc.path, tc.body, false)
		if tc.path == "/api/v1/cli/run" {
			assert.Equal(http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(w.Body.String(), "command_not_allowed")
		} else {
			assert.Equal(http.StatusUnauthorized, w.Code, w.Body.String())
			assert.Contains(w.Body.String(), `"unauthorized"`)
		}
	}
	rules, err := st.ListAgentRestrictions(t.Context())
	require.NoError(err)
	require.Len(rules, 1)
	taint, err := st.AgentRestrictionTaint(t.Context())
	require.NoError(err)
	assert.True(taint)
	assert.Zero(fixture.calls)
	w = request(http.MethodDelete, fmt.Sprintf("/api/v1/restrictions/%d", rules[0].ID), "", true)
	assert.Equal(http.StatusOK, w.Code, w.Body.String())
}
