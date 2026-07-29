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

func TestPersonProfileHTTPPromoteListGetUpdateAndConflictingLink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	bob := st.mustParticipant(t, "bob@example.com", "bob", "example.com")

	createdResponse := personRequest(t, srv, http.MethodPost, personsPath,
		fmt.Appendf(nil, `{"participant_id":%d}`, alice), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Person
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.Equal([]int64{alice}, created.ParticipantIDs)
	assert.NotEmpty(created.VCardUID)
	etag := createdResponse.Header().Get("ETag")
	assert.NotEmpty(etag)

	listResponse := personRequest(t, srv, http.MethodGet, personsPath, nil, "")
	require.Equal(http.StatusOK, listResponse.Code)
	var listed PersonsResponse
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.Persons, 1)
	assert.Equal(created.ID, listed.Persons[0].ID)

	getResponse := personRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", personsPath, created.ID), nil, "")
	require.Equal(http.StatusOK, getResponse.Code)
	assert.Equal(etag, getResponse.Header().Get("ETag"))

	updatedResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", personsPath, created.ID),
		[]byte(`{"display_name":"alice"}`), etag)
	require.Equal(http.StatusOK, updatedResponse.Code)
	var updated store.Person
	require.NoError(json.Unmarshal(updatedResponse.Body.Bytes(), &updated))
	require.NotNil(updated.DisplayName)
	assert.Equal("alice", *updated.DisplayName)

	staleResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", personsPath, created.ID),
		[]byte(`{"display_name":"alice stale"}`), etag)
	assert.Equal(http.StatusConflict, staleResponse.Code)

	_, err := st.CreatePersonFromParticipant(bob)
	require.NoError(err)
	linkResponse := postIdentityLink(t, srv, "/api/v1/identity/links",
		IdentityLinkRequest{ParticipantA: alice, ParticipantB: bob})
	assert.Equal(http.StatusConflict, linkResponse.Code)
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
