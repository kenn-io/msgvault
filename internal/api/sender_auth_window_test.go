package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthConversationWindow(t *testing.T) {
	must := require.New(t)
	f := storetest.New(t)
	pid := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	message := f.NewMessage().Build()
	message.SenderID = sql.NullInt64{Int64: pid, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{pid}, nil))
	must.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "Body", Valid: true}, sql.NullString{}))
	must.NoError(f.Store.UpsertMessageRaw(id, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nBody")))
	_, outside, err := testutil.CreateIndexedSourceMessage(f.Store, "outside@example.test", "sender@example.test", "Outside", "Private fixture body")
	must.NoError(err)
	for _, withEngine := range []bool{true, false} {
		t.Run(fmt.Sprintf("analytics=%t", withEngine), func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			var engine query.Engine
			if withEngine {
				engine = query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
			}
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}, SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.test"}}}}}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Engine: engine, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			// Both fallback and request-snapshot readers must use startup pins.
			cfg.SenderAuth.Sources[0].TrustedAuthservIDs[0] = "untrusted.test"
			_, token, _, err := srv.agentGrants.Issue("receipt reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: f.Source.ID, Type: f.Source.SourceType, Identifier: f.Source.Identifier}}, time.Time{})
			must.NoError(err)
			windowPath := fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", f.ConvID, id)
			messagePath := fmt.Sprintf("/api/v1/messages/%d", id)
			// Conversation windows are owner-only; delegated reads exercise the
			// admitted message endpoint and its request-bound archive snapshot.
			for _, auth := range []struct {
				header, value string
				paths         []string
			}{
				{"Authorization", "Bearer owner", []string{windowPath, messagePath}},
				{"X-Msgvault-Agent-Token", token, []string{messagePath}},
			} {
				for _, path := range auth.paths {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					req.Header.Set(auth.header, auth.value)
					response := httptest.NewRecorder()
					srv.Router().ServeHTTP(response, req)
					must.Equal(http.StatusOK, response.Code, response.Body.String())
					var wire struct {
						SenderAuth *senderauth.Verdict `json:"sender_auth"`
						Messages   []struct {
							SenderAuth *senderauth.Verdict `json:"sender_auth"`
						} `json:"messages"`
					}
					must.NoError(json.Unmarshal(response.Body.Bytes(), &wire))
					verdict := wire.SenderAuth
					if wire.Messages != nil {
						must.Len(wire.Messages, 1)
						verdict = wire.Messages[0].SenderAuth
					}
					must.NotNil(verdict)
					checks.True(verdict.Verified, "path %s", path)
				}
			}
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", outside), nil)
			req.Header.Set("X-Msgvault-Agent-Token", token)
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			checks.Equal(http.StatusNotFound, response.Code, response.Body.String())
			checks.NotContains(response.Body.String(), "Private fixture body")
		})
	}
}
