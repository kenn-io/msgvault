package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestOrganizationHTTPMapsProfileOrdinalValidationToBadRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	profilePath := fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID)

	negativeOrdinal := organizationRequest(t, srv, http.MethodPut, profilePath, []byte(`{
		"names":[{"name":"Example Alias","name_kind":"alias","ordinal":-1,"source":"user"}]
	}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, negativeOrdinal.Code)
	assert.Contains(negativeOrdinal.Body.String(), "invalid_organization")
}

func TestOrganizationHTTPMapsIncompleteScopeValidationToBadRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	profilePath := fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID)
	incompleteScope := organizationRequest(t, srv, http.MethodPut, profilePath, []byte(`{
		"contact_points":[{"contact_kind":"username","scope_kind":"workspace","original_value":"alice","source":"user"}]
	}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusBadRequest, incompleteScope.Code)
	assert.Contains(incompleteScope.Body.String(), "invalid_organization")
}

func TestEmploymentHTTPMapsMergedOrganizationToBadRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newOrganizationTestServerWithStore(t)
	person := mustAPIPerson(t, st, "alice@example.com", "alice")
	survivor := mustAPIOrganization(t, st, "Survivor Org")
	losing := mustAPIOrganization(t, st, "Losing Org")
	_, err := st.MergeOrganizationsContext(context.Background(),
		survivor.ID, survivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)

	response := organizationRequest(t, srv, http.MethodPost, employmentsPath,
		fmt.Appendf(nil, `{"person_id":%d,"organization_id":%d,"title":"Engineer","source":"user"}`,
			person.ID, losing.ID), "")
	require.Equal(http.StatusBadRequest, response.Code)
	assert.Contains(response.Body.String(), "invalid_organization")
}
