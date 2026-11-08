package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
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
}

func TestPostalTextOnlyCreationKeepsCurrentTextSeparateFromSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	input := PersonAddressInputRequest{AddressKind: store.PersonAddressPostal, OriginalValue: "12 Example Street"}
	request := PersonProfilePatchRequest{Addresses: &PersonAddressPatchRequest{Add: []PersonAddressInputRequest{input}}}
	patch := store.PersonProfilePatch{Addresses: &store.PersonAddressPatch{Add: []store.PersonAddressInput{{AddressKind: store.PersonAddressPostal}}}}
	require.NoError(capturePostalPatchOriginals(request, &patch))
	require.NotNil(patch.Addresses.Add[0].FreeText)
	assert.Equal(input.OriginalValue, *patch.Addresses.Add[0].FreeText)
	raw, ok := postaladdress.DecodeOriginalInput(patch.Addresses.Add[0].OriginalValue)
	assert.True(ok)
	assert.Equal(input.OriginalValue, raw.OriginalValue)
}

func TestPostalTextOnlyReplacementDistinguishesNewTextFromOriginal(t *testing.T) {
	for _, tc := range []struct {
		name, current, original string
		wantText                bool
	}{
		{"new text", "New address", "Old address", true},
		{"preserved source", "Old address", "Old address", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			input := PersonAddressInputRequest{AddressKind: store.PersonAddressPostal, OriginalValue: tc.current, OriginalInput: &postaladdress.Input{AddressKind: "postal", OriginalValue: tc.original}}
			request := PersonProfilePatchRequest{Addresses: &PersonAddressPatchRequest{Add: []PersonAddressInputRequest{input}, Supersede: []int64{1}}}
			patch := store.PersonProfilePatch{Addresses: &store.PersonAddressPatch{Add: []store.PersonAddressInput{{AddressKind: store.PersonAddressPostal}}}}
			require.NoError(capturePostalPatchOriginals(request, &patch))
			if tc.wantText {
				require.NotNil(patch.Addresses.Add[0].FreeText)
				assert.Equal(tc.current, *patch.Addresses.Add[0].FreeText)
			} else {
				assert.Nil(patch.Addresses.Add[0].FreeText)
			}
		})
	}
}

func TestPostalExplicitEmptyCurrentTextIsNotRestored(t *testing.T) {
	for _, supersede := range [][]int64{nil, {1}} {
		input := PersonAddressInputRequest{AddressKind: store.PersonAddressPostal, OriginalValue: "Old address", FreeText: new("")}
		request := PersonProfilePatchRequest{Addresses: &PersonAddressPatchRequest{Add: []PersonAddressInputRequest{input}, Supersede: supersede}}
		patch := store.PersonProfilePatch{Addresses: &store.PersonAddressPatch{Add: []store.PersonAddressInput{{AddressKind: store.PersonAddressPostal, FreeText: new("")}}}}
		require.NoError(t, capturePostalPatchOriginals(request, &patch))
		require.NotNil(t, patch.Addresses.Add[0].FreeText)
		assert.Empty(t, *patch.Addresses.Add[0].FreeText)
	}
}

func (v *countingPostalValidator) Check(_ context.Context, input postaladdress.Input) (postaladdress.Assessment, error) {
	v.calls++
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
		body, err := json.Marshal(map[string]any{"address": postaladdress.Input{AddressKind: "postal", StreetAddress: " 12 Example St ", ExtendedAddress: "Unit 7", CountryCode: "ca", OriginalValue: "raw"}, "allow_external": external})
		require.NoError(err)
		response := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), body, nil)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		assert.Equal("no-store", response.Header().Get("Cache-Control"))
		var assessment postaladdress.Assessment
		require.NoError(json.Unmarshal(response.Body.Bytes(), &assessment))
		assert.Equal("raw", assessment.Original.OriginalValue)
		assert.Equal("Unit 7", assessment.Proposed.ExtendedAddress)
		if external {
			assert.Equal(postaladdress.StatusValidated, assessment.Status)
			assert.Equal(1, validator.calls)
		} else {
			assert.Equal(postaladdress.StatusNormalizedUnverified, assessment.Status)
			assert.Zero(validator.calls)
		}
	}
	profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
	require.NoError(err)
	assert.Empty(profile.Addresses, "an assessment must never persist provider output")
}

func TestPostalAddressUnavailableStillAllowsLocalSave(t *testing.T) {
	for _, state := range []string{"disabled", "no key", "failure"} {
		t.Run(state, func(t *testing.T) {
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			srv.cfg.People.AddressValidation = config.PostalAddressConfig{Enabled: state != "disabled", Provider: "google"}
			validator := &countingPostalValidator{unavailable: true}
			if state != "no key" {
				srv.SetPostalAddressValidator(validator)
			}
			body := []byte(`{"address":{"address_kind":"postal","street_address":"12 Example St","extended_address":"Unit 7","country_code":"CA"},"allow_external":true}`)
			result := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(id), body, nil)
			require := require.New(t)
			require.Equal(http.StatusOK, result.Code, result.Body.String())
			var assessment postaladdress.Assessment
			require.NoError(json.Unmarshal(result.Body.Bytes(), &assessment))
			assert := assert.New(t)
			assert.Equal(postaladdress.StatusUnavailable, assessment.Status)
			assert.Equal("Unit 7", assessment.Proposed.ExtendedAddress)
			if state == "failure" {
				assert.Equal(1, validator.calls)
			} else {
				assert.Zero(validator.calls)
			}
			read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), []byte(`{"addresses":{"add":[{"address_kind":"postal","street_address":"12 Example St","extended_address":"Unit 7","country_code":"CA","envelope":{"source":"user"}}]}}`), map[string]string{"If-Match": read.Header().Get("ETag")})
			require.Equal(http.StatusOK, saved.Code, saved.Body.String())
		})
	}
}

func TestPostalAddressCorrectionPreservesRawDraftAndConflict(t *testing.T) {
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
	assert.Equal("12 Example Street", *address.StreetAddress)
	stored, err := wrapped.ListPersonAddressesContext(t.Context(), id, false)
	require.NoError(err)
	decoded, ok := postaladdress.DecodeOriginalInput(stored[0].OriginalValue)
	require.True(ok)
	assert.Equal(raw, decoded)
	conflict := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": etag})
	assert.Equal(http.StatusConflict, conflict.Code, conflict.Body.String())
	stored, err = wrapped.ListPersonAddressesContext(t.Context(), id, false)
	require.NoError(err)
	assert.Len(stored, 1)
	forbidden := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), []byte(`{"addresses":{"add":[{"address_kind":"postal","street_address":"x","assessment":{"status":"validated"},"envelope":{"source":"user"}}]}}`), map[string]string{"If-Match": saved.Header().Get("ETag")})
	assert.Equal(http.StatusBadRequest, forbidden.Code)
}

func TestPostalAddressEmptyOriginalInputDoesNotPermitEmptyRow(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	response := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), []byte(`{"addresses":{"add":[{"address_kind":"postal","original_input":{"address_kind":"postal"},"envelope":{"source":"user"}}]}}`), map[string]string{"If-Match": read.Header().Get("ETag")})
	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, profile.Addresses)
}

func TestPostalAddressExternalDeniesDelegatedAndReadOnlyKeys(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	srv.cfg.People.AddressValidation = config.PostalAddressConfig{Enabled: true, Provider: "google"}
	srv.cfg.Server.RemoteClients = []config.RemoteClientConfig{{ClientID: "reader", APIKey: "synthetic-reader-key"}}
	validator := &countingPostalValidator{}
	srv.SetPostalAddressValidator(validator)
	_, secret, _, err := reg.Issue("postal-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "alice@example.com"}}, time.Time{})
	require.NoError(t, err)
	for _, headers := range []map[string]string{{apiprotocol.AgentTokenHeader: secret}, {"X-Api-Key": "synthetic-reader-key"}} {
		result := doRequest(t, srv.Router(), http.MethodPost, postalAssessPath(1), []byte(`{"address":{"address_kind":"postal","street_address":"12 Example St","country_code":"CA"},"allow_external":true}`), headers)
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, result.Code, result.Body.String())
	}
	assert.Zero(t, validator.calls)
}

func TestPostalAddressEditPreservesOriginalHistoryAndChangesFingerprint(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	raw := postaladdress.Input{AddressKind: "postal", StreetAddress: " imported street ", ExtendedAddress: " Unit 7 ", CountryCode: "ca", OriginalValue: " original line\n raw "}
	street, unit, country, source, uid, label := "Old street", "Unit 7", "CA", "import:example", "synthetic-card", "home"
	pref, ordinal := 1, 3
	encoded, err := postaladdress.EncodeOriginalInput(raw)
	require := require.New(t)
	require.NoError(err)
	old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: &street, ExtendedAddress: &unit, CountryCode: &country, OriginalValue: encoded, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport, SourceRef: &source, SourceResourceUID: &uid, TypeLabel: &label, Pref: &pref, Ordinal: &ordinal, VCard: store.VCardIdentity{Property: "ADR", PID: []string{"1.1"}}}})
	require.NoError(err)
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	var before store.PersonProfile
	require.NoError(json.Unmarshal(read.Body.Bytes(), &before))
	body, err := json.Marshal(map[string]any{"addresses": map[string]any{"supersede": []int64{old.Envelope.ID}, "add": []any{map[string]any{"address_kind": "postal", "street_address": "Corrected street", "extended_address": unit, "country_code": country, "original_input": raw, "envelope": map[string]any{"source": "user", "type_label": label, "pref": pref, "ordinal": ordinal}}}}})
	require.NoError(err)
	saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, saved.Code, saved.Body.String())
	var after store.PersonProfile
	require.NoError(json.Unmarshal(saved.Body.Bytes(), &after))
	require.Len(after.Addresses, 1)
	current := after.Addresses[0]
	assert := assert.New(t)
	assert.Equal(raw, current.Assessment.Original)
	assert.NotEqual(before.Addresses[0].Assessment.Fingerprint, current.Assessment.Fingerprint)
	assert.Equal(store.ProvenanceUser, current.Envelope.Source)
	assert.Nil(current.Envelope.Confidence)
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
	assert.NotNil(historic.Envelope.SupersededAt)
	assert.Equal(old.Envelope.Source, historic.Envelope.Source)
	assert.Equal(old.Envelope.SourceRef, historic.Envelope.SourceRef)
	assert.Equal(old.Envelope.SourceResourceUID, historic.Envelope.SourceResourceUID)
	assert.Equal(old.Envelope.VCard, historic.Envelope.VCard)
	assert.Equal(raw, historic.Assessment.Original)
	assert.Equal(raw.OriginalValue, historic.OriginalValue)
}

func TestPostalAddressCurrentTextRoundTripsWithoutChangingProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	raw := postaladdress.Input{AddressKind: "postal", OriginalValue: "  First raw address\nUnit 7  ", ExtendedComponents: `["raw"]`, Label: " original label "}
	read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	body, err := json.Marshal(map[string]any{"addresses": map[string]any{"add": []any{map[string]any{"address_kind": "postal", "original_input": raw, "envelope": map[string]any{"source": "user"}}}}})
	require.NoError(err)
	created := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
	require.Equal(http.StatusOK, created.Code, created.Body.String())
	var before store.PersonProfile
	require.NoError(json.Unmarshal(created.Body.Bytes(), &before))
	require.Len(before.Addresses, 1)
	text := "  Updated current address\nUnit 8  "
	body, err = json.Marshal(map[string]any{"addresses": map[string]any{"supersede": []int64{before.Addresses[0].Envelope.ID}, "add": []any{map[string]any{"address_kind": "postal", "free_text": text, "original_input": raw, "envelope": map[string]any{"source": "user"}}}}})
	require.NoError(err)
	edited := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": created.Header().Get("ETag")})
	require.Equal(http.StatusOK, edited.Code, edited.Body.String())
	current := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
	var profile store.PersonProfile
	require.NoError(json.Unmarshal(current.Body.Bytes(), &profile))
	require.Len(profile.Addresses, 1)
	address := profile.Addresses[0]
	require.NotNil(address.FreeText)
	assert.Equal(text, *address.FreeText)
	require.NotNil(address.Assessment)
	assert.Equal(raw, address.Assessment.Original)
	assert.Equal(raw.OriginalValue, address.OriginalValue)
	stored, err := wrapped.ListPersonAddressesContext(t.Context(), id, true)
	require.NoError(err)
	require.Len(stored, 1)
	decoded, ok := postaladdress.DecodeOriginalInput(stored[0].OriginalValue)
	require.True(ok)
	assert.Equal(raw, decoded)
	assert.Equal(text, *stored[0].FreeText)
	historyResponse := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id)+"/history", nil, nil)
	var history store.PersonProfileHistory
	require.NoError(json.Unmarshal(historyResponse.Body.Bytes(), &history))
	require.Len(history.Addresses, 2)
	for _, historic := range history.Addresses {
		require.NotNil(historic.Assessment)
		assert.Equal(raw, historic.Assessment.Original)
		if historic.Envelope.ID == before.Addresses[0].Envelope.ID {
			assert.NotNil(historic.Envelope.SupersededAt)
			assert.Nil(historic.FreeText)
		}
	}
}

func TestPostalAddressFallbackOriginalValuePreservesRawSnapshot(t *testing.T) {
	literal := "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"original_value":"literal raw text"}}`
	for _, tc := range []struct {
		name       string
		fields     map[string]any
		raw        postaladdress.Input
		want       string
		wantEdited string
	}{
		{
			name:       "free text",
			fields:     map[string]any{"free_text": "  Exampleville  "},
			raw:        postaladdress.Input{AddressKind: "postal", FreeText: "  Exampleville  "},
			want:       "Exampleville",
			wantEdited: "Updated Exampleville",
		},
		{
			name:       "components without a street",
			fields:     map[string]any{"locality": "  Exampleville  ", "country_code": "ca"},
			raw:        postaladdress.Input{AddressKind: "postal", Locality: "  Exampleville  ", CountryCode: "ca"},
			want:       ";;;  Exampleville  ;;;",
			wantEdited: ";;;  Exampleville  ;;;",
		},
		{
			name:       "explicit whitespace",
			fields:     map[string]any{"free_text": "Exampleville", "original_value": " \t\n "},
			raw:        postaladdress.Input{AddressKind: "postal", FreeText: "Exampleville", OriginalValue: " \t\n "},
			want:       " \t\n ",
			wantEdited: " \t\n ",
		},
		{
			name:       "literal snapshot text",
			fields:     map[string]any{"free_text": "Exampleville", "original_value": literal},
			raw:        postaladdress.Input{AddressKind: "postal", FreeText: "Exampleville", OriginalValue: literal},
			want:       literal,
			wantEdited: literal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			checkAddress := func(address store.PersonAddress, want string) {
				assert.Equal(want, address.OriginalValue)
				require.NotNil(address.Assessment)
				assert.Equal(tc.raw, address.Assessment.Original, "public fallback must not replace raw provenance")
				assert.Nil(address.StreetAddress, "a display fallback must not invent a street")
			}
			checkProfile := func(response *httptest.ResponseRecorder, want string) store.PersonAddress {
				require.Equal(http.StatusOK, response.Code, response.Body.String())
				var profile store.PersonProfile
				require.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
				require.Len(profile.Addresses, 1)
				checkAddress(profile.Addresses[0], want)
				return profile.Addresses[0]
			}
			fields := maps.Clone(tc.fields)
			fields["address_kind"] = "postal"
			fields["envelope"] = map[string]any{"source": "user"}
			body, err := json.Marshal(map[string]any{"addresses": map[string]any{"add": []any{fields}}})
			require.NoError(err)
			before := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			created := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": before.Header().Get("ETag")})
			original := checkProfile(created, tc.want)
			read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			checkProfile(read, tc.want)
			stored, err := wrapped.ListPersonAddressesContext(t.Context(), id, true)
			require.NoError(err)
			require.Len(stored, 1)
			snapshot := stored[0].OriginalValue
			decoded, ok := postaladdress.DecodeOriginalInput(snapshot)
			require.True(ok)
			assert.Equal(tc.raw, decoded)

			fields["free_text"] = "  Updated Exampleville  "
			fields["original_input"] = tc.raw
			body, err = json.Marshal(map[string]any{"addresses": map[string]any{"supersede": []int64{original.Envelope.ID}, "add": []any{fields}}})
			require.NoError(err)
			edited := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
			current := checkProfile(edited, tc.wantEdited)
			read = doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			checkProfile(read, tc.wantEdited)

			historyResponse := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id)+"/history", nil, nil)
			require.Equal(http.StatusOK, historyResponse.Code, historyResponse.Body.String())
			var history store.PersonProfileHistory
			require.NoError(json.Unmarshal(historyResponse.Body.Bytes(), &history))
			require.Len(history.Addresses, 2)
			for _, address := range history.Addresses {
				if address.Envelope.ID == original.Envelope.ID {
					checkAddress(address, tc.want)
					assert.NotNil(address.Envelope.SupersededAt)
				} else {
					assert.Equal(current.Envelope.ID, address.Envelope.ID)
					checkAddress(address, tc.wantEdited)
				}
			}
			stored, err = wrapped.ListPersonAddressesContext(t.Context(), id, false)
			require.NoError(err)
			require.Len(stored, 2)
			for _, address := range stored {
				assert.Equal(snapshot, address.OriginalValue, "serialization and superseding must leave snapshot bytes unchanged")
			}
		})
	}
}

func TestPostalAddressAssessmentRequiresPresentAddress(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantCode  int
		wantCalls int
	}{
		{"missing", `{}`, http.StatusBadRequest, 0},
		{"missing with external consent", `{"allow_external":true}`, http.StatusBadRequest, 0},
		{"null", `{"address":null}`, http.StatusBadRequest, 0},
		{"null with external consent", `{"address":null,"allow_external":true}`, http.StatusBadRequest, 0},
		{"unknown member", `{"address":{},"unknown":true}`, http.StatusBadRequest, 0},
		{"multiple values", `{"address":{}} {}`, http.StatusBadRequest, 0},
		{"present empty object", `{"address":{}}`, http.StatusOK, 0},
		{"present external draft", `{"address":{"street_address":"12 Example St","country_code":"CA"},"allow_external":true}`, http.StatusOK, 1},
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
			assert.Equal(tc.wantCalls, validator.calls)
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
				if tc.wantCalls == 0 {
					assert.Equal(postaladdress.StatusNeedsReview, assessment.Status)
				} else {
					assert.Equal(postaladdress.StatusValidated, assessment.Status)
				}
			}
			profile, err := wrapped.GetPersonProfileContext(t.Context(), id)
			require.NoError(err)
			assert.Empty(profile.Addresses, "assessment requests must not store addresses")
		})
	}
}
