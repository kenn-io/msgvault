package cmd

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestVerificationArrivalsUsesProductionStoreAPIAdapter(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "verification-adapter@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "verification-adapter-thread", "direct_chat", "Synthetic")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "verification-adapter-message", MessageType: "beeper",
		Subject: sql.NullString{String: "Routine notice", Valid: true},
		SentAt:  sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true},
	})
	requirements.NoError(err)
	body := "Your verification code is 482913."
	requirements.NoError(st.UpsertMessageBody(messageID, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	requirements.NoError(st.UpsertFTS(messageID, "Routine notice", body, "", "", ""))

	cfg := &config.Config{
		HomeDir: t.TempDir(),
		Data:    config.DataConfig{DataDir: t.TempDir()},
		Server:  config.ServerConfig{APIKey: "synthetic-owner", AgentAccess: true},
	}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/verifications", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("X-Api-Key", "synthetic-owner")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	assertions.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), `"message_id":`)
	assertions.Contains(response.Body.String(), `"masked":true`)
	assertions.NotContains(response.Body.String(), "482913")
}
