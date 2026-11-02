package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthConversationWindow(t *testing.T) {
	for _, backend := range []string{"store", "query"} {
		t.Run(backend, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			f := storetest.New(t)
			id := f.NewMessage().Create(t, f.Store)
			pid := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
			must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{pid}, nil))
			must.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "Body", Valid: true}, sql.NullString{}))
			must.NoError(f.Store.UpsertMessageRaw(id, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nBody")))
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}, SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.test"}}}}}
			var engine query.Engine
			if backend == "query" {
				engine = query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
			}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Engine: engine, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", f.ConvID, id), nil)
			req.Header.Set("Authorization", "Bearer owner")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			must.Equal(http.StatusOK, response.Code, response.Body.String())
			var window ConversationResponse
			must.NoError(json.Unmarshal(response.Body.Bytes(), &window))
			must.Len(window.Messages, 1)
			must.NotNil(window.Messages[0].SenderAuth)
			checks.True(window.Messages[0].SenderAuth.Verified)
		})
	}
}
