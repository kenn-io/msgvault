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

func TestPersonDiscoveryClientRequiresSupportedScope(t *testing.T) {
	for _, version := range []string{"5.0.0", "5.1.0"} {
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
				switch r.URL.Path {
				case "/api/v1/people/directory":
					assert.Equal("13", r.URL.Query().Get("organization_id"))
				case "/api/v1/people/search":
					var body map[string]any
					require.NoError(json.NewDecoder(r.Body).Decode(&body))
					assert.Equal("equipment", body["query"])
					assert.Equal(float64(13), body["organization_id"])
					assert.Equal(float64(3), body["limit"])
				case "/api/v1/organizations":
					assert.Equal("Example", r.URL.Query().Get("q"))
					assert.Equal("2", r.URL.Query().Get("limit"))
					assert.Equal("1", r.URL.Query().Get("offset"))
				default:
					assert.Fail("unexpected discovery request", r.URL.Path)
				}
				writePeopleBrowserJSON(t, w, http.StatusOK, `{"results":[],"people":[],"organizations":[]}`)
			}))
			_, directoryErr := people.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{OrganizationID: 13})
			_, searchErr := people.SearchSemanticPeople(t.Context(), peoplebrowser.SemanticPeopleRequest{Query: "equipment", OrganizationID: 13, Limit: 3})
			_, organizationErr := people.ListOrganizations(t.Context(), "Example", 2, 1)
			if version == "5.0.0" {
				require.ErrorContains(directoryErr, "5.1.0")
				require.ErrorContains(searchErr, "5.1.0")
				require.ErrorContains(organizationErr, "5.1.0")
				assert.Zero(requests)
			} else {
				require.NoError(directoryErr)
				require.NoError(searchErr)
				require.NoError(organizationErr)
				assert.Equal(3, requests)
			}
		})
	}
}
