package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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

func TestPatchPersonProfileAcceptsServiceLessEmailAndRejectsUnknownService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)

	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	plainEmail := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
		[]byte(`{"contact_points":{"add":[{"address_kind":"email","original_value":"plain@example.test","envelope":{"source":"user"}}]}}`),
		map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, plainEmail.Code, plainEmail.Body.String())
	var profile store.PersonProfile
	require.NoError(json.Unmarshal(plainEmail.Body.Bytes(), &profile), plainEmail.Body.String())
	var added *store.PersonContactPoint
	for index := range profile.ContactPoints {
		if profile.ContactPoints[index].OriginalValue == "plain@example.test" {
			added = &profile.ContactPoints[index]
			break
		}
	}
	require.NotNil(added)
	assert.Nil(added.ServiceSlug)

	unknownService := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
		[]byte(`{"contact_points":{"add":[{"address_kind":"email","service_slug":"not-a-real-service","original_value":"scoped@example.test","envelope":{"source":"user"}}]}}`),
		map[string]string{"If-Match": plainEmail.Header().Get("ETag")})
	assert.Equal(http.StatusBadRequest, unknownService.Code, unknownService.Body.String())
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

func TestPatchPersonProfileAcceptsFallbackBackedOptionalFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)

	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	body := []byte(`{
		"names":{"add":[{"name_kind":"formatted","formatted":"Alice Example","envelope":{"source":"user"}}]},
		"addresses":{"add":[{"address_kind":"postal","free_text":"Exampleville","envelope":{"source":"user"}}]},
		"dates":{"add":[{"date_kind":"custom","date_text":"Spring 2020","envelope":{"source":"user"}}]},
		"media":{"add":[{"media_kind":"photo","uri":"https://example.org/alice.jpg","envelope":{"source":"user"}}]}
	}`)
	response := doRequest(t, server, http.MethodPatch, personProfilePath(personID), body,
		map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	var profile store.PersonProfile
	require.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
	require.Len(profile.Names, 1)
	assert.Equal("Alice Example", profile.Names[0].OriginalValue)
	require.Len(profile.Addresses, 1)
	assert.Equal("Exampleville", profile.Addresses[0].OriginalValue)
	require.Len(profile.Dates, 1)
	assert.Equal("Spring 2020", profile.Dates[0].OriginalValue)
	require.Len(profile.Media, 1)
	assert.Equal("https://example.org/alice.jpg", profile.Media[0].OriginalValue)
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
		{"provider identity is observation-only", `{"contact_points":{"add":[{"address_kind":"provider_identity","original_value":"provider:key","envelope":{"source":"user"}}]}}`},
		{"unknown service", `{"contact_points":{"add":[{"address_kind":"username","service_slug":"no-such","original_value":"alice","envelope":{"source":"user"}}]}}`},
		{"invalid partial date", `{"dates":{"add":[{"date_kind":"birthday","date":{"year":1985,"month":2,"day":30},"original_value":"1985-02-30","envelope":{"source":"user"}}]}}`},
		{"close before active", `{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"user","active_from":"2026-08-08T12:00:00Z","active_until":"2026-08-08T11:00:00Z"}}]}}`},
		{"confidence on declared value", `{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"user","confidence":0.5}}]}}`},
		{"negative ordinal", `{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"user","ordinal":-1}}]}}`},
		{"empty patch", `{}`},
	}
	for _, tc := range cases {
		recorder := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
			[]byte(tc.body), map[string]string{"If-Match": etag})
		assert.Equal(http.StatusBadRequest, recorder.Code, "%s: %s", tc.name, recorder.Body.String())
	}
}

func TestPatchPersonProfilePreservesExplicitZeroOrdinal(t *testing.T) {
	require := require.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)
	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())

	response := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
		[]byte(`{"contact_points":{"add":[{"address_kind":"email","original_value":"pinned@example.com","envelope":{"source":"user","ordinal":0}}]}}`),
		map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var profile store.PersonProfile
	require.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
	for _, point := range profile.ContactPoints {
		if point.OriginalValue == "pinned@example.com" {
			assert.Equal(t, 0, point.Envelope.Ordinal)
			return
		}
	}
	require.Fail("patched contact point was not returned")
}

func TestPatchPersonProfileRejectsResponseOnlyEnvelopeFields(t *testing.T) {
	responseOnlyFields := []struct {
		name  string
		value string
	}{
		{name: "id", value: `123`},
		{name: "created_at", value: `"2026-08-08T12:00:00Z"`},
		{name: "updated_at", value: `"2026-08-08T12:00:00Z"`},
		{name: "superseded_at", value: `"2026-08-08T12:00:00Z"`},
	}
	for _, field := range responseOnlyFields {
		t.Run(field.name, func(t *testing.T) {
			server, st := newProfileTestServer(t)
			personID := seedAPIPerson(t, st)
			read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
			require.Equal(t, http.StatusOK, read.Code, read.Body.String())

			body := fmt.Sprintf(
				`{"categories":{"add":[{"original_value":"Friends","envelope":{"source":"user",%q:%s}}]}}`,
				field.name, field.value,
			)
			response := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
				[]byte(body), map[string]string{"If-Match": read.Header().Get("ETag")})
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}
}

func TestPatchPersonProfileRejectsOversizedBody(t *testing.T) {
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)
	read := doRequest(t, server, http.MethodGet, personProfilePath(personID), nil, nil)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())

	response := doRequest(t, server, http.MethodPatch, personProfilePath(personID),
		bytes.Repeat([]byte(" "), MaxPersonProfilePatchBytes+1),
		map[string]string{"If-Match": read.Header().Get("ETag")})
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
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
	missing := doRequest(t, server, http.MethodGet, "/api/v1/people/999999/profile", nil, nil)
	assert.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
	bad := doRequest(t, server, http.MethodGet, "/api/v1/people/0/profile", nil, nil)
	assert.Equal(http.StatusBadRequest, bad.Code, bad.Body.String())
}

func TestGetPersonProfileMediaContentReturnsStoredBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)
	payload := []byte("synthetic-profile-photo")
	media, err := st.AddPersonMediaContext(t.Context(), personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto,
		MediaType: new("image/png"),
		Data:      payload,
		Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	response := doRequest(t, server, http.MethodGet,
		personProfileMediaContentPath(personID, media.Envelope.ID), nil, nil,
	)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(payload, response.Body.Bytes())
	assert.Equal("image/png", response.Header().Get("Content-Type"))
	assert.Equal(strconv.Itoa(len(payload)), response.Header().Get("Content-Length"))
	assert.Equal("attachment", response.Header().Get("Content-Disposition"))
	assert.Equal("nosniff", response.Header().Get("X-Content-Type-Options"))
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
}

func TestGetPersonProfileMediaContentRejectsMissingAndURIOnlyValues(t *testing.T) {
	assert := assert.New(t)
	server, st := newProfileTestServer(t)
	personID := seedAPIPerson(t, st)
	media, err := st.AddPersonMediaContext(t.Context(), personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto,
		URI:       new("https://example.com/alice.png"),
		Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)

	for _, path := range []string{
		personProfileMediaContentPath(personID, media.Envelope.ID),
		personProfileMediaContentPath(personID, media.Envelope.ID+1),
		personProfileMediaContentPath(personID+1, media.Envelope.ID),
		fmt.Sprintf("/api/v1/people/%d/profile/media/0/content", personID),
	} {
		response := doRequest(t, server, http.MethodGet, path, nil, nil)
		if path == fmt.Sprintf("/api/v1/people/%d/profile/media/0/content", personID) {
			assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
		} else {
			assert.Equal(http.StatusNotFound, response.Code, response.Body.String())
		}
	}
}

func TestGetPersonProfileMediaContentRequiresAuthentication(t *testing.T) {
	const apiKey = "profile-media-test-key"
	st := testutil.NewTestStore(t)
	wrapped := &stubIdentityCacheStore{Store: st}
	server := NewServer(&config.Config{Server: config.ServerConfig{
		APIPort: 8080,
		APIKey:  apiKey,
	}}, wrapped, nil, testLogger()).Router()
	personID := seedAPIPerson(t, st)
	media, err := st.AddPersonMediaContext(t.Context(), personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto,
		Data:      []byte("authenticated-profile-photo"),
		Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	path := personProfileMediaContentPath(personID, media.Envelope.ID)

	unauthorized := doRequest(t, server, http.MethodGet, path, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code, unauthorized.Body.String())
	authorized := doRequest(t, server, http.MethodGet, path, nil,
		map[string]string{"X-Api-Key": apiKey},
	)
	assert.Equal(t, http.StatusOK, authorized.Code, authorized.Body.String())
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
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	return person.ID
}

func personProfilePath(personID int64) string {
	return fmt.Sprintf("/api/v1/people/%d/profile", personID)
}

func personProfileMediaContentPath(personID, mediaID int64) string {
	return fmt.Sprintf(
		"/api/v1/people/%d/profile/media/%d/content", personID, mediaID,
	)
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
