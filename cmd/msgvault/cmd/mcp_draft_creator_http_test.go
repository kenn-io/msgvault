package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPDraftCreatorAuthenticatedHTTPModes(t *testing.T) {
	req := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic-slack")
	req.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "draft-channel", "channel", "Synthetic draft channel")
	req.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	router := server.Router()
	send := func(path string, body any, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		req.NoError(err)
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	ownerHeaders := map[string]string{"X-Api-Key": "synthetic-owner-key"}
	login := send("/api/session/login", map[string]any{"api_key": "synthetic-owner-key"}, nil)
	req.Equal(http.StatusOK, login.Code, login.Body.String())
	var session api.SessionStatus
	req.NoError(json.Unmarshal(login.Body.Bytes(), &session))
	cookies := login.Result().Cookies()
	req.NotEmpty(cookies)
	issued := send("/api/v1/agent-tokens", map[string]any{"label": "synthetic-agent", "permissions": []string{"draft.create"}, "source_ids": []int64{source.ID}}, ownerHeaders)
	req.Equal(http.StatusCreated, issued.Code, issued.Body.String())
	var grant struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	req.NoError(json.Unmarshal(issued.Body.Bytes(), &grant))
	req.NotEmpty(grant.ID)
	req.NotEmpty(grant.Secret)
	for _, tc := range []struct {
		name, want string
		headers    map[string]string
		cookies    []*http.Cookie
	}{
		{"owner", "owner", ownerHeaders, nil},
		{"session", "session", map[string]string{"X-Csrf-Token": session.CSRFToken, "Origin": "http://example.com"}, cookies},
		{"agent", "agent:" + grant.ID, map[string]string{"X-Msgvault-Agent-Token": grant.Secret}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			response := send("/api/v1/cli/run", map[string]any{"args": []string{"draft-compose", "--conversation", strconv.FormatInt(conv, 10), "--body", tc.name + " synthetic draft", "--json"}}, tc.headers, tc.cookies...)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var creator sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_by_principal FROM chat_drafts WHERE conversation_id = ? AND body = ?`), conv, tc.name+" synthetic draft").Scan(&creator))
			assert.Equal(tc.want, creator.String)
			assert.True(creator.Valid)
		})
	}
}
