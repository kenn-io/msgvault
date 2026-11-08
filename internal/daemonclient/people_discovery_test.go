package daemonclient

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonDiscoveryClientRejectsUnsupportedScopeBeforeRequests(t *testing.T) {
	for _, version := range []string{"", "3.48.0"} {
		t.Run(version, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests int
			people := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					writePeopleBrowserJSON(t, w, http.StatusOK, `{"status":"ok","api_schema_version":"`+version+`"}`)
					return
				}
				requests++
				writePeopleBrowserJSON(t, w, http.StatusOK, `{"results":[],"people":[],"organizations":[]}`)
			}))
			_, err := people.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{OrganizationID: 13})
			require.ErrorContains(err, "3.49.0")
			_, err = people.SearchSemanticPeople(t.Context(), peoplebrowser.SemanticPeopleRequest{Query: "equipment", OrganizationID: 13})
			require.ErrorContains(err, "3.49.0")
			_, err = people.ListOrganizations(t.Context(), "Example", 2, 0)
			require.ErrorContains(err, "3.49.0")
			assert.Zero(requests, "an old daemon must never receive a possibly ignored organization scope")
		})
	}
}

func TestPersonDiscoveryClientProjectsIdentitiesAndPreservesScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	people := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"status":"ok","api_schema_version":"3.49.0"}`)
		case "/api/v1/people/search":
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			assert.InDelta(13, body["organization_id"], 0)
			assert.Equal("equipment", body["query"])
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"results":[{"person":{"id":7,"vcard_uid":"person-7","display_name":"Alex Example","revision":1,"participant_ids":[],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},"score":0.9}]}`)
		case "/api/v1/organizations":
			assert.Equal("Coop", r.URL.Query().Get("q"))
			assert.Equal("1", r.URL.Query().Get("offset"))
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"organizations":[{"id":13,"name":"Example Cooperative","vcard_uid":"organization-13","description":"private excluded detail","name_normalized":"example cooperative","kind":"company","revision":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],"total":3,"limit":1,"offset":1}`)
		default:
			assert.Fail("unexpected request", "%s", r.URL.Path)
		}
	}))
	page, err := people.SearchSemanticPeople(t.Context(), peoplebrowser.SemanticPeopleRequest{Query: "equipment", OrganizationID: 13, Limit: 2})
	require.NoError(err)
	require.Len(page.Results, 1)
	assert.Equal(int64(7), page.Results[0].PersonID)
	assert.Equal("person-7", page.Results[0].VCardUID)
	organizations, err := people.ListOrganizations(t.Context(), "Coop", 1, 1)
	require.NoError(err)
	assert.True(organizations.HasMore)
	encoded, err := json.Marshal(organizations)
	require.NoError(err)
	assert.NotContains(string(encoded), "private excluded detail")
	assert.Contains(string(encoded), "Example Cooperative")
}
