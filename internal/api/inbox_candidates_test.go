package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type inboxCandidateHTTPStore struct {
	mockStore

	archive *store.Store
}

func (s *inboxCandidateHTTPStore) InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error) {
	return s.archive.InboxCandidates(ctx, source, scope, limit, cursor)
}

// Exercises the real router, grant registry and Store query. Removing source
// authorization would expose the foreign candidate; dropping cursor conflict
// handling would permit stale pagination after incoming mail.
func TestInboxCandidatesHTTPSourceAuthorization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
	for _, id := range []string{"candidate-one", "candidate-two"} {
		mid := f.CreateMessage(id)
		_, err := f.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: id}, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
		requirements.NoError(err)
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, &inboxCandidateHTTPStore{archive: f.Store}, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	grantID, secret, _, err := srv.agentGrants.Issue("inbox-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: source.SourceID, Type: source.SourceType, Identifier: source.SourceIdentifier}})
	requirements.NoError(err)
	query := url.Values{"source_id": {strconv.FormatInt(source.SourceID, 10)}, "source_type": {source.SourceType}, "source_identifier": {source.SourceIdentifier}, "account_id": {source.AccountID}, "scope": {"message"}, "limit": {"1"}}
	call := func(q url.Values, owner, agent string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/inbox/candidates?"+q.Encode(), nil)
		if owner != "" {
			r.Header.Set("Authorization", "Bearer "+owner)
		}
		if agent != "" {
			r.Header.Set(apiprotocol.AgentTokenHeader, agent)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	response := call(query, "", secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var page inboxcontrol.CandidatePage
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &page))
	requirements.Len(page.Candidates, 1)
	requirements.NotEmpty(page.NextCursor)
	assertions.Equal(source, page.Source)
	query.Set("cursor", page.NextCursor)
	response = call(query, "", secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var next inboxcontrol.CandidatePage
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &next))
	requirements.Len(next.Candidates, 1)
	assertions.NotEqual(page.Candidates[0].State.Target, next.Candidates[0].State.Target)
	f.CreateMessage("incoming-unobserved")
	response = call(query, "", secret)
	assertions.Equal(http.StatusConflict, response.Code, response.Body.String())
	query.Del("cursor")
	query.Set("source_identifier", "foreign@example.test")
	response = call(query, "", secret)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	query.Set("source_identifier", source.SourceIdentifier)
	query.Set("account_id", "foreign@example.test")
	response = call(query, "synthetic-owner-key", "")
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	query.Set("account_id", source.AccountID)
	for _, limit := range []string{"0", "101", "bad"} {
		query.Set("limit", limit)
		response = call(query, "synthetic-owner-key", "")
		assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	query.Del("limit")
	response = call(query, "synthetic-owner-key", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &next))
	assertions.Len(next.Candidates, 2)
	assertions.True(next.Unavailable)
	query.Set("unknown", "true")
	response = call(query, "synthetic-owner-key", "")
	assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	query.Del("unknown")
	query.Set("scope", "chat")
	response = call(query, "synthetic-owner-key", "")
	assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	query.Set("scope", "message")
	query.Set("limit", "1")
	query.Add("source_id", "999")
	response = call(query, "synthetic-owner-key", "")
	assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	query.Set("source_id", strconv.FormatInt(source.SourceID, 10))
	requirements.True(srv.agentGrants.Revoke(grantID))
	response = call(query, "", secret)
	assertions.Equal(http.StatusUnauthorized, response.Code, response.Body.String())
}
