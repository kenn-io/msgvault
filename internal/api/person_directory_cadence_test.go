package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryPeopleHTTPCadenceContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	p := createDirectoryHTTPPerson(t, st, "Cadence Example", "cadence-http@example.test", "friend", "Example Co", false)
	for _, suffix := range []string{"?cadence=no_list", "?sort=next_due", "?sort=most_overdue"} {
		response := personRequest(t, srv, http.MethodGet, peoplePath+"/directory"+suffix, nil, "")
		require.Equal(http.StatusOK, response.Code)
		var page DirectoryPeopleResponse
		require.NoError(json.Unmarshal(response.Body.Bytes(), &page))
		require.Len(page.People, 1)
		assert.Equal(p.ID, page.People[0].ID)
		assert.Len(page.CadencePresets, 4)
		require.NotNil(page.CadenceCoverage)
		assert.Equal(1, page.CadenceCoverage.NoList)
	}
	due := personRequest(t, srv, http.MethodGet, peoplePath+"/directory?cadence=due&due_within_days=7", nil, "")
	require.Equal(http.StatusOK, due.Code)
	var page DirectoryPeopleResponse
	require.NoError(json.Unmarshal(due.Body.Bytes(), &page))
	assert.Empty(page.People)
	for _, suffix := range []string{"?cadence=bad", "?cadence=due&due_within_days=-1", "?cadence=due&due_within_days=3661", "?due_within_days=0", "?cadence=due&due_within_days=nope", "?cadence=due&due_within_days="} {
		response := personRequest(t, srv, http.MethodGet, peoplePath+"/directory"+suffix, nil, "")
		assert.Equal(http.StatusBadRequest, response.Code, suffix)
	}
}

func TestContactListHTTPExpectAbsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	p := createDirectoryHTTPPerson(t, st, "First List Example", "first-list@example.test", "friend", "Example Co", false)
	path := fmt.Sprintf("%s/%d/attributes/contact_frequency", peoplePath, p.ID)
	body := []byte(`{"value":{"type":"integer","integer":21},"expected_value_id":0}`)
	first := personRequest(t, srv, http.MethodPut, path, body, "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	next := personRequest(t, srv, http.MethodPut, path, body, "")
	assert.Equal(http.StatusConflict, next.Code, next.Body.String())
}
