package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonSenderCeilingUsesProductionStoreAPIAdapter(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("contact@example.test", "Contact", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}},
		Store:  &storeAPIAdapter{store: f.Store},
		Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/people/%d", person.ID), nil)
	req.Header.Set("Authorization", "Bearer owner")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var got store.Person
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &got))
	requirements.Equal(store.SenderCeilingContact, got.SenderCeilingReadout.SenderClassCeiling)
	requirements.Equal(store.SenderCeilingCuratedPerson, got.SenderCeilingReadout.SenderClassCeilingBasis)
}
