package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/postaladdress"
	"go.kenn.io/msgvault/internal/vcard"
)

// PostalInput exposes the address components independently of storage pointers.
// OriginalValue is storage text; callers handling a known snapshot decode it
// explicitly, rather than guessing whether newly entered raw text is a tag.
func (a PersonAddress) PostalInput() postaladdress.Input {
	value := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return postaladdress.Input{AddressKind: string(a.AddressKind), PostOfficeBox: value(a.PostOfficeBox), ExtendedAddress: value(a.ExtendedAddress), StreetAddress: value(a.StreetAddress), Locality: value(a.Locality), Region: value(a.Region), PostalCode: value(a.PostalCode), CountryName: value(a.CountryName), ExtendedComponents: value(a.ExtendedComponents), FreeText: value(a.FreeText), Label: value(a.Label), GeoURI: value(a.GeoURI), Timezone: value(a.Timezone), CountryCode: value(a.CountryCode), PlaceURI: value(a.PlaceURI), OriginalValue: a.OriginalValue}
}

// ResolvePostalInput parses legacy text with an ADR shape or an empty second field;
// otherwise it keeps the text intact. Explicit current text disables the fallback.
func ResolvePostalInput(input postaladdress.Input, useOriginal bool) postaladdress.Input {
	if !useOriginal {
		return input
	}
	raw := strings.TrimSpace(input.OriginalValue)
	if raw == "" || strings.TrimSpace(input.FreeText) != "" {
		return input
	}
	fields := []string{input.PostOfficeBox, input.ExtendedAddress, input.StreetAddress, input.Locality, input.Region, input.PostalCode, input.CountryName}
	if slices.ContainsFunc(fields, func(value string) bool { return strings.TrimSpace(value) != "" }) || strings.TrimSpace(input.ExtendedComponents) != "" {
		if !postalTextCovered(raw, fields, input.ExtendedComponents) {
			input.FreeText = raw
		}
		return input
	}
	parts, err := vcard.SplitStructuredText(raw)
	if err == nil && (len(parts) >= 7 || len(parts) >= 4 && parts[1] == "" || strings.HasPrefix(raw, ";;")) && slices.ContainsFunc(parts, func(part string) bool { return strings.TrimSpace(part) != "" }) {
		fields := []*string{&input.PostOfficeBox, &input.ExtendedAddress, &input.StreetAddress, &input.Locality, &input.Region, &input.PostalCode, &input.CountryName}
		for i := range min(len(parts), len(fields)) {
			*fields[i] = parts[i]
		}
		if len(parts) > 7 {
			input.ExtendedComponents = vcard.JoinStructuredText(parts)
		}
	} else {
		input.FreeText = raw
	}
	return input
}

func postalTextCovered(raw string, fields []string, extended string) bool {
	if slices.ContainsFunc(fields, func(value string) bool { return strings.TrimSpace(value) == raw }) {
		return true
	}
	parts, err := vcard.SplitStructuredText(raw)
	if err != nil || len(parts) < len(fields) {
		return false
	}
	for i, field := range fields {
		if strings.TrimSpace(parts[i]) != strings.TrimSpace(field) {
			return false
		}
	}
	if len(parts) > len(fields) {
		extendedParts, err := vcard.SplitStructuredText(extended)
		return err == nil && slices.Equal(parts, extendedParts)
	}
	return true
}

// SetPostalFields stores each non-empty component and free text of current.
func SetPostalFields(current postaladdress.Input, fields [9]**string) {
	for i, value := range [9]string{current.PostOfficeBox, current.ExtendedAddress, current.StreetAddress, current.Locality, current.Region, current.PostalCode, current.CountryName, current.ExtendedComponents, current.FreeText} {
		if value != "" {
			*fields[i] = new(value)
		}
	}
}

// ResolvedPostalInput separates current data from the captured source.
func (a PersonAddress) ResolvedPostalInput() (postaladdress.Input, postaladdress.Input) {
	current := a.PostalInput()
	original, snapshot := postaladdress.DecodeStoredOriginalInput(a.OriginalValue)
	if snapshot {
		current.OriginalValue = ""
		return current, original
	}
	current = ResolvePostalInput(current, a.FreeText == nil)
	return current, current
}

type personFactPostalProjector struct {
	store *Store
	tx    *loggedTx
}

func (p *personFactPostalProjector) loadCurrent(ctx context.Context, personID int64, target personfacts.TargetDescriptor) ([]personfacts.CurrentProjection, error) {
	rows, err := p.store.listPersonAddressesTx(ctx, p.tx, personID, true)
	if err != nil {
		return nil, err
	}
	current := []personfacts.CurrentProjection{}
	for _, row := range rows {
		if row.AddressKind != PersonAddressPostal {
			continue
		}
		input, _ := row.ResolvedPostalInput()
		v := personfacts.PostalAddressValue{PostOfficeBox: input.PostOfficeBox, ExtendedAddress: input.ExtendedAddress, StreetAddress: input.StreetAddress, Locality: input.Locality, Region: input.Region, PostalCode: input.PostalCode, CountryName: input.CountryName, CountryCode: input.CountryCode, ExtendedComponents: input.ExtendedComponents, FreeText: input.FreeText, SourceText: input.OriginalValue}
		data, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		active := row.Envelope.CreatedAt
		if row.Envelope.ActiveFrom != nil {
			active = *row.Envelope.ActiveFrom
		}
		current = append(current, personfacts.CurrentProjection{Ref: personfacts.ProjectionRef{Kind: "postal_address", RowID: row.Envelope.ID}, Normalized: personfacts.NormalizedValue{JSON: jsontext.Value(data), Fingerprint: "sha256:" + hex.EncodeToString(sum[:])}, ActiveFrom: active, ActiveUntil: row.Envelope.ActiveUntil, TransactionTime: row.Envelope.CreatedAt, Declared: true})
	}
	return current, nil
}
func (p *personFactPostalProjector) projectionContext(context.Context, int64, []personfacts.ResolvedClaim) (string, error) {
	return "", nil
}
func (p *personFactPostalProjector) project(context.Context, personfacts.ProjectionPlan, personfacts.ResolvedClaim, string, time.Time) (*personfacts.ProjectionRef, bool, error) {
	return nil, false, errors.New("postal addresses require explicit owner review")
}

func postalReviewComponents(input postaladdress.Input) postaladdress.Input {
	input = postaladdress.Assess(input).Proposed
	input.OriginalValue, input.Label, input.GeoURI, input.Timezone, input.PlaceURI = "", "", "", "", ""
	return input
}

// attachPostalReviewsTx calculates eligibility against all claims and current
// support in this read snapshot, regardless of the requested claim page.
func (s *Store) attachPostalReviewsTx(ctx context.Context, tx *loggedTx, personID int64, page []personfacts.Claim) error {
	hasPostal := false
	for _, claim := range page {
		if claim.Target.Kind == personfacts.TargetPostalAddress {
			hasPostal = true
			break
		}
	}
	if !hasPostal {
		return nil
	}
	descriptor, _, err := s.loadPersonFactTargetDescriptorSnapshotTx(ctx, tx, personfacts.TargetPostalAddress, "system:postal_address", nil, true)
	if err != nil {
		return err
	}
	claims, _, err := s.loadPersonFactResolvedClaimsTx(ctx, tx, personID, descriptor, nil)
	if err != nil {
		return err
	}
	resolver, err := personfacts.NewResolver(personfacts.DefaultPolicyV1())
	if err != nil {
		return err
	}
	reviews, err := resolver.PostalReviewEligibility(personfacts.ResolutionInput{PersonID: personID, Target: descriptor, ResolvedAt: time.Now().UTC(), Policy: personfacts.PolicyContext{AllowSensitive: true}, Claims: claims})
	if err != nil {
		return err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`, personID).Scan(&revision); err != nil {
		return err
	}
	// History counts as adoption too; replacing a row must not reoffer its claim.
	addresses, err := s.listPersonAddressesTx(ctx, tx, personID, false)
	if err != nil {
		return err
	}
	adopted := map[string]bool{}
	current := map[postaladdress.Input]bool{}
	for _, row := range addresses {
		if row.AddressKind != PersonAddressPostal {
			continue
		}
		if row.Envelope.SourceRef != nil {
			adopted[*row.Envelope.SourceRef] = true
		}
		if row.Envelope.ActiveUntil == nil && row.Envelope.SupersededAt == nil {
			input, _ := row.ResolvedPostalInput()
			current[postalReviewComponents(input)] = true
		}
	}
	for i := range page {
		claim := &page[i]
		if claim.Target.Kind != personfacts.TargetPostalAddress {
			continue
		}
		review, ok := reviews[claim.ClaimKey]
		if !ok {
			return errors.New("postal review claim missing from current snapshot")
		}
		review.PersonRevision = revision
		if adopted["person-fact-claim:"+claim.ClaimKey] {
			review.Eligible = false
			review.ReasonCodes = append(review.ReasonCodes, "already_adopted")
		}
		var raw personfacts.PostalAddressValue
		if err := json.Unmarshal(claim.SubmittedValue, &raw, json.RejectUnknownMembers(true)); err == nil {
			assessment := postaladdress.Assess(raw.Input())
			if claim.Normalized != nil {
				var proposed personfacts.PostalAddressValue
				if err := json.Unmarshal(claim.Normalized.JSON, &proposed); err == nil {
					assessment.Proposed = proposed.Input()
					assessment.ProposedExtendedParts = postaladdress.ExtendedParts(assessment.Proposed.ExtendedComponents)
				}
			}
			if review.Eligible && current[postalReviewComponents(assessment.Proposed)] {
				review.Eligible = false
				review.ReasonCodes = append(review.ReasonCodes, "already_adopted")
			}
			review.Assessment = &assessment
		}
		claim.PostalReview = &review
	}
	return nil
}

// validatePostalInputTx rejects a postal snapshot that cites a suggestion whose
// submitted address it was not captured from. Edited fields stay free.
func (s *Store) validatePostalInputTx(ctx context.Context, tx *loggedTx, personID int64, input PersonAddressInput, replacedOriginal *string) error {
	original, snapshot, err := postaladdress.DecodeOriginalInput(input.OriginalValue)
	key, cited := "", false
	if input.Envelope.SourceRef != nil {
		key, cited = strings.CutPrefix(*input.Envelope.SourceRef, "person-fact-claim:")
	}
	if err != nil || !cited {
		return err
	}
	if replacedOriginal != nil {
		previous, previousSnapshot, err := postaladdress.DecodeOriginalInput(*replacedOriginal)
		if err != nil || !snapshot || !previousSnapshot || previous != original {
			return ErrPostalSuggestionMismatch
		}
		return nil
	}
	var submitted []byte
	err = tx.QueryRowContext(ctx, `SELECT submitted_value_json FROM person_fact_claims WHERE person_id = ? AND claim_key = ? AND target_kind = 'postal_address' LIMIT 1`, personID, key).Scan(&submitted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPostalSuggestionInvalid
	}
	if err != nil {
		return err
	}
	var claimed personfacts.PostalAddressValue
	if json.Unmarshal(submitted, &claimed, json.RejectUnknownMembers(true)) != nil {
		return ErrPostalSuggestionInvalid
	}
	if !snapshot || claimed.Input() != original {
		return ErrPostalSuggestionMismatch
	}
	page := []personfacts.Claim{{ClaimKey: key, Target: personfacts.TargetRef{Kind: personfacts.TargetPostalAddress}, SubmittedValue: submitted}}
	if err := s.attachPostalReviewsTx(ctx, tx, personID, page); err != nil {
		return err
	}
	if !page[0].PostalReview.Eligible {
		return ErrPostalSuggestionInvalid
	}
	return nil
}

// LocalPostalAssessment is derived on read; no external verdict is stored.
func (a PersonAddress) LocalPostalAssessment() *postaladdress.Assessment {
	if a.AddressKind != PersonAddressPostal {
		return nil
	}
	current, original := a.ResolvedPostalInput()
	assessment := postaladdress.Assess(current)
	assessment.Original = original
	assessment.OriginalExtendedParts = postaladdress.ExtendedParts(original.ExtendedComponents)
	return &assessment
}

// PublicAddress exposes current fields and source text without the stored envelope.
func (a PersonAddress) PublicAddress() (PersonAddress, error) {
	if a.AddressKind != PersonAddressPostal {
		return a, nil
	}
	current, original := a.ResolvedPostalInput()
	a.OriginalValue = original.OriginalValue
	SetPostalFields(current, [9]**string{&a.PostOfficeBox, &a.ExtendedAddress, &a.StreetAddress, &a.Locality, &a.Region, &a.PostalCode, &a.CountryName, &a.ExtendedComponents, &a.FreeText})
	return a, nil
}
