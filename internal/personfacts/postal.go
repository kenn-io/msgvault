package personfacts

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/postaladdress"
)

// PostalAddressValue is an explicitly evidenced address, retained for owner
// review only. SourceText preserves the submitted wording, never a verdict.
type PostalAddressValue struct {
	PostOfficeBox      string `json:"post_office_box,omitempty"`
	ExtendedAddress    string `json:"extended_address,omitempty"`
	StreetAddress      string `json:"street_address,omitempty"`
	Locality           string `json:"locality,omitempty"`
	Region             string `json:"region,omitempty"`
	PostalCode         string `json:"postal_code,omitempty"`
	CountryName        string `json:"country_name,omitempty"`
	CountryCode        string `json:"country_code,omitempty"`
	ExtendedComponents string `json:"extended_components,omitempty"`
	FreeText           string `json:"free_text,omitempty"`
	SourceText         string `json:"source_text"`
}

func (v PostalAddressValue) Input() postaladdress.Input {
	return postaladdress.Input{AddressKind: "postal", PostOfficeBox: v.PostOfficeBox, ExtendedAddress: v.ExtendedAddress, StreetAddress: v.StreetAddress, Locality: v.Locality, Region: v.Region, PostalCode: v.PostalCode, CountryName: v.CountryName, CountryCode: v.CountryCode, ExtendedComponents: v.ExtendedComponents, FreeText: v.FreeText, OriginalValue: v.SourceText}
}

// PostalAddressReview is populated by Store from a consistent full-state
// snapshot. Client-side evidence pages cannot confer adoption eligibility.
type PostalAddressReview struct {
	Eligible              bool                      `json:"eligible"`
	ReasonCodes           []string                  `json:"reason_codes"`
	Assessment            *postaladdress.Assessment `json:"assessment,omitzero" nullable:"false"`
	PersonRevision        int64                     `json:"person_revision"`
	SupportingEvidenceIDs []int64                   `json:"supporting_evidence_ids"`
}

func postalAddressDescriptor() TargetDescriptor {
	fields := make([]FieldDescriptor, 0, 11)
	for _, name := range []string{"post_office_box", "extended_address", "street_address", "locality", "region", "postal_code", "country_name", "country_code", "extended_components", "free_text", "source_text"} {
		fields = append(fields, FieldDescriptor{Name: name, ValueType: ValueText, Cardinality: CardinalitySingle, Required: name == "source_text"})
	}
	return TargetDescriptor{Kind: TargetPostalAddress, Key: "system:postal_address", UniversalID: "system:postal_address", Slug: "postal_address", Description: "Mailing or residential address from explicit evidence. Requires your review; never inferred from a city or employer location. No automatic saving or external checks.", ValueType: ValuePostalAddress, Cardinality: CardinalityMulti, Fields: fields, Sensitive: true}
}

func normalizePostalAddressValue(submitted jsontext.Value) ([]byte, error) {
	if err := jsonexact.Validate(submitted, PostalAddressValue{}); err != nil {
		return nil, fmt.Errorf("validate postal address fields: %w", err)
	}
	var value PostalAddressValue
	if err := decodeJSONValue(submitted, &value); err != nil {
		return nil, fmt.Errorf("decode postal address: %w", err)
	}
	if strings.TrimSpace(value.SourceText) == "" {
		return nil, errors.New("postal address source_text is required")
	}
	proposed := postaladdress.Assess(value.Input()).Proposed
	if proposed.StreetAddress == "" && proposed.PostOfficeBox == "" && proposed.FreeText == "" {
		return nil, errors.New("postal address needs explicit street, post office box or free text")
	}
	value = PostalAddressValue{PostOfficeBox: proposed.PostOfficeBox, ExtendedAddress: proposed.ExtendedAddress, StreetAddress: proposed.StreetAddress, Locality: proposed.Locality, Region: proposed.Region, PostalCode: proposed.PostalCode, CountryName: proposed.CountryName, CountryCode: proposed.CountryCode, ExtendedComponents: proposed.ExtendedComponents, FreeText: proposed.FreeText, SourceText: proposed.OriginalValue}
	return json.Marshal(value, json.Deterministic(true))
}

// PostalReviewEligibility reuses the resolver's full evidence, identity,
// preparation, revision and validity rules without resolving a projection.
func (r Resolver) PostalReviewEligibility(input ResolutionInput) (map[string]PostalAddressReview, error) {
	if err := validateResolverPolicy(r.Policy); err != nil {
		return nil, err
	}
	if err := validateResolutionInput(input); err != nil {
		return nil, err
	}
	if input.Target.Kind != TargetPostalAddress {
		return nil, errors.New("postal review requires a postal target")
	}
	statuses, err := collectEffectiveEvidenceStatuses(input)
	if err != nil {
		return nil, err
	}
	reviews := make(map[string]PostalAddressReview, len(input.Claims))
	for _, claim := range input.Claims {
		candidate, err := r.prepareCandidate(input, claim, statuses)
		if err != nil {
			return nil, err
		}
		review := PostalAddressReview{Eligible: candidate.eligible, ReasonCodes: []string{}, SupportingEvidenceIDs: []int64{}}
		if !candidate.eligible {
			review.ReasonCodes = append(review.ReasonCodes, string(candidate.decision.Reason))
		}
		if claim.Claim.Relation != RelationSupport {
			review.Eligible = false
			review.ReasonCodes = append(review.ReasonCodes, "relation_not_support")
		}
		for _, evidence := range candidate.supportedEvidence {
			review.SupportingEvidenceIDs = append(review.SupportingEvidenceIDs, evidence.ID)
		}
		slices.Sort(review.SupportingEvidenceIDs)
		reviews[claim.ClaimKey] = review
	}
	return reviews, nil
}
