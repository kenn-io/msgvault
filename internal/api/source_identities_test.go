package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type sourceIdentityAPIFixture struct {
	server      *Server
	store       *store.Store
	sourceA     *store.Source
	sourceB     *store.Source
	emptySource *store.Source
}

func newSourceIdentityAPIFixture(t *testing.T) sourceIdentityAPIFixture {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	sourceA, err := st.GetOrCreateSource("gmail", "account-a@example.test")
	requirements.NoError(err)
	sourceB, err := st.GetOrCreateSource("imap", "account-b@example.test")
	requirements.NoError(err)
	emptySource, err := st.GetOrCreateSource("mbox", "empty-source")
	requirements.NoError(err)

	requirements.NoError(st.AddAccountIdentity(sourceA.ID, "Second@Example.test", "manual"))
	requirements.NoError(st.AddAccountIdentity(sourceA.ID, "Masked-Shop@Example.test", "sent-folder"))
	requirements.NoError(st.AddAccountIdentity(sourceA.ID, "masked-shop@example.test", "provider-alias"))
	requirements.NoError(st.AddAccountIdentity(sourceB.ID, "Masked-Shop@Example.test", "other-source"))

	return sourceIdentityAPIFixture{
		server: NewServer(
			&config.Config{Server: config.ServerConfig{APIPort: 8080}},
			st,
			nil,
			testLogger(),
		),
		store: st, sourceA: sourceA, sourceB: sourceB, emptySource: emptySource,
	}
}

func TestSourceIdentitiesListsOnlyRequestedSource(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newSourceIdentityAPIFixture(t)
	response := getSourceIdentities(t, fixture.server, fixture.sourceA.ID)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())

	var body SourceIdentitiesResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Equal(fixture.sourceA.ID, body.SourceID)
	assertions.Equal("account-a@example.test", body.Account)
	requirements.Len(body.Identities, 2)
	assertions.Equal("Masked-Shop@Example.test", body.Identities[0].Identifier, "stored spelling is preserved")
	assertions.Equal([]string{"provider-alias", "sent-folder"}, body.Identities[0].Signals)
	assertions.Equal("Second@Example.test", body.Identities[1].Identifier)
	assertions.Equal([]string{"manual"}, body.Identities[1].Signals)

	stored, err := fixture.store.ListAccountIdentities(fixture.sourceA.ID)
	requirements.NoError(err)
	requirements.Len(stored, 2)
	assertions.True(body.Identities[0].ConfirmedAt.Equal(stored[0].ConfirmedAt), "confirmation instant")
	for _, identity := range body.Identities {
		assertions.NotContains(identity.Signals, "other-source", "same identifier on another source must not leak")
	}

	var wire map[string]json.RawMessage
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &wire))
	assertions.Len(wire, 3)
	assertions.Contains(wire, "source_id")
	assertions.Contains(wire, "account")
	assertions.Contains(wire, "identities")
}

func TestSourceIdentitiesReturnsStableEmptyArray(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newSourceIdentityAPIFixture(t)
	response := getSourceIdentities(t, fixture.server, fixture.emptySource.ID)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())

	var body SourceIdentitiesResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Equal(fixture.emptySource.ID, body.SourceID)
	assertions.Equal("empty-source", body.Account)
	assertions.NotNil(body.Identities)
	assertions.Empty(body.Identities)
	assertions.JSONEq(`{"source_id":3,"account":"empty-source","identities":[]}`, response.Body.String())
}

func TestSourceIdentitiesRejectsInvalidAndUnknownSourceIDs(t *testing.T) {
	fixture := newSourceIdentityAPIFixture(t)
	tests := []struct {
		name       string
		sourceID   string
		wantStatus int
		wantCode   string
	}{
		{name: "not numeric", sourceID: "not-a-number", wantStatus: http.StatusBadRequest, wantCode: "invalid_source_id"},
		{name: "zero", sourceID: "0", wantStatus: http.StatusBadRequest, wantCode: "invalid_source_id"},
		{name: "negative", sourceID: "-1", wantStatus: http.StatusBadRequest, wantCode: "invalid_source_id"},
		{name: "unknown", sourceID: "999999", wantStatus: http.StatusNotFound, wantCode: "source_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/sources/"+test.sourceID+"/identities", nil)
			response := httptest.NewRecorder()
			fixture.server.Router().ServeHTTP(response, request)
			assertions.Equal(test.wantStatus, response.Code, response.Body.String())
			var body ErrorResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assertions.Equal(test.wantCode, body.Error)
		})
	}
}

func TestSourceIdentitiesOpenAPIContract(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, document := range []struct {
		name string
		doc  *huma.OpenAPI
	}{
		{name: "public", doc: OpenAPIDocument()},
		{name: "client", doc: openAPIClientDocument()},
	} {
		t.Run(document.name, func(t *testing.T) {
			path := document.doc.Paths["/api/v1/sources/{source_id}/identities"]
			requirements.NotNil(path)
			requirements.NotNil(path.Get)
			assertions.Equal("listSourceIdentities", path.Get.OperationID)
			requirements.Len(path.Get.Parameters, 1)
			parameter := path.Get.Parameters[0]
			assertions.Equal("source_id", parameter.Name)
			assertions.Equal("path", parameter.In)
			assertions.True(parameter.Required)
			requirements.NotNil(parameter.Schema)
			assertions.Equal("integer", parameter.Schema.Type)
			assertions.Equal("int64", parameter.Schema.Format)

			responseSchema := document.doc.Components.Schemas.Map()["SourceIdentitiesResponse"]
			requirements.NotNil(responseSchema)
			assertions.Contains(responseSchema.Required, "identities")
			identities := responseSchema.Properties["identities"]
			requirements.NotNil(identities)
			assertions.Equal("array", identities.Type)
			assertions.False(identities.Nullable)

			identitySchema := document.doc.Components.Schemas.Map()["SourceIdentityResponse"]
			requirements.NotNil(identitySchema)
			assertions.Contains(identitySchema.Required, "signals")
			signals := identitySchema.Properties["signals"]
			requirements.NotNil(signals)
			assertions.Equal("array", signals.Type)
			assertions.False(signals.Nullable)
		})
	}
}

func getSourceIdentities(t *testing.T, server *Server, sourceID int64) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sources/"+strconv.FormatInt(sourceID, 10)+"/identities", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	return response
}
