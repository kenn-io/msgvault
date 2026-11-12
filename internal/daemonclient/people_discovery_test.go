package daemonclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonDiscoveryClientRequiresSupportedScope(t *testing.T) {
	for _, version := range []string{"5.1.0", "5.2.0"} {
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
					var body struct {
						Query          string `json:"query"`
						OrganizationID int64  `json:"organization_id"`
						Limit          int    `json:"limit"`
					}
					raw, err := io.ReadAll(r.Body)
					if !assert.NoError(err) || !assert.NoError(json.Unmarshal(raw, &body)) {
						return
					}
					assert.Equal("equipment", body.Query)
					assert.Equal(int64(13), body.OrganizationID)
					assert.Equal(3, body.Limit)
					if version == "5.1.0" {
						var oldRequest struct {
							Query string `json:"query"`
							Limit int    `json:"limit"`
						}
						decoder := json.NewDecoder(bytes.NewReader(raw))
						decoder.DisallowUnknownFields()
						err := decoder.Decode(&oldRequest)
						if !assert.Error(err) {
							return
						}
						writePeopleBrowserJSON(t, w, http.StatusBadRequest, fmt.Sprintf(`{"error":"bad_request","message":%q}`, "Invalid person request: "+err.Error()))
						return
					}
					writePeopleBrowserJSON(t, w, http.StatusOK, `{"results":[{"person":{"id":7,"vcard_uid":"person-7","display_name":null,"revision":1,"participant_ids":[],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},"score":0.9}]}`)
					return
				case "/api/v1/organizations":
					assert.Equal("Example", r.URL.Query().Get("q"))
					assert.Equal("2", r.URL.Query().Get("limit"))
					assert.Equal("1", r.URL.Query().Get("offset"))
					writePeopleBrowserJSON(t, w, http.StatusOK, `{"organizations":[{"id":13,"name":"Example Cooperative","vcard_uid":"organization-13","name_normalized":"example cooperative","kind":"company","revision":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],"total":3,"limit":2,"offset":1}`)
					return
				default:
					assert.Fail("unexpected discovery request", r.URL.Path)
				}
				writePeopleBrowserJSON(t, w, http.StatusOK, `{"results":[],"people":[],"organizations":[]}`)
			}))
			_, directoryErr := people.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{OrganizationID: 13})
			searchPage, searchErr := people.SearchSemanticPeople(t.Context(), peoplebrowser.SemanticPeopleRequest{Query: "equipment", OrganizationID: 13, Limit: 3})
			organizationPage, organizationErr := people.ListOrganizations(t.Context(), "Example", 2, 1)
			if version == "5.1.0" {
				require.ErrorContains(directoryErr, "5.2.0")
				var apiErr *APIError
				require.ErrorAs(searchErr, &apiErr)
				assert.Equal(http.StatusBadRequest, apiErr.Status)
				assert.Equal("bad_request", apiErr.Code)
				assert.Contains(apiErr.Message, "organization_id")
				require.ErrorContains(organizationErr, "5.2.0")
				assert.Equal(1, requests)
			} else {
				require.NoError(directoryErr)
				require.NoError(searchErr)
				require.NoError(organizationErr)
				require.Len(searchPage.Results, 1)
				assert.Equal(int64(7), searchPage.Results[0].PersonID)
				assert.Equal("person-7", searchPage.Results[0].VCardUID)
				assert.Nil(searchPage.Results[0].DisplayName)
				assert.InDelta(0.9, searchPage.Results[0].Score, 0.00001)
				assert.Equal([]peoplebrowser.OrganizationSummary{{ID: 13, Name: "Example Cooperative"}}, organizationPage.Organizations)
				assert.Equal(int64(3), organizationPage.Total)
				assert.Equal(2, organizationPage.Limit)
				assert.Equal(1, organizationPage.Offset)
				assert.True(organizationPage.HasMore)
				assert.Equal(3, requests)
			}
		})
	}
}
