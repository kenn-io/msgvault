package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListCommunicationServicesReturnsSeededCatalogWithAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, _ := newProfileTestServer(t)

	recorder := doRequest(t, server, http.MethodGet, "/api/v1/communication-services", nil, nil)
	require.Equal(http.StatusOK, recorder.Code, recorder.Body.String())
	var response CommunicationServicesResponse
	require.NoError(json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	assert.GreaterOrEqual(len(response.Services), 24)

	aliasesBySlug := make(map[string][]string, len(response.Services))
	for _, service := range response.Services {
		aliasesBySlug[service.Slug] = service.Aliases
	}
	assert.Contains(aliasesBySlug["x"], "twitter")
	assert.Contains(aliasesBySlug["bluesky"], "bsky")
	assert.Contains(aliasesBySlug["google-messages"], "gmessages")
}

func TestCreateCommunicationServiceRegistersUnknownBridge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, _ := newProfileTestServer(t)
	body := []byte(`{
		"slug":"example-bridge",
		"display_label":"Example Bridge",
		"aliases":["examplebridge"],
		"scope_policy":"optional",
		"default_scope_kind":"account",
		"normalization":"lower"
	}`)
	created := doRequest(t, server, http.MethodPost, "/api/v1/communication-services", body, nil)
	require.Equal(http.StatusCreated, created.Code, created.Body.String())
	assert.NotEmpty(created.Header().Get("Location"))

	again := doRequest(t, server, http.MethodPost, "/api/v1/communication-services", body, nil)
	assert.Equal(http.StatusOK, again.Code, "re-registering the same slug is idempotent")
	listed := doRequest(t, server, http.MethodGet, "/api/v1/communication-services", nil, nil)
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	assert.Contains(listed.Body.String(), "example-bridge")
}

func TestCreateCommunicationServiceValidatesInput(t *testing.T) {
	assert := assert.New(t)
	server, _ := newProfileTestServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"invalid slug", `{"slug":"Example Bridge","display_label":"X","scope_policy":"none","normalization":"lower"}`, http.StatusBadRequest},
		{"invalid scope policy", `{"slug":"example","display_label":"X","scope_policy":"sometimes","normalization":"lower"}`, http.StatusBadRequest},
		{"invalid normalization", `{"slug":"example","display_label":"X","scope_policy":"none","normalization":"soundex"}`, http.StatusBadRequest},
		{"alias belongs to another service", `{"slug":"example","display_label":"X","aliases":["twitter"],"scope_policy":"none","normalization":"lower"}`, http.StatusConflict},
	}
	for _, tc := range cases {
		recorder := doRequest(t, server, http.MethodPost, "/api/v1/communication-services",
			[]byte(tc.body), nil)
		assert.Equal(tc.want, recorder.Code, "%s: %s", tc.name, recorder.Body.String())
	}
}
