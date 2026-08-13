package cmd

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestServeRuntimeConfigCarriesVectorScopeBeforeInitialization(t *testing.T) {
	vectorCfg := config.NewDefaultConfig().Vector
	vectorCfg.Enabled = true
	vectorCfg.Embed.Scope.MessageTypes = []string{"teams"}
	opts := api.ServerOptions{}

	applyServerRuntimeConfig(&opts, &config.Config{Vector: vectorCfg})

	assert.Equal(t, []string{"teams"}, opts.VectorCfg.Embed.Scope.MessageTypes)
}

func TestStoreAPIAdapterExposesFileMetadataCatalog(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	adapter := &storeAPIAdapter{store: st}

	file, err := adapter.GetFileMetadata(t.Context(), 999999)
	requirements.NoError(err)
	assertions.Nil(file)
	files, err := adapter.GetFileMetadataBatch(t.Context(), nil)
	requirements.NoError(err)
	assertions.Empty(files)
}

func TestStoreAPIAdapterReconcilesEnabledDocumentSearchConsumer(t *testing.T) {
	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("document-search-adapter")
	hash := strings.Repeat("a", 64)
	require.NoError(t, fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "evidence.pdf", MIMEType: "application/pdf", Size: 128,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	_, created, err := fixture.Store.RegisterAttachmentChangeConsumer(
		t.Context(), documentindex.DocumentAttachmentConsumerKey,
	)
	require.NoError(t, err)
	require.True(t, created)

	adapter := &storeAPIAdapter{store: fixture.Store}
	_, err = adapter.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "absent"})
	require.NoError(t, err)
	consumer, err := fixture.Store.GetAttachmentChangeConsumer(
		t.Context(), documentindex.DocumentAttachmentConsumerKey,
	)
	require.NoError(t, err)
	assert.True(t, consumer.ReconciliationComplete)
	var occurrences int
	require.NoError(t, fixture.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM document_occurrences`,
	).Scan(&occurrences))
	assert.Equal(t, 1, occurrences)
}

func TestStoreAPIAdapterServesProfileAndCommunicationServiceRoutes(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "production-adapter@example.test", "Production Adapter",
	)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})

	for _, test := range []struct {
		name string
		path string
	}{
		{"communication services", "/api/v1/communication-services"},
		{"structured profile", fmt.Sprintf("/api/v1/persons/%d/profile", person.ID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			srv.Router().ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		})
	}
}

func TestStoreAPIAdapterServesPersonRelationshipRoutes(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/relationship-types", nil)
	response := httptest.NewRecorder()

	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
}
