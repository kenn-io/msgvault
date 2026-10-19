package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInboxReadHTTPRejectsRecreatedSourceGrant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	old, err := st.GetOrCreateSource("gmail", "reader@example.test")
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, st, nil, testLogger())
	issue := func(sourceID int64) string {
		encoded, issueErr := json.Marshal(agentTokenIssueRequest{Label: "source-lifetime", Permissions: []string{string(agentgrant.PermissionInboxRead), string(agentgrant.PermissionInboxContentRead)}, SourceIDs: []int64{sourceID}})
		requirements.NoError(issueErr)
		req := httptest.NewRequest(http.MethodPost, agentTokensPath, strings.NewReader(string(encoded)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer synthetic-owner-key")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		requirements.Equal(http.StatusCreated, response.Code)
		var issued agentTokenIssueResponse
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &issued))
		requirements.NotEmpty(issued.Secret)
		return issued.Secret
	}
	secret := issue(old.ID)
	grant, found := srv.agentGrants.Lookup(secret)
	requirements.True(found)
	requirements.NoError(st.RemoveSource(old.ID))
	current, err := st.GetOrCreateSource("gmail", old.Identifier)
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(current.ID, "new-lifetime", "Synthetic source")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: current.ID, ConversationID: conversation, SourceMessageID: "new-lifetime-message", MessageType: "email"})
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: current.ID, SourceType: "gmail", SourceIdentifier: current.Identifier, AccountID: current.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "new-lifetime-message"}
	_, err = st.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic recreated source body")
	requirements.NoError(err)
	// General archive grants deliberately match across reimports. Inbox reads
	// additionally require the original source lifetime, checked by real routes.
	assertions.True(grant.Allows(agentgrant.PermissionInboxRead, agentgrant.SourceRef{ID: current.ID, Type: "gmail", Identifier: current.Identifier}))
	freshSecret := issue(current.ID)
	query := url.Values{"source_id": {strconv.FormatInt(current.ID, 10)}, "source_type": {"gmail"}, "source_identifier": {current.Identifier}, "account_id": {current.Identifier}, "scope": {"message"}}
	body, err := json.Marshal(inboxcontrol.ContextRequest{Target: target})
	requirements.NoError(err)
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/inbox/candidates?" + query.Encode(), ""},
		{http.MethodPost, "/api/v1/inbox/context", string(body)},
	} {
		for _, credential := range []struct {
			name, token string
			owner       bool
			want        int
		}{
			{name: "retired grant", token: secret, want: http.StatusForbidden},
			{name: "owner", owner: true, want: http.StatusOK},
			{name: "fresh grant", token: freshSecret, want: http.StatusOK},
		} {
			req := httptest.NewRequest(call.method, call.path, strings.NewReader(call.body))
			req.Header.Set("Content-Type", "application/json")
			if credential.owner {
				req.Header.Set("Authorization", "Bearer synthetic-owner-key")
			} else {
				req.Header.Set(apiprotocol.AgentTokenHeader, credential.token)
			}
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			assertions.Equal(credential.want, response.Code, "%s %s", credential.name, call.method)
			if credential.want == http.StatusForbidden {
				assertions.NotContains(response.Body.String(), "Synthetic recreated")
				continue
			}
			if call.method == http.MethodPost {
				var content inboxcontrol.Context
				requirements.NoError(json.Unmarshal(response.Body.Bytes(), &content))
				assertions.Equal(target, content.Target)
				assertions.Equal("Synthetic recreated source body", content.Text)
			} else {
				var page inboxcontrol.CandidatePage
				requirements.NoError(json.Unmarshal(response.Body.Bytes(), &page))
				requirements.Len(page.Candidates, 1)
				assertions.Equal(target, page.Candidates[0].State.Target)
			}
		}
	}
}
