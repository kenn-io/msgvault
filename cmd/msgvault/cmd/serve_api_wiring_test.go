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

func TestStoreAPIAdapterExposesCuratedPeopleCompletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "completion-adapter@example.test", "Completion Adapter",
	)
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameNickname, Formatted: new("Adapter Nickname"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	adapter := &storeAPIAdapter{store: st}
	rows, err := adapter.CompletePersonProfilesContext(t.Context(), store.PersonCompletionQuery{
		Query: "nickname", Limit: 8,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(participantID, rows[0].ParticipantID)
	assert.Equal("Adapter Nickname", rows[0].Value)
}

func TestStoreAPIAdapterRecreatesConsentedDocumentSearchConsumer(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("document-search-adapter")
	hash := strings.Repeat("a", 64)
	require.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "evidence.pdf", MIMEType: "application/pdf", Size: 128,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	fingerprint := strings.Repeat("b", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint,
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: documentindex.ModelMistralOCR,
		RetentionPosture:  string(documentindex.RetentionStandard),
		TrainingPosture:   string(documentindex.TrainingOptedOut),
		AllowedMediaTypes: []string{"application/pdf"},
		PolicyJSON:        []byte(`{"normalization":1}`),
	}
	_, err := fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	adapter := &storeAPIAdapter{store: fixture.Store}
	_, err = adapter.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "absent"})
	require.NoError(err)
	consumer, err := fixture.Store.GetAttachmentChangeConsumer(
		t.Context(), documentindex.DocumentAttachmentConsumerKey,
	)
	require.NoError(err)
	assert.True(consumer.ReconciliationComplete)
	var occurrences int
	require.NoError(fixture.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM document_occurrences`,
	).Scan(&occurrences))
	assert.Equal(1, occurrences)
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
		{"structured profile", fmt.Sprintf("/api/v1/people/%d/profile", person.ID)},
		{"person network", fmt.Sprintf("/api/v1/people/%d/network", person.ID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			srv.Router().ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		})
	}
}

func TestStoreAPIAdapterServesPersonTracking(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "tracking-adapter@example.test", "Tracking Adapter")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
	request := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/v1/people/%d/tracking", person.ID),
		strings.NewReader(`{"tracked":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"tracked":true`)
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

func TestStoreAPIAdapterServesOrganizationAndEmploymentRoutes(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "employment-adapter@example.test", "Employment Adapter",
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
		{"organizations", "/api/v1/organizations"},
		{"employments", fmt.Sprintf("/api/v1/people/%d/employments", person.ID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			srv.Router().ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		})
	}
}
