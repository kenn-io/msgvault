package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/senderclass"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMessageSenderClassUsesProductionStoreAPIAdapter(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: f.NewMessage().Build(),
		Recipients: []store.RecipientSet{{
			Type: "from", ParticipantIDs: []int64{participantID}, EmailAddresses: []string{"sender@example.test"},
		}},
	})
	requirements.NoError(err)
	requirements.NoError(f.Store.UpsertMessageBody(messageID, sql.NullString{String: "Body", Valid: true}, sql.NullString{}))
	requirements.NoError(f.Store.UpsertMessageRaw(messageID, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nBody")))

	cfg := &config.Config{
		HomeDir: t.TempDir(),
		Data:    config.DataConfig{DataDir: t.TempDir()},
		Server:  config.ServerConfig{APIKey: "owner"},
		SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{
			SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.test"},
		}}},
	}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", messageID), nil)
	req.Header.Set("Authorization", "Bearer owner")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var detail api.MessageDetail
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &detail))
	requirements.NotNil(detail.SenderAuth)
	requirements.True(detail.SenderAuth.Verified)
	requirements.Equal(senderclass.Verified, detail.SenderClass)
	requirements.Equal(senderclass.RelationshipNone, detail.SenderRelationship)
}
