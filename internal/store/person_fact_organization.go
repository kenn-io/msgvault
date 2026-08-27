package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/personfacts"
)

type OrganizationResolutionStatus string

const (
	OrganizationReused    OrganizationResolutionStatus = "reused"
	OrganizationCreated   OrganizationResolutionStatus = "created"
	OrganizationAmbiguous OrganizationResolutionStatus = "ambiguous"
)

type OrganizationMatch struct {
	Organization *Organization
	Status       OrganizationResolutionStatus
	CandidateIDs []int64
	Fingerprint  string
}

type personFactOrganizationLookupKey struct {
	Kind  string `json:"Kind"`
	Value string `json:"Value"`
}

const maxPersonFactOrganizationRedirects = 64

type personFactOrganizationLockSet struct {
	discovered map[int64]struct{}
	locked     map[int64]*Organization
	candidates map[string][]int64
}

func (s *Store) preparePersonFactOrganizationTx(
	ctx context.Context, tx *loggedTx, ref personfacts.OrganizationReference,
) (OrganizationMatch, error) {
	ref, keys, err := normalizePersonFactOrganizationReference(ref)
	if err != nil {
		return OrganizationMatch{}, err
	}
	lockSet, err := s.lockPersonFactOrganizationReferencesTx(
		ctx, tx, []personfacts.OrganizationReference{ref})
	if err != nil {
		return OrganizationMatch{}, err
	}
	return s.prepareLockedPersonFactOrganizationReferenceTx(
		ctx, tx, ref, keys, lockSet, true)
}

func (s *Store) prepareLockedPersonFactOrganizationReferenceTx(
	ctx context.Context, tx *loggedTx, ref personfacts.OrganizationReference,
	keys []personFactOrganizationLookupKey, lockSet *personFactOrganizationLockSet,
	verifyReference bool,
) (OrganizationMatch, error) {
	if ref.ID != nil {
		referenced, organization, err := lockSet.canonicalOrganization(*ref.ID)
		if err != nil {
			return OrganizationMatch{}, err
		}
		if verifyReference {
			if err := s.verifyPersonFactOrganizationReferenceTx(
				ctx, tx, *referenced, ref); err != nil {
				return OrganizationMatch{}, err
			}
		}
		match := OrganizationMatch{
			Organization: organization,
			Status:       OrganizationReused,
			CandidateIDs: []int64{organization.ID},
		}
		match.Fingerprint, err = personFactOrganizationFingerprint(ref, keys, match)
		return match, err
	}

	candidateSetKey, err := personFactOrganizationCandidateSetKey(keys)
	if err != nil {
		return OrganizationMatch{}, err
	}
	candidateIDs, exists := lockSet.candidates[candidateSetKey]
	if !exists {
		return OrganizationMatch{}, errors.New(
			"person fact organization candidates were not prepared in the transaction lock set")
	}
	match := OrganizationMatch{CandidateIDs: append([]int64(nil), candidateIDs...)}
	switch len(candidateIDs) {
	case 0:
		// "created" is the durable projection-context status for a currently
		// creatable zero match. No row is written until an accepted projection
		// is materialized.
		match.Status = OrganizationCreated
	case 1:
		match.Status = OrganizationReused
		match.Organization = lockSet.locked[candidateIDs[0]]
		if match.Organization == nil {
			return OrganizationMatch{}, errors.New(
				"person fact organization candidate changed during locked resolution")
		}
	default:
		match.Status = OrganizationAmbiguous
	}
	fingerprint, err := personFactOrganizationFingerprint(ref, keys, match)
	match.Fingerprint = fingerprint
	return match, err
}

func (s *Store) materializePersonFactOrganizationTx(
	ctx context.Context, tx *loggedTx, ref personfacts.OrganizationReference,
	prepared OrganizationMatch,
) (*Organization, OrganizationResolutionStatus, error) {
	return s.materializeLockedPersonFactOrganizationTx(ctx, tx, ref, prepared, nil)
}

func (s *Store) materializeLockedPersonFactOrganizationTx(
	ctx context.Context, tx *loggedTx, ref personfacts.OrganizationReference,
	prepared OrganizationMatch, lockSet *personFactOrganizationLockSet,
) (*Organization, OrganizationResolutionStatus, error) {
	ref, keys, err := normalizePersonFactOrganizationReference(ref)
	if err != nil {
		return nil, "", err
	}
	expectedFingerprint, err := personFactOrganizationFingerprint(ref, keys, prepared)
	if err != nil {
		return nil, "", err
	}
	if prepared.Fingerprint == "" || prepared.Fingerprint != expectedFingerprint {
		return nil, "", errors.New("person fact organization match failed integrity verification")
	}
	if prepared.Status == OrganizationAmbiguous {
		return nil, OrganizationAmbiguous, nil
	}
	if prepared.Status == OrganizationReused {
		if len(prepared.CandidateIDs) != 1 {
			return nil, "", errors.New("reused person fact organization requires one candidate")
		}
		if ref.ID != nil {
			var referenced, organization *Organization
			var resolveErr error
			if lockSet != nil {
				referenced, organization, resolveErr = lockSet.canonicalOrganization(*ref.ID)
			} else {
				referenced, organization, resolveErr = s.canonicalPersonFactOrganizationTx(
					ctx, tx, *ref.ID)
			}
			if resolveErr != nil {
				return nil, "", resolveErr
			}
			if verifyErr := s.verifyPersonFactOrganizationReferenceTx(
				ctx, tx, *referenced, ref); verifyErr != nil {
				return nil, "", verifyErr
			}
			if organization.ID != prepared.CandidateIDs[0] {
				return nil, "", errors.New(
					"person fact organization redirect changed after match preparation")
			}
			return organization, OrganizationReused, nil
		}
		if lockSet != nil {
			organization := lockSet.locked[prepared.CandidateIDs[0]]
			if organization == nil {
				return nil, "", errors.New(
					"person fact organization candidate changed after match preparation")
			}
			return organization, OrganizationReused, nil
		}
		organization, loadErr := getOrganizationForUpdateTx(
			ctx, tx, s.dialect, prepared.CandidateIDs[0])
		if loadErr != nil {
			return nil, "", loadErr
		}
		return organization, OrganizationReused, nil
	}
	if prepared.Status != OrganizationCreated || len(prepared.CandidateIDs) != 0 {
		return nil, "", errors.New("invalid creatable person fact organization match")
	}

	// A prior accepted projection in this transaction may already have
	// materialized the same zero match. Requery under the lookup locks before
	// inserting so every concurrent writer converges on that row.
	candidateIDs, err := s.personFactOrganizationCandidateIDsTx(ctx, tx, keys)
	if err != nil {
		return nil, "", err
	}
	switch len(candidateIDs) {
	case 1:
		var organization *Organization
		var loadErr error
		if lockSet != nil {
			// The lookup-key advisory lock is already retained by the generation
			// scope. A row found only now was created earlier by this transaction.
			organization, loadErr = getOrganizationTx(ctx, tx, candidateIDs[0])
		} else {
			organization, loadErr = getOrganizationForUpdateTx(ctx, tx, s.dialect, candidateIDs[0])
		}
		if loadErr != nil {
			return nil, "", loadErr
		}
		return organization, OrganizationReused, nil
	case 0:
		input, validateErr := validateOrganizationInput(OrganizationInput{
			Name: ref.Name, Kind: OrganizationKindCompany,
		})
		if validateErr != nil {
			return nil, "", validateErr
		}
		if ref.Domain != "" {
			input.PrimaryDomain = &ref.Domain
		}
		organization, insertErr := scanOrganization(tx.QueryRowContext(ctx, `
			INSERT INTO organizations (
				name, name_normalized, kind, primary_domain, description
			) VALUES (?, ?, ?, ?, ?)
			RETURNING `+organizationColumns,
			input.Name, NormalizeOrganizationName(input.Name), input.Kind,
			input.PrimaryDomain, input.Description))
		if insertErr != nil {
			return nil, "", fmt.Errorf("create person fact organization: %w", insertErr)
		}
		return organization, OrganizationCreated, nil
	default:
		return nil, OrganizationAmbiguous, nil
	}
}

func (s *Store) canonicalPersonFactOrganizationTx(
	ctx context.Context, tx *loggedTx, id int64,
) (*Organization, *Organization, error) {
	lockSet, err := s.lockPersonFactOrganizationChainsTx(ctx, tx,
		[]personfacts.OrganizationReference{{ID: &id}})
	if err != nil {
		return nil, nil, err
	}
	return lockSet.canonicalOrganization(id)
}

func (s *Store) lockPersonFactOrganizationReferencesTx(
	ctx context.Context, tx *loggedTx, refs []personfacts.OrganizationReference,
) (*personFactOrganizationLockSet, error) {
	normalized := make([]personfacts.OrganizationReference, 0, len(refs))
	lookupKeys := make(map[personFactOrganizationLookupKey]struct{})
	hasNonIDReference := false
	for _, ref := range refs {
		ref, keys, err := normalizePersonFactOrganizationReference(ref)
		if err != nil {
			if errors.Is(err, ErrOrganizationInvalid) {
				continue
			}
			return nil, err
		}
		normalized = append(normalized, ref)
		hasNonIDReference = hasNonIDReference || ref.ID == nil
		for _, key := range keys {
			lookupKeys[key] = struct{}{}
		}
	}
	if err := s.lockPersonFactOrganizationTableTx(ctx, tx, hasNonIDReference); err != nil {
		return nil, err
	}
	orderedKeys := make([]personFactOrganizationLookupKey, 0, len(lookupKeys))
	for key := range lookupKeys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Slice(orderedKeys, func(i, j int) bool {
		if orderedKeys[i].Kind != orderedKeys[j].Kind {
			return orderedKeys[i].Kind < orderedKeys[j].Kind
		}
		return orderedKeys[i].Value < orderedKeys[j].Value
	})
	for _, key := range orderedKeys {
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-fact-organization", key.Kind, key.Value); err != nil {
			return nil, err
		}
	}
	return s.lockPersonFactOrganizationChainsTx(ctx, tx, normalized)
}

func (s *Store) lockPersonFactOrganizationTableForReferencesTx(
	ctx context.Context, tx *loggedTx, refs []personfacts.OrganizationReference,
) error {
	hasNonIDReference := false
	for _, ref := range refs {
		normalized, _, err := normalizePersonFactOrganizationReference(ref)
		if err != nil {
			if errors.Is(err, ErrOrganizationInvalid) {
				continue
			}
			return err
		}
		hasNonIDReference = hasNonIDReference || normalized.ID == nil
	}
	return s.lockPersonFactOrganizationTableTx(ctx, tx, hasNonIDReference)
}

func (s *Store) lockPersonFactOrganizationTableTx(
	ctx context.Context, tx *loggedTx, exclusive bool,
) error {
	if !s.IsPostgreSQL() {
		return nil
	}
	mode := "ROW SHARE"
	if exclusive {
		mode = "EXCLUSIVE"
	}
	if _, err := tx.ExecContext(ctx, "LOCK TABLE organizations IN "+mode+" MODE"); err != nil {
		return fmt.Errorf("lock person fact organization table in %s mode: %w", mode, err)
	}
	return nil
}

func (s *Store) lockPersonFactOrganizationChainsTx(
	ctx context.Context, tx *loggedTx, refs []personfacts.OrganizationReference,
) (*personFactOrganizationLockSet, error) {
	// Discover the complete batch before the first row lock. Sorting within one
	// redirect chain is insufficient: two generations can reference different
	// aliases of the same roots and otherwise acquire those roots in reverse.
	discovered := make(map[int64]struct{})
	candidates := make(map[string][]int64)
	for _, ref := range refs {
		if ref.ID == nil {
			_, keys, err := normalizePersonFactOrganizationReference(ref)
			if err != nil {
				return nil, err
			}
			candidateIDs, err := s.personFactOrganizationCandidateIDsTx(ctx, tx, keys)
			if err != nil {
				return nil, err
			}
			candidateSetKey, err := personFactOrganizationCandidateSetKey(keys)
			if err != nil {
				return nil, err
			}
			candidates[candidateSetKey] = candidateIDs
			for _, id := range candidateIDs {
				discovered[id] = struct{}{}
			}
			continue
		}
		ids, err := s.discoverPersonFactOrganizationChainTx(ctx, tx, *ref.ID)
		for _, id := range ids {
			discovered[id] = struct{}{}
		}
		// Missing and structurally invalid chains are claim-local outcomes. Keep
		// their discovered prefix in the global lock set, then classify each
		// reference from the locked snapshot after every valid chain is locked.
		if err != nil && !errors.Is(err, ErrOrganizationNotFound) &&
			!errors.Is(err, ErrOrganizationInvalid) {
			return nil, err
		}
	}
	lockIDs := make([]int64, 0, len(discovered))
	for id := range discovered {
		lockIDs = append(lockIDs, id)
	}
	slices.Sort(lockIDs)
	locked := make(map[int64]*Organization, len(lockIDs))
	for _, id := range lockIDs {
		organization, err := getOrganizationForUpdateTx(ctx, tx, s.dialect, id)
		if errors.Is(err, ErrOrganizationNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		locked[id] = organization
	}
	return &personFactOrganizationLockSet{
		discovered: discovered, locked: locked, candidates: candidates,
	}, nil
}

func (s *Store) discoverPersonFactOrganizationChainTx(
	ctx context.Context, tx *loggedTx, id int64,
) ([]int64, error) {
	ids := make([]int64, 0, 2)
	seen := make(map[int64]struct{}, 2)
	currentID := id
	for len(ids) < maxPersonFactOrganizationRedirects {
		if _, exists := seen[currentID]; exists {
			return ids, fmt.Errorf("%w: organization merge redirect cycle at %d",
				ErrOrganizationInvalid, currentID)
		}
		seen[currentID] = struct{}{}
		ids = append(ids, currentID)
		organization, err := getOrganizationTx(ctx, tx, currentID)
		if err != nil {
			return ids, err
		}
		if organization.MergedIntoID == nil {
			return ids, nil
		}
		currentID = *organization.MergedIntoID
	}
	return ids, fmt.Errorf("%w: organization merge redirect exceeds %d hops",
		ErrOrganizationInvalid, maxPersonFactOrganizationRedirects-1)
}

func (l *personFactOrganizationLockSet) canonicalOrganization(
	id int64,
) (*Organization, *Organization, error) {
	if l == nil {
		return nil, nil, errors.New("person fact organization lock set is required")
	}
	referenced := l.locked[id]
	seen := make(map[int64]struct{}, 2)
	currentID := id
	for len(seen) < maxPersonFactOrganizationRedirects {
		if _, exists := seen[currentID]; exists {
			return nil, nil, fmt.Errorf("%w: organization merge redirect cycle at %d",
				ErrOrganizationInvalid, currentID)
		}
		seen[currentID] = struct{}{}
		if _, exists := l.discovered[currentID]; !exists {
			return nil, nil, errors.New(
				"person fact organization redirect changed during resolution")
		}
		organization, exists := l.locked[currentID]
		if !exists {
			return nil, nil, ErrOrganizationNotFound
		}
		if organization.MergedIntoID == nil {
			if organization.Kind != OrganizationKindCompany || organization.RetiredAt != nil {
				return nil, nil, fmt.Errorf("%w: organization %d is not an active company",
					ErrOrganizationInvalid, organization.ID)
			}
			return referenced, organization, nil
		}
		currentID = *organization.MergedIntoID
	}
	return nil, nil, fmt.Errorf("%w: organization merge redirect exceeds %d hops",
		ErrOrganizationInvalid, maxPersonFactOrganizationRedirects-1)
}

func normalizePersonFactOrganizationReference(
	ref personfacts.OrganizationReference,
) (personfacts.OrganizationReference, []personFactOrganizationLookupKey, error) {
	ref.Name = strings.Join(strings.Fields(ref.Name), " ")
	if ref.Name == "" {
		return personfacts.OrganizationReference{}, nil,
			fmt.Errorf("%w: person fact organization name is required", ErrOrganizationInvalid)
	}
	if ref.ID != nil && *ref.ID <= 0 {
		return personfacts.OrganizationReference{}, nil,
			fmt.Errorf("%w: person fact organization id must be positive", ErrOrganizationInvalid)
	}
	if ref.Domain != "" {
		ref.Domain = NormalizeDomain(ref.Domain)
		if ref.Domain == "" {
			return personfacts.OrganizationReference{}, nil,
				fmt.Errorf("%w: person fact organization domain is invalid", ErrOrganizationInvalid)
		}
	}
	keys := make([]personFactOrganizationLookupKey, 0, 2)
	if ref.ID != nil {
		keys = append(keys, personFactOrganizationLookupKey{Kind: "id", Value: strconv.FormatInt(*ref.ID, 10)})
	} else {
		if ref.Domain != "" {
			keys = append(keys, personFactOrganizationLookupKey{Kind: "domain", Value: ref.Domain})
		}
		keys = append(keys, personFactOrganizationLookupKey{
			Kind: "name", Value: NormalizeOrganizationName(ref.Name),
		})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		return keys[i].Value < keys[j].Value
	})
	return ref, keys, nil
}

func personFactOrganizationCandidateSetKey(
	keys []personFactOrganizationLookupKey,
) (string, error) {
	encoded, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("encode person fact organization candidate set key: %w", err)
	}
	return string(encoded), nil
}

func (s *Store) personFactOrganizationCandidateIDsTx(
	ctx context.Context, tx *loggedTx, keys []personFactOrganizationLookupKey,
) ([]int64, error) {
	var candidates map[int64]struct{}
	for _, key := range keys {
		if key.Kind == "id" {
			continue
		}
		var condition string
		switch key.Kind {
		case "domain":
			condition = `(
				o.primary_domain = ? OR EXISTS (
					SELECT 1 FROM organization_identifiers identifier
					WHERE identifier.organization_id = o.id
					  AND identifier.identifier_kind = 'domain'
					  AND identifier.normalized_value = ?
					  AND identifier.active_until IS NULL
					  AND identifier.superseded_at IS NULL
				)
			)`
		case "name":
			condition = `(
				o.name_normalized = ? OR EXISTS (
					SELECT 1 FROM organization_names name
					WHERE name.organization_id = o.id
					  AND name.name_normalized = ?
					  AND name.active_until IS NULL
					  AND name.superseded_at IS NULL
				)
			)`
		default:
			return nil, fmt.Errorf("unknown person fact organization lookup kind %q", key.Kind)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT o.id FROM organizations o
			WHERE o.kind = 'company' AND o.retired_at IS NULL AND o.merged_into_id IS NULL
			  AND `+condition+`
			ORDER BY o.id`, key.Value, key.Value)
		if err != nil {
			return nil, fmt.Errorf("query person fact organization %s match: %w", key.Kind, err)
		}
		matched := make(map[int64]struct{})
		for rows.Next() {
			var id int64
			if scanErr := rows.Scan(&id); scanErr != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan person fact organization match: %w", scanErr)
			}
			matched[id] = struct{}{}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate person fact organization matches: %w", rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return nil, fmt.Errorf("close person fact organization matches: %w", closeErr)
		}
		if candidates == nil {
			candidates = matched
			continue
		}
		for id := range candidates {
			if _, exists := matched[id]; !exists {
				delete(candidates, id)
			}
		}
	}
	ids := make([]int64, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *Store) verifyPersonFactOrganizationReferenceTx(
	ctx context.Context, tx *loggedTx, organization Organization,
	ref personfacts.OrganizationReference,
) error {
	if organization.Kind != OrganizationKindCompany {
		return fmt.Errorf("%w: organization %d is not a company",
			ErrOrganizationInvalid, organization.ID)
	}
	nameMatches := NormalizeOrganizationName(organization.Name) == NormalizeOrganizationName(ref.Name)
	if !nameMatches {
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM organization_names
			WHERE organization_id = ? AND name_normalized = ?
			  AND active_until IS NULL AND superseded_at IS NULL
		)`, organization.ID, NormalizeOrganizationName(ref.Name)).Scan(&nameMatches)
		if err != nil {
			return fmt.Errorf("verify person fact organization name: %w", err)
		}
	}
	if !nameMatches {
		return fmt.Errorf("%w: organization %d name disagrees with the supplied reference",
			ErrOrganizationInvalid, organization.ID)
	}
	if ref.Domain == "" {
		return nil
	}
	domainMatches := organization.PrimaryDomain != nil && *organization.PrimaryDomain == ref.Domain
	if !domainMatches {
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM organization_identifiers
			WHERE organization_id = ? AND identifier_kind = 'domain' AND normalized_value = ?
			  AND active_until IS NULL AND superseded_at IS NULL
		)`, organization.ID, ref.Domain).Scan(&domainMatches)
		if err != nil {
			return fmt.Errorf("verify person fact organization domain: %w", err)
		}
	}
	if !domainMatches {
		return fmt.Errorf("%w: organization %d domain disagrees with the supplied reference",
			ErrOrganizationInvalid, organization.ID)
	}
	return nil
}

func personFactOrganizationFingerprint(
	ref personfacts.OrganizationReference, keys []personFactOrganizationLookupKey,
	match OrganizationMatch,
) (string, error) {
	candidateIDs := append([]int64(nil), match.CandidateIDs...)
	slices.Sort(candidateIDs)
	encoded, err := json.Marshal(struct {
		ID           *int64                            `json:"id,omitempty"`
		Name         string                            `json:"name"`
		Domain       string                            `json:"domain,omitempty"`
		LookupKeys   []personFactOrganizationLookupKey `json:"lookup_keys"`
		Status       OrganizationResolutionStatus      `json:"status"`
		CandidateIDs []int64                           `json:"candidate_ids"`
	}{
		ID: ref.ID, Name: NormalizeOrganizationName(ref.Name), Domain: ref.Domain,
		LookupKeys: append([]personFactOrganizationLookupKey(nil), keys...),
		Status:     match.Status, CandidateIDs: candidateIDs,
	})
	if err != nil {
		return "", fmt.Errorf("encode person fact organization match: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
