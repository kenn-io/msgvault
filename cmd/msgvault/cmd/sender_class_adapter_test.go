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
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	participantID := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	sent := f.NewMessage().Build()
	sent.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
	sent.IsSent = sql.NullBool{Bool: true, Valid: true}
	_, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: sent,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
			{Type: "to", ParticipantIDs: []int64{participantID}, EmailAddresses: []string{"sender@example.test"}},
		},
		LabelRefs: []store.MessageLabelRef{{
			SourceLabelID: "SENT",
			Info:          store.LabelInfo{Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		}},
	})
	requirements.NoError(err)
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
	requirements.Equal(senderclass.Known, detail.SenderClass)
	requirements.Equal(senderclass.OwnerSentTo, detail.SenderClassBasis)
	requirements.Equal(senderclass.RelationshipKnown, detail.SenderRelationship)
}
