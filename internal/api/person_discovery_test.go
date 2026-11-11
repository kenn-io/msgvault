package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonDiscoveryHTTPRejectsNegativeScopeBeforeSearch(t *testing.T) {
	assert := assert.New(t)
	engine := &fakePersonSearchEngine{}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")
	response := semanticPersonSearchRequest(t, srv, []byte(`{"query":"equipment","organization_id":-1}`), "")
	assert.Equal(http.StatusBadRequest, response.Code)
	assert.Empty(engine.queries)
}

func TestPersonDiscoveryHTTPDirectoryForwardsOrganizationID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	createDirectoryHTTPPerson(t, st, "Alex Example", "outsider@example.test", "colleague", "Other Cooperative", true)
	organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Cooperative", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	response := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/directory?organization_id=%d", peoplePath, organization.ID), nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assertDirectoryPeopleResponseIDs(t, response, []int64{}...)
	for _, scope := range []string{"-1", "bad"} {
		invalid := personRequest(t, srv, http.MethodGet, peoplePath+"/directory?organization_id="+scope, nil, "")
		assert.Equal(http.StatusBadRequest, invalid.Code)
	}
}
