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
	"go.kenn.io/msgvault/internal/senderclass"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderClassConversationWindow(t *testing.T) {
	for _, withEngine := range []bool{true, false} {
		t.Run(fmt.Sprintf("engine=%t", withEngine), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			participant := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
			id, err := f.Store.PersistMessage(&store.MessagePersistData{
				Message:    f.NewMessage().Build(),
				Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, EmailAddresses: []string{"sender@example.test"}}},
			})
			requirements.NoError(err)
			requirements.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "Body", Valid: true}, sql.NullString{}))
			raw := func(result string) []byte {
				return []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=" + result + " header.from=example.test\r\n\r\nBody")
			}
			requirements.NoError(f.Store.UpsertMessageRaw(id, raw("pass")))
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}, SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.test"}}}}}
			var engine query.Engine
			if withEngine {
				engine = query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
			}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Engine: engine, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			read := func() senderclass.Readout {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", f.ConvID, id), nil)
				req.Header.Set("Authorization", "Bearer owner")
				response := httptest.NewRecorder()
				srv.Router().ServeHTTP(response, req)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var window ConversationResponse
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &window))
				require.Len(t, window.Messages, 1)
				return window.Messages[0].Readout
			}
			assertions.Equal(senderclass.Verified, read().SenderClass)
			_, _, err = f.Store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			assertions.Equal(senderclass.Contact, read().SenderClass)
			requirements.NoError(f.Store.UpsertMessageRaw(id, raw("fail")))
			assertions.Equal(senderclass.Readout{SenderClass: senderclass.Unverified, SenderClassBasis: senderclass.AuthFailed, SenderRelationship: senderclass.RelationshipContact}, read())
		})
	}
}
