package daemonclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCreatePersonIfExistsDaemonRoundTrip(t *testing.T) {
	for _, status := range []string{"created", "matched_existing"} {
		t.Run(status, func(t *testing.T) {
			engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/people/create", r.URL.Path)
				var body store.PersonCreateInput
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, store.PersonIfExistsReturn, body.IfExists)
				assert.Equal(t, store.ProvenanceEnrichment, body.Source)
				code, basis, unapplied := http.StatusCreated, `[]`, `[]`
				if status == "matched_existing" {
					code = http.StatusOK
					basis = `[{"kind":"email","value":"alex@example.test","matched":"contact_point","person_id":12,"participant_id":null}]`
					unapplied = `[{"kind":"phone","value":"+12025550123","type":"cell"}]`
				}
				writePeopleBrowserJSON(t, w, code, fmt.Sprintf(`{"id":12,"display_name":"Alex Example","vcard_uid":"person-12","revision":1,"participant_ids":[],"created_at":"2026-10-08T12:00:00Z","updated_at":"2026-10-08T12:00:00Z","ensure":{"status":%q,"match_basis":%s,"unapplied_contacts":%s}}`, status, basis, unapplied))
			}))
			person, err := engine.CreatePerson(t.Context(), store.PersonCreateInput{Name: "Alex Example", IfExists: store.PersonIfExistsReturn, Source: store.ProvenanceEnrichment, Emails: []store.PersonCreateContact{{Value: "alex@example.test"}}, Phones: []store.PersonCreateContact{{Value: "+12025550123"}}})
			require.NoError(t, err)
			require.NotNil(t, person.Ensure)
			assert.Equal(t, store.PersonEnsureStatus(status), person.Ensure.Status)
			if status == "created" {
				assert.Empty(t, person.Ensure.MatchBasis)
				assert.Empty(t, person.Ensure.UnappliedContacts)
			} else {
				assert.Equal(t, []store.PersonMatchBasis{{Kind: store.ContactAddressEmail, Value: "alex@example.test", Matched: "contact_point", PersonID: new(int64(12))}}, person.Ensure.MatchBasis)
				assert.Equal(t, []store.PersonUnappliedContact{{Kind: store.ContactAddressPhone, Value: "+12025550123", Type: "cell"}}, person.Ensure.UnappliedContacts)
			}
		})
	}
}

func TestCreatePersonIfExistsDaemonConflictDetails(t *testing.T) {
	for _, test := range []struct{ code, details string }{
		{"person_matches_participant", `{"participant_id":7,"match_basis":[{"kind":"email","value":"alex@example.test","matched":"participant","person_id":null,"participant_id":7}],"next":{"cli":["msgvault","person","promote","7","--json"],"mcp":{"tool":"promote_person","arguments":{"participant_id":7}}}}`},
		{"person_ambiguous", `{"candidates":[{"person_id":1,"participant_id":null,"name":"Alex Example"},{"person_id":null,"participant_id":7,"name":"Blair Example"}],"match_basis":[{"kind":"email","value":"alex@example.test","matched":"contact_point","person_id":1,"participant_id":null},{"kind":"phone","value":"+12025550123","matched":"participant","person_id":null,"participant_id":7}]}`},
	} {
		t.Run(test.code, func(t *testing.T) {
			engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writePeopleBrowserJSON(t, w, http.StatusConflict, fmt.Sprintf(`{"error":%q,"message":"Resolve matches","details":%s}`, test.code, test.details))
			}))
			_, err := engine.CreatePerson(t.Context(), store.PersonCreateInput{Name: "Alex Example", IfExists: store.PersonIfExistsReturn, Emails: []store.PersonCreateContact{{Value: "alex@example.test"}}, Phones: []store.PersonCreateContact{{Value: "+12025550123"}}})
			var conflict *APIError
			require.ErrorAs(t, err, &conflict)
			assert.Equal(t, test.code, conflict.Code)
			assert.JSONEq(t, test.details, string(conflict.Details))
		})
	}
}
