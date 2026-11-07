package postaladdress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// GoogleAddressValidationEnvVar names the dedicated address-validation credential environment variable.
const GoogleAddressValidationEnvVar = "MSGVAULT_GOOGLE_ADDRESS_VALIDATION_API_KEY"
const googleEndpoint = "https://addressvalidation.googleapis.com/v1:validateAddress"
const maxGoogleResponse = 1 << 20

var ErrProviderUnavailable = errors.New("postal address provider unavailable")

// GoogleValidator performs one explicit check. Callers own per-request consent.
// Client is injectable for tests; the production destination is fixed. It never
// retries or follows redirects carrying the key to a different destination.
type GoogleValidator struct {
	APIKey   string
	Client   *http.Client
	endpoint string // package tests only; never exposed through operator config
	timeout  time.Duration
}

type googlePostalAddress struct {
	RegionCode         string   `json:"regionCode,omitempty"`
	PostalCode         string   `json:"postalCode,omitempty"`
	AdministrativeArea string   `json:"administrativeArea,omitempty"`
	Locality           string   `json:"locality,omitempty"`
	AddressLines       []string `json:"addressLines"`
}

type googleResult struct {
	Verdict struct {
		AddressComplete             bool   `json:"addressComplete"`
		ValidationGranularity       string `json:"validationGranularity"`
		HasUnconfirmedComponents    bool   `json:"hasUnconfirmedComponents"`
		HasInferredComponents       bool   `json:"hasInferredComponents"`
		HasReplacedComponents       bool   `json:"hasReplacedComponents"`
		HasSpellCorrectedComponents bool   `json:"hasSpellCorrectedComponents"`
		PossibleNextAction          string `json:"possibleNextAction"`
	} `json:"verdict"`
	Address *struct {
		PostalAddress     googlePostalAddress `json:"postalAddress"`
		AddressComponents []struct {
			ComponentType     string `json:"componentType"`
			ConfirmationLevel string `json:"confirmationLevel"`
			Inferred          bool   `json:"inferred"`
			Replaced          bool   `json:"replaced"`
			Unexpected        bool   `json:"unexpected"`
			SpellCorrected    bool   `json:"spellCorrected"`
		} `json:"addressComponents"`
		MissingComponentTypes     []string `json:"missingComponentTypes"`
		UnconfirmedComponentTypes []string `json:"unconfirmedComponentTypes"`
		UnresolvedTokens          []string `json:"unresolvedTokens"`
	} `json:"address"`
}

// Check maps only explicit postal components, enforces Google's 280-character
// bound without truncation, and returns a transient assessment. API errors are
// deliberately redacted: transport errors and provider bodies may echo input.
// Protocol: https://developers.google.com/maps/documentation/address-validation/reference/rest/v1/TopLevel/validateAddress
func (v *GoogleValidator) Check(ctx context.Context, input Input) (Assessment, error) {
	assessment := Assess(input)
	assessment.Provider = "google"
	payload, reason := googleRequest(assessment.Proposed)
	if reason != "" {
		assessment.Status = StatusNeedsReview
		assessment.ReasonCodes = append(assessment.ReasonCodes, reason)
		return assessment, nil
	}
	fail := func(reason string, cause error) (Assessment, error) {
		assessment.Status = StatusUnavailable
		assessment.ReasonCodes = append(assessment.ReasonCodes, reason)
		if cause != nil {
			return assessment, errors.Join(ErrProviderUnavailable, cause)
		}
		return assessment, ErrProviderUnavailable
	}
	if strings.TrimSpace(v.APIKey) == "" {
		return fail("provider_not_configured", nil)
	}
	timeout := v.timeout
	if timeout <= 0 || timeout > 8*time.Second {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := json.Marshal(struct {
		Address googlePostalAddress `json:"address"`
	}{payload})
	if err != nil {
		return fail("provider_request_failed", nil)
	}
	endpoint := v.endpoint
	if endpoint == "" {
		endpoint = googleEndpoint
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return fail("provider_request_failed", nil)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Goog-Api-Key", v.APIKey)
	client := http.Client{}
	if v.Client != nil {
		client = *v.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := time.Now().UTC()
	assessment.CheckedAt = &now
	response, err := client.Do(request)
	if err != nil {
		return fail("provider_request_failed", ctx.Err())
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail("provider_request_failed", nil)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGoogleResponse+1))
	if err != nil || len(body) > maxGoogleResponse {
		return fail("provider_response_invalid", ctx.Err())
	}
	var decoded struct {
		Result *googleResult `json:"result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Result == nil || decoded.Result.Address == nil {
		return fail("provider_response_invalid", nil)
	}
	return googleAssessment(assessment, *decoded.Result), nil
}

func googleRequest(input Input) (googlePostalAddress, string) {
	payload := googlePostalAddress{RegionCode: input.CountryCode, PostalCode: input.PostalCode, AdministrativeArea: input.Region, Locality: input.Locality}
	// No original/free text, labels, citations, opaque extended JSON, location
	// metadata, recipients or organizations are sent to the provider.
	for _, value := range []string{input.StreetAddress, input.ExtendedAddress, input.PostOfficeBox} {
		if value != "" {
			payload.AddressLines = append(payload.AddressLines, strings.Split(value, "\n")...)
		}
	}
	if !knownCountry(input.CountryCode) {
		return payload, "external_country_unsuitable"
	}
	if input.StreetAddress == "" && input.PostOfficeBox == "" {
		return payload, "external_input_unsuitable"
	}
	count := 0
	for _, value := range append([]string{payload.RegionCode, payload.PostalCode, payload.AdministrativeArea, payload.Locality}, payload.AddressLines...) {
		if !utf8.ValidString(value) {
			return payload, "external_input_unsuitable"
		}
		count += utf8.RuneCountInString(value)
	}
	if count > 280 {
		return payload, "external_input_too_long"
	}
	return payload, ""
}

func googleAssessment(assessment Assessment, result googleResult) Assessment {
	address := result.Address
	reasons := []string{}
	review := func(reason string) {
		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	verdict := result.Verdict
	if !verdict.AddressComplete || len(address.MissingComponentTypes) > 0 {
		review("provider_incomplete")
	}
	if verdict.ValidationGranularity != "PREMISE" && verdict.ValidationGranularity != "SUB_PREMISE" {
		review("provider_coarse_result")
	}
	if verdict.HasUnconfirmedComponents || len(address.UnconfirmedComponentTypes) > 0 || len(address.AddressComponents) == 0 {
		review("provider_unconfirmed")
	}
	if verdict.HasInferredComponents {
		review("provider_inferred")
	}
	if verdict.HasReplacedComponents {
		review("provider_replaced")
	}
	if verdict.HasSpellCorrectedComponents {
		review("provider_corrected")
	}
	if len(address.UnresolvedTokens) > 0 {
		review("provider_unresolved")
	}
	if verdict.PossibleNextAction != "" && verdict.PossibleNextAction != "ACCEPT" {
		review("provider_confirmation_required")
	}
	for _, component := range address.AddressComponents {
		if component.ConfirmationLevel != "CONFIRMED" {
			review("provider_unconfirmed")
		}
		if component.Inferred {
			review("provider_inferred")
		}
		if component.Replaced {
			review("provider_replaced")
		}
		if component.Unexpected {
			review("provider_unexpected")
		}
		if component.SpellCorrected {
			review("provider_corrected")
		}
	}
	proposal := assessment.Proposed
	postal := address.PostalAddress
	// Missing response fields cannot clear input. An uncertain country change
	// also stays a review item rather than inheriting an unrelated country name.
	for _, pair := range []struct {
		dst   *string
		value string
	}{{&proposal.CountryCode, postal.RegionCode}, {&proposal.Locality, postal.Locality}, {&proposal.Region, postal.AdministrativeArea}, {&proposal.PostalCode, postal.PostalCode}} {
		if pair.value != "" {
			*pair.dst = pair.value
		} else if *pair.dst != "" {
			review("provider_component_not_confirmed")
		}
	}
	if !knownCountry(postal.RegionCode) {
		review("provider_country_unknown")
	}
	if proposal.CountryCode != assessment.Proposed.CountryCode {
		review("provider_country_changed")
	}
	lines := slices.Clone(postal.AddressLines)
	preserveStreet := false
	for _, detail := range []struct{ value, reason string }{{proposal.ExtendedAddress, "unit_not_confirmed"}, {proposal.PostOfficeBox, "po_box_not_confirmed"}} {
		if detail.value == "" {
			continue
		}
		index := slices.Index(lines, detail.value)
		if index >= 0 {
			lines = slices.Delete(lines, index, index+1)
		} else {
			preserveStreet = true
			review(detail.reason)
		}
	}
	if proposal.ExtendedAddress != "" && verdict.ValidationGranularity != "SUB_PREMISE" {
		review("unit_not_confirmed")
	}
	if len(lines) > 0 && !preserveStreet {
		proposal.StreetAddress = strings.Join(lines, "\n")
	} else if proposal.StreetAddress != "" {
		// Detail-only lines cannot confirm the retained input street.
		review("provider_component_not_confirmed")
	}
	if len(postal.AddressLines) == 0 {
		review("provider_incomplete")
	}
	if proposal.ExtendedComponents != "" || proposal.FreeText != "" || proposal.CountryName != "" {
		review("unmapped_components_preserved")
	}
	assessment.Proposed = proposal
	assessment.Status = StatusValidated
	assessment.ReasonCodes = []string{"provider_confirmed"}
	if len(reasons) > 0 {
		assessment.Status = StatusNeedsReview
		assessment.ReasonCodes = reasons
	}
	return assessment
}
