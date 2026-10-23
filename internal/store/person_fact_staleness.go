package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

var errPersonFactStalenessSupportUnmatched = errors.New(
	"employment support does not match a current organization",
)

// PersonFactStaleness describes a current value and when its supporting
// evidence should next be verified. Reading this report never changes the value.
type PersonFactStaleness struct {
	Target        personfacts.TargetRef     `json:"target"`
	Slug          string                    `json:"slug"`
	Projection    personfacts.ProjectionRef `json:"projection"`
	Value         jsontext.Value            `json:"value"`
	Stability     personfacts.Stability     `json:"stability"`
	Declared      bool                      `json:"declared"`
	NewestSupport time.Time                 `json:"newest_support"`
	Stale         bool                      `json:"stale"`
	VerifyBy      *time.Time                `json:"verify_by,omitzero"`
}

// PersonFactStalenessContext reports current values using one read snapshot.
// Declared values age from their start date; machine values age from their
// newest still-supported evidence, falling back to the value's start date.
func (s *Store) PersonFactStalenessContext(
	ctx context.Context, personID int64, asOf time.Time, includeSensitive bool,
) ([]PersonFactStaleness, error) {
	if personID <= 0 {
		return nil, ErrPersonNotFound
	}
	asOf = asOf.UTC()
	items := make([]PersonFactStaleness, 0)
	policy := personfacts.DefaultPolicyV2()
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM persons WHERE id = ?`, personID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPersonNotFound
			}
			return fmt.Errorf("validate person fact staleness person: %w", err)
		}
		catalog, err := s.buildPersonFactCatalogContext(ctx, tx, includeSensitive)
		if err != nil {
			return fmt.Errorf("build person fact staleness catalog: %w", err)
		}
		targets := append([]personfacts.TargetDescriptor(nil), catalog.Targets...)
		seen := make(map[personFactStalenessTargetKey]struct{}, len(targets))
		for _, target := range targets {
			seen[personFactStalenessTargetKey{kind: target.Kind, key: target.Key}] = struct{}{}
		}
		definitions, err := s.listAttributeDefinitionsContext(ctx, tx, AttributeDefinitionFilter{
			ObjectType: AttributeObjectPerson, IncludeHidden: true,
		})
		if err != nil {
			return fmt.Errorf("list person fact staleness attributes: %w", err)
		}
		for _, definition := range definitions {
			if !personFactAttributeScalarEligible(definition, includeSensitive) {
				continue
			}
			key := personFactStalenessTargetKey{
				kind: personfacts.TargetAttribute, key: definition.UniversalID,
			}
			if _, exists := seen[key]; exists {
				continue
			}
			target, err := personFactAttributeDescriptor(definition)
			if err != nil {
				return fmt.Errorf("describe person fact staleness attribute %q: %w", definition.Slug, err)
			}
			targets = append(targets, target)
			seen[key] = struct{}{}
		}
		sort.Slice(targets, func(i, j int) bool {
			if targets[i].Kind != targets[j].Kind {
				return targets[i].Kind < targets[j].Kind
			}
			return targets[i].Key < targets[j].Key
		})
		for _, target := range targets {
			_, eligibility, err := s.loadPersonFactTargetDescriptorSnapshotTx(ctx, tx, target.Kind, target.Key, &target, includeSensitive)
			if err != nil {
				return fmt.Errorf("load person fact staleness target: %w", err)
			}
			projector, err := s.personFactProjectorSnapshotTx(ctx, tx, target)
			if err != nil {
				return fmt.Errorf("load person fact staleness projector: %w", err)
			}
			current, err := projector.loadCurrent(ctx, personID, target)
			if err != nil {
				return fmt.Errorf("load person fact staleness current values: %w", err)
			}
			for _, value := range current {
				newest := value.ActiveFrom.UTC()
				if !value.Declared {
					support, err := s.newestPersonFactSupportTx(
						ctx, tx, personID, target, value.Normalized.Fingerprint,
						policy.MinimumIdentityScore, asOf,
					)
					if err != nil {
						return err
					}
					if !support.IsZero() {
						newest = support
					}
				}
				item := PersonFactStaleness{
					Target: personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, Slug: target.Slug,
					Projection: value.Ref, Value: append(jsontext.Value(nil), value.Normalized.JSON...), Stability: eligibility.Stability,
					Declared: value.Declared, NewestSupport: newest,
				}
				if horizon, ok := policy.StaleAfter(eligibility.Stability); ok {
					verifyBy := newest.Add(horizon)
					item.VerifyBy = &verifyBy
					item.Stale = asOf.Sub(newest) > horizon
				}
				items = append(items, item)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Target.Kind != b.Target.Kind {
			return a.Target.Kind < b.Target.Kind
		}
		if a.Target.Key != b.Target.Key {
			return a.Target.Key < b.Target.Key
		}
		return a.Projection.RowID < b.Projection.RowID
	})
	return items, nil
}

type personFactStalenessTargetKey struct {
	kind personfacts.TargetKind
	key  string
}

type personFactStalenessEvidenceSupport struct {
	claimKey         string
	evidenceID       int64
	eventTime        time.Time
	identityEligible bool
}

func (s *Store) newestPersonFactSupportTx(
	ctx context.Context, tx *loggedTx, personID int64, target personfacts.TargetDescriptor,
	fingerprint string, minimumIdentityScore int, asOf time.Time,
) (time.Time, error) {
	if target.Kind == personfacts.TargetEmployment {
		return s.newestPersonFactEmploymentSupportTx(
			ctx, tx, personID, target, fingerprint, minimumIdentityScore, asOf)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_key, e.id, e.event_time, e.subject_person_id, e.identity_score
		FROM person_fact_claims c
		JOIN person_fact_claim_evidence ce ON ce.claim_id = c.id
		JOIN person_fact_evidence e ON e.id = ce.evidence_id
		WHERE c.person_id = ? AND e.person_id = c.person_id
		  AND c.target_kind = ? AND c.target_key = ? AND c.relation = 'support'
		  AND c.value_fingerprint = ? AND c.rejection_action IS NULL
		  AND (c.valid_from IS NULL OR c.valid_from <= ?)
		  AND (c.valid_until IS NULL OR c.valid_until > ?)
		ORDER BY c.id, e.id`, personID, target.Kind, target.Key, fingerprint, asOf, asOf)
	if err != nil {
		return time.Time{}, fmt.Errorf("load person fact staleness evidence: %w", err)
	}
	supports := make([]personFactStalenessEvidenceSupport, 0)
	for rows.Next() {
		var claimKey string
		var id, identityScore int64
		var subjectPersonID sql.NullInt64
		var eventTime personFactTimestamp
		if err := rows.Scan(&claimKey, &id, &eventTime, &subjectPersonID, &identityScore); err != nil {
			_ = rows.Close()
			return time.Time{}, fmt.Errorf("scan person fact staleness evidence: %w", err)
		}
		supports = append(supports, personFactStalenessEvidenceSupport{
			claimKey: claimKey, evidenceID: id, eventTime: eventTime.Time.UTC(),
			identityEligible: subjectPersonID.Valid && subjectPersonID.Int64 == personID &&
				identityScore >= int64(minimumIdentityScore),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return time.Time{}, fmt.Errorf("iterate person fact staleness evidence: %w", err)
	}
	if err := rows.Close(); err != nil {
		return time.Time{}, fmt.Errorf("close person fact staleness evidence: %w", err)
	}
	return s.newestSupportedPersonFactEvidenceTx(ctx, tx, supports)
}

func (s *Store) newestSupportedPersonFactEvidenceTx(
	ctx context.Context, tx *loggedTx, supports []personFactStalenessEvidenceSupport,
) (time.Time, error) {
	ids := make([]int64, 0, len(supports))
	seen := make(map[int64]struct{}, len(supports))
	for _, support := range supports {
		if _, exists := seen[support.evidenceID]; exists {
			continue
		}
		ids = append(ids, support.evidenceID)
		seen[support.evidenceID] = struct{}{}
	}
	if len(ids) == 0 {
		return time.Time{}, nil
	}
	supported, err := s.latestEvidenceSupportedTx(ctx, tx, ids)
	if err != nil {
		return time.Time{}, err
	}
	type claimSupport struct {
		hasSupportedEvidence bool
		identityRejected     bool
		newest               time.Time
	}
	claims := make(map[string]claimSupport)
	for _, evidence := range supports {
		if !supported[evidence.evidenceID] {
			continue
		}
		claim := claims[evidence.claimKey]
		claim.hasSupportedEvidence = true
		if !evidence.identityEligible {
			claim.identityRejected = true
		} else if evidence.eventTime.After(claim.newest) {
			claim.newest = evidence.eventTime
		}
		claims[evidence.claimKey] = claim
	}
	var newest time.Time
	for _, claim := range claims {
		if claim.hasSupportedEvidence && !claim.identityRejected && claim.newest.After(newest) {
			newest = claim.newest
		}
	}
	return newest, nil
}

func (s *Store) newestPersonFactEmploymentSupportTx(
	ctx context.Context, tx *loggedTx, personID int64, target personfacts.TargetDescriptor,
	fingerprint string, minimumIdentityScore int, asOf time.Time,
) (time.Time, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_key, CAST(c.normalized_value_json AS TEXT), e.id, e.event_time,
		       e.subject_person_id, e.identity_score
		FROM person_fact_evidence e
		JOIN person_fact_claim_evidence ce ON ce.evidence_id = e.id
		JOIN person_fact_claims c ON c.id = ce.claim_id
		WHERE e.person_id = ? AND c.person_id = e.person_id
		  AND c.target_kind = ? AND c.target_key = ? AND c.relation = 'support'
		  AND c.rejection_action IS NULL AND c.normalized_value_json IS NOT NULL
		  AND (c.valid_from IS NULL OR c.valid_from <= ?)
		  AND (c.valid_until IS NULL OR c.valid_until > ?)
		ORDER BY c.id, e.id`, personID, target.Kind, target.Key, asOf, asOf)
	if err != nil {
		return time.Time{}, fmt.Errorf("load employment staleness support: %w", err)
	}
	claimValues := make(map[string]string)
	supports := make([]personFactStalenessEvidenceSupport, 0)
	claimKeys := make([]string, 0)
	seenClaims := make(map[string]struct{})
	for rows.Next() {
		var claimKey, valueJSON string
		var evidenceID, identityScore int64
		var subjectPersonID sql.NullInt64
		var eventTime personFactTimestamp
		if err := rows.Scan(&claimKey, &valueJSON, &evidenceID, &eventTime,
			&subjectPersonID, &identityScore); err != nil {
			_ = rows.Close()
			return time.Time{}, fmt.Errorf("scan employment staleness support: %w", err)
		}
		claimValues[claimKey] = valueJSON
		if _, exists := seenClaims[claimKey]; !exists {
			claimKeys = append(claimKeys, claimKey)
			seenClaims[claimKey] = struct{}{}
		}
		supports = append(supports, personFactStalenessEvidenceSupport{
			claimKey: claimKey, evidenceID: evidenceID, eventTime: eventTime.Time.UTC(),
			identityEligible: subjectPersonID.Valid && subjectPersonID.Int64 == personID &&
				identityScore >= int64(minimumIdentityScore),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return time.Time{}, fmt.Errorf("iterate employment staleness support: %w", err)
	}
	if err := rows.Close(); err != nil {
		return time.Time{}, fmt.Errorf("close employment staleness support: %w", err)
	}
	if len(claimKeys) == 0 {
		return time.Time{}, nil
	}
	bindings, err := loadPersonFactEmploymentOrganizationBindingsTx(ctx, tx, personID, claimKeys)
	if err != nil {
		return time.Time{}, err
	}
	matchingClaims := make(map[string]struct{})
	for _, claimKey := range claimKeys {
		organizationID, bound := bindings[claimKey]
		normalized, err := s.normalizePersonFactEmploymentSupportValueTx(
			ctx, tx, target, claimValues[claimKey], organizationID, bound)
		if errors.Is(err, errPersonFactStalenessSupportUnmatched) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if normalized.Fingerprint == fingerprint {
			matchingClaims[claimKey] = struct{}{}
		}
	}
	matchingEvidence := make([]personFactStalenessEvidenceSupport, 0)
	for _, support := range supports {
		if _, matches := matchingClaims[support.claimKey]; !matches {
			continue
		}
		matchingEvidence = append(matchingEvidence, support)
	}
	if len(matchingEvidence) == 0 {
		return time.Time{}, nil
	}
	return s.newestSupportedPersonFactEvidenceTx(ctx, tx, matchingEvidence)
}

func (s *Store) normalizePersonFactEmploymentSupportValueTx(
	ctx context.Context, tx *loggedTx, target personfacts.TargetDescriptor,
	valueJSON string, boundOrganizationID int64, bound bool,
) (personfacts.NormalizedValue, error) {
	var value personfacts.EmploymentValue
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return personfacts.NormalizedValue{},
			fmt.Errorf("decode employment staleness support value: %w", err)
	}
	var organizationID int64
	if bound {
		organizationID = boundOrganizationID
	} else if value.Organization.ID != nil {
		organizationID = *value.Organization.ID
	} else {
		_, keys, err := normalizePersonFactOrganizationReference(value.Organization)
		if err != nil {
			return personfacts.NormalizedValue{}, errPersonFactStalenessSupportUnmatched
		}
		candidates, err := s.personFactOrganizationCandidateIDsTx(ctx, tx, keys)
		if err != nil {
			return personfacts.NormalizedValue{},
				fmt.Errorf("resolve employment staleness support organization: %w", err)
		}
		if len(candidates) != 1 {
			return personfacts.NormalizedValue{}, errPersonFactStalenessSupportUnmatched
		}
		organizationID = candidates[0]
	}
	organization, err := s.personFactStalenessOrganizationTx(ctx, tx, organizationID)
	if errors.Is(err, ErrOrganizationNotFound) || errors.Is(err, ErrOrganizationInvalid) {
		return personfacts.NormalizedValue{}, errPersonFactStalenessSupportUnmatched
	}
	if err != nil {
		return personfacts.NormalizedValue{},
			fmt.Errorf("load employment staleness support organization: %w", err)
	}
	value.Organization = canonicalPersonFactOrganizationReference(*organization)
	normalized, failure, err := personfacts.NormalizeClaimValue(
		target, mustMarshalPersonFactEmployment(value))
	if err != nil {
		return personfacts.NormalizedValue{},
			fmt.Errorf("normalize employment staleness support: %w", err)
	}
	if failure != nil || normalized == nil {
		return personfacts.NormalizedValue{}, errPersonFactStalenessSupportUnmatched
	}
	return *normalized, nil
}

func (s *Store) personFactStalenessOrganizationTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*Organization, error) {
	seen := make(map[int64]struct{}, 2)
	for range maxPersonFactOrganizationRedirects {
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: organization merge redirect cycle at %d", ErrOrganizationInvalid, id)
		}
		seen[id] = struct{}{}
		organization, err := getOrganizationTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if organization.MergedIntoID == nil {
			if organization.Kind != OrganizationKindCompany || organization.RetiredAt != nil {
				return nil, fmt.Errorf("%w: organization %d is not an active company",
					ErrOrganizationInvalid, organization.ID)
			}
			return organization, nil
		}
		id = *organization.MergedIntoID
	}
	return nil, fmt.Errorf("%w: organization merge redirect exceeds %d hops",
		ErrOrganizationInvalid, maxPersonFactOrganizationRedirects-1)
}
