package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonNetworkRouteReturnsProjection(t *testing.T) {
	require := require.New(t)
	srv, s := newPersonNetworkTestServer(t)
	root := mustAPIPerson(t, s, "root@example.test", "Root")
	peer := mustAPIPerson(t, s, "peer@example.test", "Peer")
	_, err := s.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
		SourcePersonID: root.ID,
		TargetPersonID: peer.ID,
		TypeSlug:       "friend",
		Source:         store.ProvenanceUser,
		Actor:          "test",
	})
	require.NoError(err)

	got := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/network?depth=1", root.ID), nil, nil)
	require.Equal(http.StatusOK, got.Code)
	var body store.PersonNetwork
	require.NoError(json.NewDecoder(got.Body).Decode(&body))
	assert.Equal(t, root.ID, body.RootPersonID)
}

func newPersonNetworkTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newOrganizationTestServerWithStore(t)
}

func TestPersonNetworkRouteRejectsInvalidDepth(t *testing.T) {
	srv, s := newPersonNetworkTestServer(t)
	root := mustAPIPerson(t, s, "root@example.test", "Root")

	got := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/network?depth=4", root.ID), nil, nil)
	assert.Equal(t, http.StatusBadRequest, got.Code)
	assert.Contains(t, got.Body.String(), "invalid_depth")
}

func TestPersonNetworkRouteReturnsNotFoundForMissingRoot(t *testing.T) {
	srv, _ := newPersonNetworkTestServer(t)

	got := doRequest(t, srv.Router(), http.MethodGet, "/api/v1/people/999/network?depth=1", nil, nil)
	assert.Equal(t, http.StatusNotFound, got.Code)
	assert.Contains(t, got.Body.String(), "person_profile_not_found")
}

func TestPersonNetworkOpenAPIDocumentsDepthBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		operation := document.Paths["/api/v1/people/{id}/network"].Get
		require.NotNil(operation)
		depth := personNetworkDepthParameter(t, operation.Parameters)
		require.NotNil(depth.Schema)
		require.NotNil(depth.Schema.Minimum)
		require.NotNil(depth.Schema.Maximum)
		assert.Equal(1, depth.Schema.Default)
		assert.InDelta(1.0, *depth.Schema.Minimum, 0)
		assert.InDelta(3.0, *depth.Schema.Maximum, 0)
	}
}

func personNetworkDepthParameter(t *testing.T, parameters []*huma.Param) *huma.Param {
	t.Helper()
	for _, parameter := range parameters {
		if parameter.Name == "depth" {
			return parameter
		}
	}
	require.Fail(t, "depth parameter is not documented")
	return nil
}
