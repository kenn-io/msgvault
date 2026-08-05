package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func attributeRequest(
	t *testing.T, srv *Server, method, path string, body []byte, ifMatch string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	return response
}

func TestAttributeDefinitionsHTTPListsSeedsAndRejectsUniqueness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)

	response := attributeRequest(t, srv, http.MethodGet,
		"/api/v1/attribute-definitions?object_type=person", nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var listed AttributeDefinitionsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &listed))
	require.Len(listed.Definitions, 4)
	assert.NotContains(response.Body.String(), "is_unique")

	rejected := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", []byte(`{
			"object_type":"person","slug":"employee_number",
			"label":"Employee number","value_type":"text","field_type":"text",
			"is_unique":true
		}`), "")
	assert.Equal(http.StatusBadRequest, rejected.Code)
	assert.Contains(rejected.Body.String(), "attribute_uniqueness_unsupported")
}

func TestAttributeDefinitionsHTTPCreateRenameAndDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)

	createdResponse := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", []byte(`{
			"object_type":"person","slug":"favorite_tea","label":"Favorite tea",
			"value_type":"text","field_type":"select","cardinality":"multi",
			"options":{"choices":[{"value":"green","label":"Green"}]}
		}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code, createdResponse.Body.String())
	var created store.AttributeDefinition
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.NotEmpty(created.UniversalID)
	assert.Equal(store.AttributeOwnershipUser, created.Ownership)
	assert.Equal(fmt.Sprintf(`"attribute-definition-%d-r1"`, created.ID),
		createdResponse.Header().Get("ETag"))

	renamedResponse := attributeRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", created.ID),
		[]byte(`{"label":"Tea preferences"}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, renamedResponse.Code, renamedResponse.Body.String())
	var renamed store.AttributeDefinition
	require.NoError(json.Unmarshal(renamedResponse.Body.Bytes(), &renamed))
	assert.Equal("Tea preferences", renamed.Label)
	assert.Equal(created.UniversalID, renamed.UniversalID)
	assert.Equal(created.Slug, renamed.Slug)

	stale := attributeRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", created.ID),
		[]byte(`{"label":"Stale"}`), createdResponse.Header().Get("ETag"))
	assert.Equal(http.StatusConflict, stale.Code)

	removed := attributeRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", created.ID), nil,
		renamedResponse.Header().Get("ETag"))
	assert.Equal(http.StatusNoContent, removed.Code, removed.Body.String())
}

func TestAttributeDefinitionsHTTPProtectsSeedAndRejectsUnknownFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)

	seeded, err := st.GetAttributeDefinitionBySlugContext(t.Context(),
		store.AttributeObjectPerson, store.AttributeSlugPrimaryChannel)
	require.NoError(err)
	protected := attributeRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", seeded.ID), nil,
		attributeDefinitionETag(*seeded))
	assert.Equal(http.StatusConflict, protected.Code)

	unknown := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", []byte(`{
			"object_type":"person","slug":"scratch","label":"Scratch",
			"value_type":"text","field_type":"text","ownership":"system"
		}`), "")
	assert.Equal(http.StatusBadRequest, unknown.Code)
}
