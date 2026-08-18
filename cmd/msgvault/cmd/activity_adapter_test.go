package cmd

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

// The daemon passes storeAPIAdapter, not *store.Store, to the API server.
// Keep this compile-time guard next to the production adapter so the optional
// activity interface cannot silently regress to the route's 503 fallback.
var _ api.ActivityStore = (*storeAPIAdapter)(nil)

func TestStoreAPIAdapterServesActivityRoute(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "activity-adapter@example.test", "Activity Adapter")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
	request := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/v1/persons/%d/contact-state", person.ID),
		nil,
	)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
}
