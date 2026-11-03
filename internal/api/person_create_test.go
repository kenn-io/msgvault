package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestStandalonePersonCreate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	srv, st := newIdentityLinkTestServer(t)
	body := []byte(`{"name":"Alex Example","emails":[{"value":"Alex@example.com","type":"work"}],"phones":[{"value":"+12025550123","type":"cell"}],"org":"Example Company","title":"Engineer","address":"123 Example Street","note":"Met at a conference"}`)
	response := personRequest(t, srv, http.MethodPost, peoplePath+"/create", body, "")
	require.Equal(http.StatusCreated, response.Code, response.Body.String())
	var person store.Person
	require.NoError(json.Unmarshal(response.Body.Bytes(), &person))
	assert.Equal("Alex Example", *person.DisplayName)
	assert.Empty(person.ParticipantIDs)
	assert.NotEmpty(person.VCardUID)
	points, err := st.ListPersonContactPointsContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.Len(points, 2)
	assert.Equal(store.ProvenanceUser, points[0].Envelope.Source)
	duplicate := personRequest(t, srv, http.MethodPost, peoplePath+"/create", body, "")
	assert.Equal(http.StatusConflict, duplicate.Code, duplicate.Body.String())
	people, err := st.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Len(people, 1)
}

func TestStandalonePersonValidationAndObservedDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	srv, st := newIdentityLinkTestServer(t)
	st.mustParticipant(t, "existing@example.com", "Existing Example", "example.com")
	for _, body := range []string{
		`{}`, `{"name":" "}`, `{"name":"Alex","emails":[{"value":"invalid"}]}`,
		`{"name":"Alex","phones":[{"value":"bad"}]}`, `{"name":"Alex","unexpected":true}`,
		`{"name":"Alex","title":"Engineer"}`,
		`{"name":"Alex","emails":[{"value":"alex@example.com","type":"x y"}]}`,
		`{"name":"Alex","source":"extraction"}`,
		`{"name":"Alex","org":"Example Company","title":"` + strings.Repeat("t", 281) + `"}`,
	} {
		response := personRequest(t, srv, http.MethodPost, peoplePath+"/create", []byte(body), "")
		assert.Equal(http.StatusBadRequest, response.Code, body+response.Body.String())
	}
	response := personRequest(t, srv, http.MethodPost, peoplePath+"/create", []byte(`{"name":"Alex","emails":[{"value":"EXISTING@example.com"}]}`), "")
	assert.Equal(http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "promote")
	people, err := st.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Empty(people)
}

func TestStandalonePersonContactLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		emails int
		phones int
		status int
	}{
		{"both at limit", 200, 200, http.StatusCreated},
		{"too many emails", 201, 0, http.StatusBadRequest},
		{"too many phones", 0, 201, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, st := newIdentityLinkTestServer(t)
			input := store.PersonCreateInput{Name: "Contact Limits Example"}
			for i := range tc.emails {
				input.Emails = append(input.Emails, store.PersonCreateContact{Value: fmt.Sprintf("contact-%d@example.com", i)})
			}
			for i := range tc.phones {
				area := []int{202, 212, 415}[i/100]
				input.Phones = append(input.Phones, store.PersonCreateContact{Value: fmt.Sprintf("+1%d55501%02d", area, i%100)})
			}
			body, err := json.Marshal(input)
			require.NoError(err)
			response := personRequest(t, srv, http.MethodPost, peoplePath+"/create", body, "")
			require.Equal(tc.status, response.Code, response.Body.String())
			people, err := st.ListPersonsContext(t.Context())
			require.NoError(err)
			if tc.status == http.StatusBadRequest {
				assert.Empty(people)
				return
			}
			require.Len(people, 1)
			points, err := st.ListPersonContactPointsContext(t.Context(), people[0].ID, true)
			require.NoError(err)
			assert.Len(points, 400)
		})
	}
}

func TestStandalonePersonEnrichmentSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	srv, st := newIdentityLinkTestServer(t)
	body := []byte(`{"name":"Alex Example","org":"Example Company","note":"Met at a conference","source":"enrichment"}`)
	response := personRequest(t, srv, http.MethodPost, peoplePath+"/create", body, "")
	require.Equal(http.StatusCreated, response.Code, response.Body.String())
	var person store.Person
	require.NoError(json.Unmarshal(response.Body.Bytes(), &person))
	notes, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugNotes})
	require.NoError(err)
	require.Len(notes, 1)
	assert.Equal(store.ProvenanceEnrichment, notes[0].Source)
}
