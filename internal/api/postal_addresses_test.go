package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/postaladdress"
	"go.kenn.io/msgvault/internal/store"
)

type countingPostalValidator struct {
	calls       int
	unavailable bool
	previousID  string
}

func (v *countingPostalValidator) Check(_ context.Context, input postaladdress.Input, previousResponseID string) (postaladdress.Assessment, error) {
	v.calls++
	v.previousID = previousResponseID
	assessment := postaladdress.Assess(input)
	assessment.Provider = "google"
	assessment.Status = postaladdress.StatusValidated
	assessment.Proposed.StreetAddress = "12 Example Street"
	if v.unavailable {
		assessment.Status = postaladdress.StatusUnavailable
		assessment.ReasonCodes = append(assessment.ReasonCodes, "provider_request_failed")
		return assessment, postaladdress.ErrProviderUnavailable
	}
	return assessment, nil
}

func postalAssessPath(id int64) string { return fmt.Sprintf("/api/v1/people/%d/addresses/assess", id) }

func TestPostalAddressAssessmentRequiresExplicitConsent(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	srv.cfg.People.AddressValidation = config.PostalAddressConfig{Enabled: true, Provider: "google"}
	validator := &countingPostalValidator{}
	srv.SetPostalAddressValidator(validator)
	require := require.New(t)
	assert := assert.New(t)
	for _, external := range []bool{false, true} {
		body, err := json.Marshal(map[string]any{"address": postaladdress.Input{AddressKind: "postal", StreetAddress: " 12 Example St ", ExtendedAddress: "Unit 7", CountryCode: "ca", OriginalValue: "raw", ExtendedComponents: `;;12 Example St;;;;;Room\;3;Floor 2`}, "allow_external": external, "previous_response_id": "first-google-id"})
		require.NoError(err)
		response := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), body, nil)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		assert.Equal("no-store", response.Header().Get("Cache-Control"))
		var assessment postaladdress.Assessment
		require.NoError(json.Unmarshal(response.Body.Bytes(), &assessment))
		assert.Equal("raw", assessment.Original.OriginalValue)
		if external {
			assert.Equal(postaladdress.StatusValidated, assessment.Status)
			assert.Equal(1, validator.calls)
			assert.Equal("first-google-id", validator.previousID)
		} else {
			assert.Equal(postaladdress.StatusNormalizedUnverified, assessment.Status)
			assert.Zero(validator.calls)
		}
	}
	profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
	require.NoError(err)
	assert.Empty(profile.Addresses, "an assessment must never persist provider output")
}

func TestPostalAddressUnavailableStates(t *testing.T) {
	for _, state := range []string{"disabled", "failure"} {
		t.Run(state, func(t *testing.T) {
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			srv.cfg.People.AddressValidation = config.PostalAddressConfig{Enabled: state != "disabled", Provider: "google"}
			validator := &countingPostalValidator{unavailable: true}
			srv.SetPostalAddressValidator(validator)
			body := []byte(`{"address":{"address_kind":"postal","street_address":"12 Example St","extended_address":"Unit 7","country_code":"CA","extended_components":";;;;;;;Room\\;3;Floor 2"},"allow_external":true}`)
			result := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), body, nil)
			require := require.New(t)
			require.Equal(http.StatusOK, result.Code, result.Body.String())
			var assessment postaladdress.Assessment
			require.NoError(json.Unmarshal(result.Body.Bytes(), &assessment))
			assert := assert.New(t)
			assert.Equal(postaladdress.StatusUnavailable, assessment.Status)
			if state == "failure" {
				assert.Equal(1, validator.calls)
			} else {
				assert.Zero(validator.calls)
			}
		})
	}
}

func TestPostalAddressEditPreservesOriginalHistory(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	raw := postaladdress.Input{AddressKind: "postal", PostOfficeBox: " Box 9 ", ExtendedAddress: " Unit 7 ", StreetAddress: " imported street ", Locality: " Example City ", Region: " BC ", PostalCode: " V0V 0V0 ", CountryName: " Canada ", CountryCode: "ca", ExtendedComponents: `;;;;;;;Room\;3;Floor 2`, FreeText: " 自由 ", Label: " old label ", GeoURI: "geo:0,0", Timezone: "America/Vancouver", PlaceURI: "urn:example:place", OriginalValue: " original line\n raw "}
	street, unit, country, label := "Old street", "Unit 7", "CA", "home"
	oldText, newText := "  First raw address\nUnit 7  ", "  Updated current address\nUnit 8  "
	pref, ordinal := 1, 3
	encoded, err := postaladdress.EncodeOriginalInput(raw)
	require := require.New(t)
	require.NoError(err)
	old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: &street, ExtendedAddress: &unit, CountryCode: &country, FreeText: &oldText, OriginalValue: encoded, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser, TypeLabel: &label, Pref: &pref, Ordinal: &ordinal}})
	require.NoError(err)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	body, err := json.Marshal(map[string]any{"addresses": map[string]any{"supersede": []int64{old.Envelope.ID}, "add": []any{map[string]any{"address_kind": "postal", "street_address": "Corrected street", "extended_address": unit, "country_code": "ca", "free_text": newText, "original_input": raw, "envelope": map[string]any{"source": "user", "type_label": label, "pref": pref, "ordinal": ordinal}}}}})
	require.NoError(err)
	saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, saved.Code, saved.Body.String())
	var after store.PersonProfile
	require.NoError(json.Unmarshal(saved.Body.Bytes(), &after))
	require.Len(after.Addresses, 1)
	current := after.Addresses[0]
	assert := assert.New(t)
	assert.Equal(&newText, current.FreeText)
	assert.Equal(new("CA"), current.CountryCode)
	assert.Equal(raw, current.Assessment.Original)
	historyResponse := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id)+"/history", nil, nil)
	require.Equal(http.StatusOK, historyResponse.Code)
	var history store.PersonProfileHistory
	require.NoError(json.Unmarshal(historyResponse.Body.Bytes(), &history))
	require.Len(history.Addresses, 2)
	var historic *store.PersonAddress
	for i := range history.Addresses {
		if history.Addresses[i].Envelope.ID == old.Envelope.ID {
			historic = &history.Addresses[i]
		}
	}
	require.NotNil(historic)
	assert.Equal(&oldText, historic.FreeText)
	assert.NotNil(historic.Envelope.SupersededAt)
	assert.Equal(raw, historic.Assessment.Original)
	assert.Equal([]string{"Room;3", "Floor 2"}, historic.Assessment.OriginalExtendedParts)
	assert.Equal(raw.OriginalValue, historic.OriginalValue)
}

func TestPostalAddressAssessmentRequiresPresentAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
		message  string
	}{
		{"missing with external consent", `{"allow_external":true}`, http.StatusBadRequest, ""},
		{"unknown member", `{"address":{},"unknown":true}`, http.StatusBadRequest, ""},
		{"unknown address field", `{"address":{"postal_cod":"12345"}}`, http.StatusBadRequest, "postal_cod"},
		{"multiple values", `{"address":{}} {}`, http.StatusBadRequest, ""},
		{"present empty object", `{"address":{}}`, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			srv.cfg.People.AddressValidation = config.PostalAddressConfig{Enabled: true, Provider: "google"}
			validator := &countingPostalValidator{}
			srv.SetPostalAddressValidator(validator)
			response := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), []byte(tc.body), nil)
			require.Equal(tc.wantCode, response.Code, response.Body.String())
			assert.Equal("no-store", response.Header().Get("Cache-Control"))
			assert.Zero(validator.calls)
			if tc.wantCode == http.StatusBadRequest {
				var failure struct {
					Error   string `json:"error"`
					Message string `json:"message"`
				}
				require.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
				assert.Equal("bad_request", failure.Error)
				if tc.message != "" {
					assert.Contains(failure.Message, tc.message)
					assert.Contains(failure.Message, "unknown")
				}
			} else {
				var assessment postaladdress.Assessment
				require.NoError(json.Unmarshal(response.Body.Bytes(), &assessment))
				assert.Equal("postal", assessment.Original.AddressKind)
				assert.Equal(postaladdress.StatusNeedsReview, assessment.Status)
			}
			profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
			require.NoError(err)
			assert.Empty(profile.Addresses, "assessment requests must not store addresses")
		})
	}
}

func TestPostalReplacementRequiresCurrentValue(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("Old street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(err)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","free_text":"","original_input":{"address_kind":"postal","street_address":"Old street"},"envelope":{"source":"user"}}]}}`, old.Envelope.ID)
	saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
	assert.Equal(http.StatusBadRequest, saved.Code, saved.Body.String())
	profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
	require.NoError(err)
	require.Len(profile.Addresses, 1)
	assert.Equal(old.Envelope.ID, profile.Addresses[0].Envelope.ID)
	after := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	assert.Equal(read.Header().Get("ETag"), after.Header().Get("ETag"))
}

func TestPostalPatchExplainsInvalidOriginalInput(t *testing.T) {
	for _, tc := range []struct{ body, explanation string }{
		{`{"addresses":{"add":[{"address_kind":"birth_place","original_input":{"address_kind":"postal"},"envelope":{"source":"user"}}]}}`, "original_input is only available for postal addresses"},
		{`{"addresses":{"add":[{"address_kind":"postal","original_input":{"address_kind":"birth_place"},"envelope":{"source":"user"}}]}}`, "original_input must be postal"},
	} {
		t.Run(tc.explanation, func(t *testing.T) {
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			response := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), []byte(tc.body), map[string]string{"If-Match": read.Header().Get("ETag")})
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), tc.explanation)
		})
	}
}
