package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/personsearch"
)

type scopedPersonDiscoveryEngine struct {
	fakePersonSearchEngine

	organizationID int64
}

func (e *scopedPersonDiscoveryEngine) SearchInOrganization(ctx context.Context, query string, limit int, organizationID int64) ([]personsearch.Result, error) {
	e.organizationID = organizationID
	return e.Search(ctx, query, limit)
}

// Ignoring organization_id must never turn a company search into a global search.
func TestPersonDiscoveryHTTPDispatchesExactOrganizationScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := &scopedPersonDiscoveryEngine{}
	engine.results = []personsearch.Result{{Person: store.Person{ID: 7, VCardUID: "person-example-7"}, Score: .9}}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")
	response := semanticPersonSearchRequest(t, srv, []byte(`{"query":"equipment","limit":2,"organization_id":13}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(int64(13), engine.organizationID)
	assert.Contains(response.Body.String(), "person-example-7")
}

func TestPersonDiscoveryHTTPNeverFallsBackToUnscopedEngine(t *testing.T) {
	assert := assert.New(t)
	engine := &fakePersonSearchEngine{}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")
	response := semanticPersonSearchRequest(t, srv, []byte(`{"query":"equipment","organization_id":13}`), "")
	assert.Equal(http.StatusServiceUnavailable, response.Code)
	assert.Contains(response.Body.String(), "person_discovery_scope_unsupported")
	assert.Empty(engine.queries)
}

func TestPersonDiscoveryHTTPRejectsNegativeScopeBeforeSearch(t *testing.T) {
	assert := assert.New(t)
	engine := &scopedPersonDiscoveryEngine{}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")
	response := semanticPersonSearchRequest(t, srv, []byte(`{"query":"equipment","organization_id":-1}`), "")
	assert.Equal(http.StatusBadRequest, response.Code)
	assert.Empty(engine.queries)
}

func TestPersonDiscoveryHTTPDirectoryUsesCanonicalIDBeforePaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	// Create the outsider first so it sorts before the member in the name tie.
	createDirectoryHTTPPerson(t, st, "Alex Example", "outsider@example.test", "colleague", "Other Cooperative", true)
	person := createDirectoryHTTPPerson(t, st, "Alex Example", "scoped@example.test", "colleague", "Example Cooperative", true)
	organizations, err := st.ListOrganizationsContext(t.Context(), store.OrganizationFilter{Query: "Example Cooperative"})
	require.NoError(err)
	require.Len(organizations, 1)
	response := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/directory?organization_id=%d&limit=1", peoplePath, organizations[0].ID), nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assertDirectoryPeopleResponseIDs(t, response, person.ID)
	for _, scope := range []string{"-1", "bad"} {
		invalid := personRequest(t, srv, http.MethodGet, peoplePath+"/directory?organization_id="+scope, nil, "")
		assert.Equal(http.StatusBadRequest, invalid.Code)
	}
}
