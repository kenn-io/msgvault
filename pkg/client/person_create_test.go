package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestCreateStandalonePersonAcceptsBothSuccessStatuses(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ensureStatus := "created"
			if status == http.StatusOK {
				ensureStatus = "matched_existing"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/people/create", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"id":7,"vcard_uid":"person-7","revision":2,"participant_ids":[],"created_at":"2026-11-10T12:00:00Z","updated_at":"2026-11-10T12:00:00Z","ensure":{"status":%q,"match_basis":[],"unapplied_contacts":[]}}`, ensureStatus)
			}))
			t.Cleanup(server.Close)
			c, err := New(server.URL)
			require.NoError(t, err)
			got, err := c.CreateStandalonePerson(t.Context(), &generated.CreateStandalonePersonRequestOptions{
				Body: &generated.CreateStandalonePersonBody{Name: "Alex Example", IfExists: new(generated.PersonCreateInputIfExists("return"))},
			})
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, int64(7), got.ID)
			require.NotNil(t, got.Ensure)
			assert.Equal(t, ensureStatus, string(got.Ensure.Status))
		})
	}
}

func TestCreateStandalonePersonPreservesConflictDetails(t *testing.T) {
	for _, test := range []struct{ code, details string }{
		{"person_ambiguous", `{"candidates":[{"person_id":7,"participant_id":null,"name":"Alex Example"},{"person_id":null,"participant_id":9,"name":"Blair Example"}],"match_basis":[{"kind":"email","value":"alex@example.test","matched":"contact_point","person_id":7,"participant_id":null}]}`},
		{"person_matches_participant", `{"participant_id":9,"match_basis":[{"kind":"email","value":"alex@example.test","matched":"participant","person_id":null,"participant_id":9}],"next":{"cli":["msgvault","person","promote","9","--json"],"mcp":{"tool":"promote_person","arguments":{"participant_id":9}}}}`},
	} {
		t.Run(test.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, `{"error":%q,"message":"Resolve these matches","details":%s}`, test.code, test.details)
			}))
			t.Cleanup(server.Close)
			c, err := New(server.URL)
			require.NoError(t, err)
			response, err := c.CreateStandalonePersonWithResponse(t.Context(), &generated.CreateStandalonePersonRequestOptions{
				Body: &generated.CreateStandalonePersonBody{Name: "Alex Example", IfExists: new(generated.PersonCreateInputIfExists("return"))},
			})
			require.Error(t, err)
			require.NotNil(t, response)
			assert.Equal(t, http.StatusConflict, response.StatusCode)
			require.NotNil(t, response.JSON409)
			assert.Equal(t, test.code, response.JSON409.ErrorData)
			got, err := json.Marshal(response.JSON409.Details)
			require.NoError(t, err)
			assert.JSONEq(t, test.details, string(got))
		})
	}
}
