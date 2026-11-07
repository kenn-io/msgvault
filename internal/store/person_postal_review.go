package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/postaladdress"
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
		input := row.PostalInput()
		if raw, ok := postaladdress.DecodeOriginalInput(row.OriginalValue); ok {
			input.OriginalValue = raw.OriginalValue
		}
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
	for _, row := range addresses {
		if row.Envelope.SourceRef != nil {
			adopted[*row.Envelope.SourceRef] = true
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
		if err := json.Unmarshal(claim.SubmittedValue, &raw); err == nil {
			assessment := postaladdress.Assess(raw.Input())
			if claim.Normalized != nil {
				var proposed personfacts.PostalAddressValue
				if err := json.Unmarshal(claim.Normalized.JSON, &proposed); err == nil {
					assessment.Proposed = proposed.Input()
				}
			}
			review.Assessment = &assessment
		}
		claim.PostalReview = &review
	}
	return nil
}

// LocalPostalAssessment is derived on read; no external verdict is stored.
func (a PersonAddress) LocalPostalAssessment() *postaladdress.Assessment {
	if a.AddressKind != PersonAddressPostal {
		return nil
	}
	current := a.PostalInput()
	original := current
	if raw, ok := postaladdress.DecodeOriginalInput(a.OriginalValue); ok {
		original = raw
		current.OriginalValue = raw.OriginalValue
	}
	assessment := postaladdress.Assess(current)
	assessment.Original = original
	return &assessment
}

// PublicAddress retains the derived raw input while exposing plain original
// text instead of the private storage envelope in profile and history reads.
func (a PersonAddress) PublicAddress() PersonAddress {
	if a.AddressKind != PersonAddressPostal {
		return a
	}
	if raw, ok := postaladdress.DecodeOriginalInput(a.OriginalValue); ok {
		a.OriginalValue = raw.OriginalValue
		if a.OriginalValue == "" {
			// Preserve the legacy display fallback without changing raw provenance.
			a.OriginalValue = personAddressOriginalValue(PersonAddressInput{
				PostOfficeBox: a.PostOfficeBox, ExtendedAddress: a.ExtendedAddress,
				StreetAddress: a.StreetAddress, Locality: a.Locality,
				Region: a.Region, PostalCode: a.PostalCode, CountryName: a.CountryName,
				FreeText: a.FreeText, GeoURI: a.GeoURI, PlaceURI: a.PlaceURI,
				ExtendedComponents: a.ExtendedComponents, Label: a.Label, CountryCode: a.CountryCode,
			})
		}
	}
	return a
}
