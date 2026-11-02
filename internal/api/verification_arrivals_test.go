package api

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/verification"
)

type staleVerificationArrivalAnalytics struct{ query.Engine }

func (staleVerificationArrivalAnalytics) ListMessages(context.Context, query.MessageFilter) ([]query.MessageSummary, error) {
	return nil, nil
}

func TestVerificationArrivalsAndQueryGuardHTTP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-source")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("sms", "other-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "synthetic-chat", "direct_chat", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "provider-message", MessageType: "beeper", Subject: sql.NullString{String: "glacier 482913", Valid: true}, SentAt: sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true}})
	requirements.NoError(err)
	body := "Your verification code is 482913"
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	requirements.NoError(st.UpsertFTS(id, "glacier 482913", body, "", "", ""))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "synthetic-owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: staleVerificationArrivalAnalytics{Engine: engine}, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	issue := func(permissions []agentgrant.Permission, source *store.Source) string {
		_, token, _, err := srv.agentGrants.Issue("synthetic reader", permissions, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
		requirements.NoError(err)
		return token
	}
	masked := issue([]agentgrant.Permission{agentgrant.PermissionSearchRead}, src)
	reveal := issue([]agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionVerificationRead}, src)
	modifierOnly := issue([]agentgrant.Permission{agentgrant.PermissionVerificationRead}, src)
	wrongSource := issue([]agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionVerificationRead}, other)
	requests := 0
	request := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		requests++
		r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", requests)
		if token == "owner" {
			r.Header.Set("X-Api-Key", "synthetic-owner")
		} else {
			r.Header.Set(apiprotocol.AgentTokenHeader, token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{masked, "owner"} {
		w := request("/api/v1/verifications", token)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), "482913")
		var page verification.ArrivalPage
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
		requirements.Len(page.Data, 1)
		assertions.Equal(id, page.Data[0].MessageID)
		assertions.Equal(src.ID, page.Data[0].SourceID)
		assertions.Equal(conv, page.Data[0].ConversationID)
		assertions.Equal("provider-message", page.Data[0].SourceMessageID)
		assertions.True(page.Data[0].Verification.Masked)
	}
	assertions.NotContains(request("/api/v1/verifications?reveal=true", masked).Body.String(), "482913", "the query parameter cannot override a grant")
	for _, tc := range []struct{ token, suffix string }{{reveal, ""}, {"owner", "?reveal=true"}} {
		w := request("/api/v1/verifications"+tc.suffix, tc.token)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), "482913")
		assertions.Contains(w.Body.String(), `"masked":false`)
	}
	w := request("/api/v1/verifications", wrongSource)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), `"data":[]`)
	assertions.Equal(http.StatusForbidden, request("/api/v1/verifications", modifierOnly).Code)
	assertions.Equal(http.StatusForbidden, request(fmt.Sprintf("/api/v1/verifications?source_id=%d", other.ID), masked).Code)
	for _, suffix := range []string{"?since=invalid", "?limit=101", "?kind=unsupported", "?include_expired=invalid"} {
		assertions.Equal(http.StatusBadRequest, request("/api/v1/verifications"+suffix, masked).Code, suffix)
	}
	for _, path := range []string{"/api/v1/cli/search?q=482913", "/api/v1/search?q=482913", "/api/v1/search/fast?q=482913&limit=20", "/api/v1/search/deep?q=482913", "/api/v1/search/deep?q=482913&scope=body"} {
		if st.IsPostgreSQL() && path == "/api/v1/search/deep?q=482913&scope=body" {
			continue
		} // exact body FTS is SQLite-only
		w := request(path, masked)
		requirements.Equal(http.StatusOK, w.Code, "%s: %s", path, w.Body.String())
		assertions.Contains(w.Body.String(), `"masked_hits_dropped":1`, path)
		assertions.NotContains(w.Body.String(), "provider-message", path)
		w = request(path, reveal)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), "provider-message", path)
	}
}
