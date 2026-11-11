package daemonclient

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonDiscoveryClientRejectsUnsupportedScopeBeforeRequests(t *testing.T) {
	for _, version := range []string{"", "3.48.0", "3.49.0", "4.2.0", "5.0.0"} {
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
			require.ErrorContains(err, "5.1.0")
			_, err = people.SearchSemanticPeople(t.Context(), peoplebrowser.SemanticPeopleRequest{Query: "equipment", OrganizationID: 13})
			require.ErrorContains(err, "5.1.0")
			_, err = people.ListOrganizations(t.Context(), "Example", 2, 0)
			require.ErrorContains(err, "5.1.0")
			assert.Zero(requests, "an old daemon must never receive a possibly ignored organization scope")
		})
	}
}
