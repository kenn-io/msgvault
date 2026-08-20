package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTrackingHTTPGetsAndReplacesState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "tracking@example.test", "Tracking", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	path := fmt.Sprintf("/api/v1/people/%d/tracking", person.ID)

	response := personRequest(t, srv, http.MethodGet, path, nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var state store.PersonTracking
	require.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	assert.Equal(person.ID, state.PersonID)
	assert.False(state.Tracked)
	assert.Nil(state.TrackedAt)

	response = personRequest(t, srv, http.MethodPut, path,
		[]byte(`{"tracked":true}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	assert.True(state.Tracked)
	assert.NotNil(state.TrackedAt)

	response = personRequest(t, srv, http.MethodPut, path,
		[]byte(`{"tracked":false}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NoError(json.Unmarshal(response.Body.Bytes(), &state))
	assert.False(state.Tracked)
	assert.Nil(state.TrackedAt)
}

func TestPersonTrackingHTTPValidatesPersonIDAndExistence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)

	invalid := personRequest(t, srv, http.MethodGet,
		"/api/v1/people/0/tracking", nil, "")
	assert.Equal(http.StatusBadRequest, invalid.Code)
	assert.Contains(invalid.Body.String(), "invalid_person_id")

	missing := personRequest(t, srv, http.MethodPut,
		"/api/v1/people/999999/tracking", []byte(`{"tracked":true}`), "")
	assert.Equal(http.StatusNotFound, missing.Code)
	assert.Contains(missing.Body.String(), "person_profile_not_found")

	participant := st.mustParticipant(t, "null-tracking@example.test", "Null", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	path := fmt.Sprintf("/api/v1/people/%d/tracking", person.ID)
	tracked := personRequest(t, srv, http.MethodPut, path,
		[]byte(`{"tracked":true}`), "")
	require.Equal(http.StatusOK, tracked.Code, tracked.Body.String())

	nullValue := personRequest(t, srv, http.MethodPut, path,
		[]byte(`{"tracked":null}`), "")
	assert.Equal(http.StatusBadRequest, nullValue.Code)
	assert.Contains(nullValue.Body.String(), "bad_request")

	unchanged := personRequest(t, srv, http.MethodGet, path, nil, "")
	require.Equal(http.StatusOK, unchanged.Code, unchanged.Body.String())
	assert.Contains(unchanged.Body.String(), `"tracked":true`)
}

func TestPersonTrackingOpenAPIContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := OpenAPIDocument().Paths["/api/v1/people/{id}/tracking"]
	require.NotNil(path)
	require.NotNil(path.Get)
	require.NotNil(path.Put)
	assert.Equal("getPersonTracking", path.Get.OperationID)
	assert.Equal("setPersonTracking", path.Put.OperationID)
}
