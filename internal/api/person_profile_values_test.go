package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestGetPersonProfileReturnsTypedValuesAndETag(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)

	recorder := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, recorder.Code, recorder.Body.String())
	assert.NotEmpty(recorder.Header().Get("ETag"))
	assert.Equal("no-store", recorder.Header().Get("Cache-Control"))

	var profile store.PersonProfile
	require.NoError(json.Unmarshal(recorder.Body.Bytes(), &profile), recorder.Body.String())
	require.Len(profile.ContactPoints, 1)
	assert.Equal(store.ContactAddressEmail, profile.ContactPoints[0].AddressKind)
	assert.Equal("alice@example.com", profile.ContactPoints[0].NormalizedValue)
	assert.Equal("Alice@Example.com", profile.ContactPoints[0].OriginalValue)
}

func TestPatchPersonProfileRoundTripsPartialDatesAndAddresses(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)

	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	etag := read.Header().Get("ETag")

	body := []byte(`{
		"dates": {"add": [{
			"date_kind": "birthday",
			"date": {"month": 4, "day": 12},
			"original_value": "--0412",
			"envelope": {"source": "user"}
		}]},
		"addresses": {"add": [{
			"address_kind": "postal",
			"street_address": "123 Example St.",
			"locality": "Exampleville",
			"postal_code": "90000",
			"country_code": "US",
			"geo_uri": "geo:37.386,-122.084",
			"original_value": ";;123 Example St.;Exampleville;;90000;",
			"envelope": {"source": "user", "pref": 1}
		}]}
	}`)
	recorder := doRequest(t, server, http.MethodPatch, personProfilePath(personID), body,
		map[string]string{"If-Match": etag})
	require.Equal(http.StatusOK, recorder.Code, recorder.Body.String())

	var profile store.PersonProfile
	require.NoError(json.Unmarshal(recorder.Body.Bytes(), &profile), recorder.Body.String())
	require.Len(profile.Dates, 1)
	require.NotNil(profile.Dates[0].Date.Month)
	assert.Equal(4, *profile.Dates[0].Date.Month)
	assert.Nil(profile.Dates[0].Date.Year, "an absent year must stay absent, not become zero")
	require.Len(profile.Addresses, 1)
	assert.Equal("123 Example St.", *profile.Addresses[0].StreetAddress)
	assert.Equal("geo:37.386,-122.084", *profile.Addresses[0].GeoURI)
	assert.NotEqual(etag, recorder.Header().Get("ETag"), "the revision advanced")
}

func TestPatchPersonProfileRequiresIfMatchAndRejectsStaleRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)

	body := []byte(`{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"user"}}]}}`)
	missing := doRequest(t, server, http.MethodPatch, personProfilePath(personID), body, nil)
	assert.Equal(http.StatusPreconditionRequired, missing.Code, missing.Body.String())

	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	etag := read.Header().Get("ETag")
	first := doRequest(t, server, http.MethodPatch, personProfilePath(personID), body,
		map[string]string{"If-Match": etag})
	require.Equal(http.StatusOK, first.Code, first.Body.String())

	stale := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
		[]byte(`{"categories":{"add":[{"original_value":"Book Club","envelope":{"source":"user"}}]}}`),
		map[string]string{"If-Match": etag})
	assert.Equal(http.StatusConflict, stale.Code, stale.Body.String())
}

func TestPatchPersonProfileMapsValidationErrorsToBadRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)
	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	etag := read.Header().Get("ETag")

	cases := []struct {
		name string
		body string
	}{
		{"unknown provenance", `{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"beeper"}}]}}`},
		{"scope required", `{"contact_points":{"add":[{"address_kind":"username","service_slug":"slack","original_value":"alice","envelope":{"source":"user"}}]}}`},
		{"unknown service", `{"contact_points":{"add":[{"address_kind":"username","service_slug":"no-such","original_value":"alice","envelope":{"source":"user"}}]}}`},
		{"invalid partial date", `{"dates":{"add":[{"date_kind":"birthday","date":{"year":1985,"month":2,"day":30},"original_value":"1985-02-30","envelope":{"source":"user"}}]}}`},
		{"confidence on declared value", `{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"user","confidence":0.5}}]}}`},
		{"empty patch", `{}`},
	}
	for _, tc := range cases {
		recorder := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
			[]byte(tc.body), map[string]string{"If-Match": etag})
		assert.Equal(http.StatusBadRequest, recorder.Code, "%s: %s", tc.name, recorder.Body.String())
	}
}

func TestGetPersonProfileHistoryIsASeparateEndpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)

	recorder := doRequest(t, server, http.MethodGet, personProfilePath(personID)+"/history", nil, nil)
	require.Equal(http.StatusOK, recorder.Code, recorder.Body.String())
	var history store.PersonProfileHistory
	require.NoError(json.Unmarshal(recorder.Body.Bytes(), &history), recorder.Body.String())
	assert.Equal(personID, history.Person.ID)
	assert.NotNil(history.Observations, "the observations field is always present, even when empty")
}

func TestProfileEndpointsRejectUnknownPersonAndBadID(t *testing.T) {
	assert := assert.New(t)
	server, _ := newProfileTestServer(t)
	missing := doRequest(t, server, http.MethodGet, "/api/v1/persons/999999/profile", nil, nil)
	assert.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
	bad := doRequest(t, server, http.MethodGet, "/api/v1/persons/0/profile", nil, nil)
	assert.Equal(http.StatusBadRequest, bad.Code, bad.Body.String())
}

func newProfileTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	server, wrapped := newIdentityLinkTestServer(t)
	return server.Router(), wrapped.Store
}

func seedAPIPerson(t *testing.T, st *store.Store) int64 {
	t.Helper()
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "alice@example.com", "Alice Example",
	)
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind:   store.ContactAddressEmail,
		OriginalValue: "Alice@Example.com",
		Envelope:      store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	return person.ID
}

func personProfilePath(personID int64) string {
	return fmt.Sprintf("/api/v1/persons/%d/profile", personID)
}

func doRequest(
	t *testing.T,
	handler http.Handler,
	method, path string,
	body []byte,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
