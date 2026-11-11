package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
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
		assessment.ReasonCodes = append(assessment.ReasonCodes, "provider_unavailable")
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
		assert.Equal([]string{"Room;3", "Floor 2"}, assessment.OriginalExtendedParts)
		assert.Equal([]string{"Room;3", "Floor 2"}, assessment.ProposedExtendedParts)
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

func TestPostalAddressCorrectionPreservesRawDraft(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	raw := postaladdress.Input{AddressKind: "postal", PostOfficeBox: " Box 9 ", ExtendedAddress: " Unit 7 ", StreetAddress: " 12 Example st\n extra ", Locality: " Example City ", Region: " BC ", PostalCode: " V0V 0V0 ", CountryName: " Canada ", CountryCode: " ca ", ExtendedComponents: `["raw"]`, FreeText: " 自由 ", Label: " old label ", GeoURI: "geo:0,0", Timezone: "America/Vancouver", PlaceURI: "urn:example:place", OriginalValue: "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"original_value":"literal raw text"}}`}
	patch := map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "street_address": "12 Example Street", "extended_address": "Unit 7", "country_code": "CA", "original_input": raw, "envelope": map[string]any{"source": "user", "type_label": "home", "ordinal": 2}}}}}
	body, err := json.Marshal(patch)
	require := require.New(t)
	require.NoError(err)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	etag := read.Header().Get("ETag")
	saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": etag})
	require.Equal(http.StatusOK, saved.Code, saved.Body.String())
	var profile store.PersonProfile
	require.NoError(json.Unmarshal(saved.Body.Bytes(), &profile))
	require.Len(profile.Addresses, 1)
	address := profile.Addresses[0]
	require.NotNil(address.Assessment)
	assert := assert.New(t)
	assert.Equal(raw, address.Assessment.Original)
	assert.Equal(raw.OriginalValue, address.OriginalValue, "public reads expose original text, not the storage envelope")
	forbidden := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), []byte(`{"addresses":{"add":[{"address_kind":"postal","street_address":"x","assessment":{"status":"validated"},"envelope":{"source":"user"}}]}}`), map[string]string{"If-Match": saved.Header().Get("ETag")})
	assert.Equal(http.StatusBadRequest, forbidden.Code)
}

func TestPostalAddressExternalDeniesDelegatedAndReadOnlyKeys(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	_, wrapped := newIdentityLinkTestServer(t)
	srv.store = wrapped
	id := seedAPIPerson(t, wrapped.Store)
	srv.cfg.People.AddressValidation = config.PostalAddressConfig{Enabled: true, Provider: "google"}
	srv.cfg.Server.RemoteClients = []config.RemoteClientConfig{{ClientID: "reader", APIKey: "synthetic-reader-key"}}
	validator := &countingPostalValidator{}
	srv.SetPostalAddressValidator(validator)
	_, secret, _, err := reg.Issue("postal-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "alice@example.com"}}, time.Time{})
	require.NoError(t, err)
	for _, tc := range []struct {
		headers map[string]string
		status  int
	}{{map[string]string{apiprotocol.AgentTokenHeader: secret}, http.StatusUnauthorized}, {map[string]string{"X-Api-Key": "synthetic-reader-key"}, http.StatusForbidden}} {
		result := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), []byte(`{"address":{"address_kind":"postal","street_address":"12 Example St","country_code":"CA"},"allow_external":true}`), tc.headers)
		assert.Equal(t, tc.status, result.Code, result.Body.String())
	}
	assert.Zero(t, validator.calls)
}

func TestPostalAddressEditPreservesOriginalHistory(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	raw := postaladdress.Input{AddressKind: "postal", StreetAddress: " imported street ", ExtendedAddress: " Unit 7 ", CountryCode: "ca", ExtendedComponents: `;;;;;;;Room\;3;Floor 2`, OriginalValue: " original line\n raw "}
	street, unit, country, source, uid, label := "Old street", "Unit 7", "CA", "import:example", "synthetic-card", "home"
	oldText, newText := "  First raw address\nUnit 7  ", "  Updated current address\nUnit 8  "
	pref, ordinal := 1, 3
	encoded, err := postaladdress.EncodeOriginalInput(raw)
	require := require.New(t)
	require.NoError(err)
	old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: &street, ExtendedAddress: &unit, CountryCode: &country, FreeText: &oldText, OriginalValue: encoded, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport, SourceRef: &source, SourceResourceUID: &uid, TypeLabel: &label, Pref: &pref, Ordinal: &ordinal, VCard: store.VCardIdentity{Property: "ADR", PID: []string{"1.1"}}}})
	require.NoError(err)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	var before store.PersonProfile
	require.NoError(json.Unmarshal(read.Body.Bytes(), &before))
	body, err := json.Marshal(map[string]any{"addresses": map[string]any{"supersede": []int64{old.Envelope.ID}, "add": []any{map[string]any{"address_kind": "postal", "street_address": "Corrected street", "extended_address": unit, "country_code": country, "free_text": newText, "original_input": raw, "envelope": map[string]any{"source": "user", "type_label": label, "pref": pref, "ordinal": ordinal}}}}})
	require.NoError(err)
	saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, saved.Code, saved.Body.String())
	var after store.PersonProfile
	require.NoError(json.Unmarshal(saved.Body.Bytes(), &after))
	require.Len(after.Addresses, 1)
	current := after.Addresses[0]
	assert := assert.New(t)
	assert.Equal(&newText, current.FreeText)
	reread := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	var persisted store.PersonProfile
	require.NoError(json.Unmarshal(reread.Body.Bytes(), &persisted))
	require.Len(persisted.Addresses, 1)
	assert.Equal(&newText, persisted.Addresses[0].FreeText)
	assert.NotEqual(before.Addresses[0].Assessment.Proposed.StreetAddress, current.Assessment.Proposed.StreetAddress)
	assert.Equal(store.ProvenanceUser, current.Envelope.Source)
	assert.Nil(current.Envelope.SourceResourceUID)
	assert.True(current.Envelope.VCard.IsZero())
	assert.Equal(&pref, current.Envelope.Pref)
	assert.Equal(ordinal, current.Envelope.Ordinal)
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
	assert.Equal(old.Envelope.Source, historic.Envelope.Source)
	assert.Equal(old.Envelope.SourceRef, historic.Envelope.SourceRef)
	assert.Equal(old.Envelope.SourceResourceUID, historic.Envelope.SourceResourceUID)
	assert.Equal(old.Envelope.VCard, historic.Envelope.VCard)
	assert.Equal(raw, historic.Assessment.Original)
	assert.Equal([]string{"Room;3", "Floor 2"}, historic.Assessment.OriginalExtendedParts)
	assert.Equal(raw.OriginalValue, historic.OriginalValue)
}

func TestPostalAddressAssessmentRequiresPresentAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
	}{
		{"missing", `{}`, http.StatusBadRequest},
		{"missing with external consent", `{"allow_external":true}`, http.StatusBadRequest},
		{"null", `{"address":null}`, http.StatusBadRequest},
		{"null with external consent", `{"address":null,"allow_external":true}`, http.StatusBadRequest},
		{"unknown member", `{"address":{},"unknown":true}`, http.StatusBadRequest},
		{"multiple values", `{"address":{}} {}`, http.StatusBadRequest},
		{"present empty object", `{"address":{}}`, http.StatusOK},
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
					Error string `json:"error"`
				}
				require.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
				assert.Equal("bad_request", failure.Error)
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

func TestPostalAddressAssessmentExplainsUnknownField(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	response := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), []byte(`{"address":{"postal_cod":"12345"}}`), nil)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
	assert.Contains(t, failure.Message, "postal_cod")
	assert.Contains(t, failure.Message, "unknown")
}

func TestPostalReplacementRequiresCurrentValue(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("Old street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","free_text":"","original_input":{"address_kind":"postal","street_address":"Old street"},"envelope":{"source":"user"}}]}}`, old.Envelope.ID)
	saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
	assert.Equal(t, http.StatusBadRequest, saved.Code, saved.Body.String())
	profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, profile.Addresses, 1)
	assert.Equal(t, old.Envelope.ID, profile.Addresses[0].Envelope.ID)
	after := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	assert.Equal(t, read.Header().Get("ETag"), after.Header().Get("ETag"))
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

func TestLegacyPostalAssessmentMatchesPublicAddress(t *testing.T) {
	for _, tc := range []struct {
		name, original string
		structured     bool
	}{
		{"structured", `;;12 Example Street;Example City;;;;Room\;3;Floor 2`, true},
		{"text", "12 Example Street", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, OriginalValue: tc.original, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			require.NoError(t, err)
			for _, path := range []string{personProfilePath(id), personProfilePath(id) + "/history"} {
				response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var profile struct {
					Addresses []store.PersonAddress `json:"addresses"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &profile))
				require.Len(t, profile.Addresses, 1)
				address := profile.Addresses[0]
				require.NotNil(t, address.Assessment)
				if tc.structured {
					require.NotNil(t, address.StreetAddress)
					assert.Equal(t, "12 Example Street", *address.StreetAddress)
					assert.Equal(t, "12 Example Street", address.Assessment.Proposed.StreetAddress)
					assert.NotContains(t, address.Assessment.ReasonCodes, "address_incomplete")
					assert.Equal(t, []string{"Room;3", "Floor 2"}, address.Assessment.ProposedExtendedParts)
				} else {
					require.NotNil(t, address.FreeText)
					assert.Equal(t, "12 Example Street", *address.FreeText)
				}
			}
			if !tc.structured {
				read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
				body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","free_text":"12 Example Street","country_name":"Canada","original_input":{"address_kind":"postal","original_value":"12 Example Street"},"envelope":{"source":"user"}}]}}`, old.Envelope.ID)
				saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
				require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
				var profile store.PersonProfile
				require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &profile))
				require.Len(t, profile.Addresses, 1)
				require.NotNil(t, profile.Addresses[0].FreeText)
				assert.Equal(t, "12 Example Street", *profile.Addresses[0].FreeText)
				assert.Equal(t, "Canada", *profile.Addresses[0].CountryName)
			}
		})
	}
}
