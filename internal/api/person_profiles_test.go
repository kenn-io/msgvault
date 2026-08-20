package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonProfileHTTPPromoteListGetUpdateAndConflictingLink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	bob := st.mustParticipant(t, "bob@example.com", "bob", "example.com")

	createdResponse := personRequest(t, srv, http.MethodPost, peoplePath,
		fmt.Appendf(nil, `{"participant_id":%d}`, alice), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Person
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.Equal([]int64{alice}, created.ParticipantIDs)
	assert.NotEmpty(created.VCardUID)
	etag := createdResponse.Header().Get("ETag")
	assert.NotEmpty(etag)

	repromotedResponse := personRequest(t, srv, http.MethodPost, peoplePath,
		fmt.Appendf(nil, `{"participant_id":%d}`, alice), "")
	require.Equal(http.StatusOK, repromotedResponse.Code)
	var repromoted store.Person
	require.NoError(json.Unmarshal(repromotedResponse.Body.Bytes(), &repromoted))
	assert.Equal(created.ID, repromoted.ID)

	listResponse := personRequest(t, srv, http.MethodGet, peoplePath, nil, "")
	require.Equal(http.StatusOK, listResponse.Code)
	var listed PeopleResponse
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.People, 1)
	assert.Equal(created.ID, listed.People[0].ID)

	getResponse := personRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", peoplePath, created.ID), nil, "")
	require.Equal(http.StatusOK, getResponse.Code)
	assert.Equal(etag, getResponse.Header().Get("ETag"))

	updatedResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", peoplePath, created.ID),
		[]byte(`{"display_name":"alice"}`), etag)
	require.Equal(http.StatusOK, updatedResponse.Code)
	var updated store.Person
	require.NoError(json.Unmarshal(updatedResponse.Body.Bytes(), &updated))
	require.NotNil(updated.DisplayName)
	assert.Equal("alice", *updated.DisplayName)

	clearedResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", peoplePath, created.ID),
		[]byte(`{"display_name":null}`), updatedResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, clearedResponse.Code)
	var cleared store.Person
	require.NoError(json.Unmarshal(clearedResponse.Body.Bytes(), &cleared))
	assert.Nil(cleared.DisplayName)

	staleResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", peoplePath, created.ID),
		[]byte(`{"display_name":"alice stale"}`), etag)
	assert.Equal(http.StatusConflict, staleResponse.Code)

	_, _, err := st.CreatePersonFromParticipant(bob)
	require.NoError(err)
	linkResponse := postIdentityLink(t, srv, "/api/v1/identity/links",
		IdentityLinkRequest{ParticipantA: alice, ParticipantB: bob})
	assert.Equal(http.StatusConflict, linkResponse.Code)
}

func TestPersonProfileHTTPDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	person, _, err := st.CreatePersonFromParticipant(alice)
	require.NoError(err)
	path := fmt.Sprintf("%s/%d", peoplePath, person.ID)
	etag := fmt.Sprintf(`"person-%d-r%d"`, person.ID, person.Revision)

	missingIfMatch := personRequest(t, srv, http.MethodDelete, path, nil, "")
	assert.Equal(http.StatusPreconditionRequired, missingIfMatch.Code)

	stale := personRequest(t, srv, http.MethodDelete, path, nil,
		fmt.Sprintf(`"person-%d-r%d"`, person.ID, person.Revision+7))
	assert.Equal(http.StatusConflict, stale.Code)

	deleted := personRequest(t, srv, http.MethodDelete, path, nil, etag)
	require.Equal(http.StatusNoContent, deleted.Code)
	assert.Empty(deleted.Body.Bytes())

	gone := personRequest(t, srv, http.MethodGet, path, nil, "")
	assert.Equal(http.StatusNotFound, gone.Code)
	deletedAgain := personRequest(t, srv, http.MethodDelete, path, nil, etag)
	assert.Equal(http.StatusNotFound, deletedAgain.Code)
}

func TestPersonPatchSchemaRequiresNullableDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		schema := document.Components.Schemas.Map()["PatchPersonRequest"]
		require.NotNil(schema)
		assert.Contains(schema.Required, "display_name")
		require.Contains(schema.Properties, "display_name")
		assert.True(schema.Properties["display_name"].Nullable)
	}
}

func personRequest(
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
