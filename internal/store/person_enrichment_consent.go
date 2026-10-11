package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/personenrichment"
)

// EnsurePersonEnrichmentProfile inserts one immutable canonical policy or
// verifies that the row already stored under its fingerprint is identical.
func (s *Store) EnsurePersonEnrichmentProfile(
	ctx context.Context,
	profile personenrichment.ProviderProfile,
) (bool, error) {
	if err := profile.Validate(); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO person_enrichment_profiles
			(fingerprint, provider_name, provider_kind, provider_namespace,
			 endpoint, api_key_env, policy_json)
		VALUES (?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`)
		ON CONFLICT (fingerprint) DO NOTHING`,
		profile.Fingerprint, profile.Name, profile.Kind, profile.ProviderNamespace,
		profile.Endpoint, profile.APIKeyEnv, string(profile.PolicyJSON),
	)
	if err != nil {
		return false, fmt.Errorf("insert person enrichment profile: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read person enrichment profile insert result: %w", err)
	}
	if err := s.verifyPersonEnrichmentProfile(ctx, profile); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) verifyPersonEnrichmentProfile(
	ctx context.Context,
	profile personenrichment.ProviderProfile,
) error {
	var fingerprint, name, kind, namespace, endpoint, apiKeyEnv, policyJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT fingerprint, provider_name, provider_kind, provider_namespace,
		       endpoint, api_key_env, CAST(policy_json AS TEXT)
		FROM person_enrichment_profiles WHERE fingerprint = ?`, profile.Fingerprint).Scan(
		&fingerprint, &name, &kind, &namespace, &endpoint, &apiKeyEnv, &policyJSON,
	)
	if err != nil {
		return fmt.Errorf("read person enrichment profile: %w", err)
	}
	if fingerprint != profile.Fingerprint || name != profile.Name || kind != profile.Kind ||
		namespace != profile.ProviderNamespace || endpoint != profile.Endpoint ||
		apiKeyEnv != profile.APIKeyEnv || !equalJSON([]byte(policyJSON), profile.PolicyJSON) {
		return errors.New("person enrichment profile fingerprint already has different immutable policy")
	}
	return nil
}

// ListPersonEnrichmentProfilesContext returns immutable policies in stable
// fingerprint order. Profiles contain configuration metadata but no credential
// values.
func (s *Store) ListPersonEnrichmentProfilesContext(
	ctx context.Context,
) ([]personenrichment.ProviderProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fingerprint FROM person_enrichment_profiles ORDER BY fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	fingerprints := make([]string, 0)
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			return nil, fmt.Errorf("read person enrichment profile fingerprint: %w", err)
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person enrichment profiles: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close person enrichment profiles: %w", err)
	}
	profiles := make([]personenrichment.ProviderProfile, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		profile, err := s.loadPersonEnrichmentProfile(ctx, s.db, fingerprint, false)
		if err != nil {
			return nil, fmt.Errorf("load person enrichment profile %q: %w", fingerprint, err)
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// GrantPersonEnrichmentConsent grants one exact existing profile. An already
// active grant is returned as an idempotent success.
func (s *Store) GrantPersonEnrichmentConsent(
	ctx context.Context,
	fingerprint, actor string,
) (*ProviderConsent, bool, error) {
	actor, err := validateConsentInput(ConsentPersonEnrichment, fingerprint, actor)
	if err != nil {
		return nil, false, err
	}
	const maxAttempts = 5
	for range maxAttempts {
		consent, created, grantErr := s.grantPersonEnrichmentConsentOnce(ctx, fingerprint, actor)
		if grantErr == nil {
			return consent, created, nil
		}
		if !errors.Is(grantErr, errConsentChangedConcurrent) &&
			!s.dialect.IsBusyError(grantErr) {
			return nil, false, grantErr
		}
	}
	return nil, false, fmt.Errorf(
		"grant person enrichment consent: gave up after %d contention retries", maxAttempts)
}

func (s *Store) grantPersonEnrichmentConsentOnce(
	ctx context.Context, fingerprint, actor string,
) (*ProviderConsent, bool, error) {
	var consent *ProviderConsent
	created := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		profileExists, err := consentProfileExists(ctx, tx, ConsentPersonEnrichment, fingerprint)
		if err != nil {
			return fmt.Errorf("check person enrichment profile: %w", err)
		}
		if !profileExists {
			return errors.New("person enrichment consent profile does not exist")
		}
		consent, created, err = grantConsentOnce(ctx, tx, ConsentPersonEnrichment, fingerprint, actor)
		if err == nil && created {
			generation := "consent:" + strconv.FormatInt(consent.ID, 10)
			dueAt := s.personEnrichmentTime()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO person_enrichment_work
					(person_id, profile_fingerprint, trigger_mask, trigger_generation, due_at)
				SELECT pt.person_id, ?, 1, ?, ? FROM person_tracking pt WHERE 1 = 1`+
				personEnrichmentWorkConflictClause,
				fingerprint, generation, dueAt); err != nil {
				return fmt.Errorf("publish person enrichment consent work: %w", err)
			}
		}
		return err
	})
	return consent, created, err
}

// RevokePersonEnrichmentConsent stamps the current exact grant. Missing or
// already-revoked consent is an idempotent no-op.
func (s *Store) RevokePersonEnrichmentConsent(
	ctx context.Context,
	fingerprint, actor string,
) (bool, error) {
	actor, err := validateConsentInput(ConsentPersonEnrichment, fingerprint, actor)
	if err != nil {
		return false, err
	}
	changed, err := retryContendedWrite(ctx, s, "revoke person enrichment consent", func() (*bool, error) {
		result, revokeErr := s.revokePersonEnrichmentConsentOnce(ctx, fingerprint, actor)
		return &result, revokeErr
	})
	if err != nil {
		return false, err
	}
	return *changed, nil
}

func (s *Store) revokePersonEnrichmentConsentOnce(
	ctx context.Context, fingerprint, actor string,
) (bool, error) {
	changed := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		var err error
		changed, err = s.revokePersonEnrichmentConsentTx(ctx, tx, fingerprint, actor)
		return err
	})
	return changed, err
}

func (s *Store) revokePersonEnrichmentConsentTx(
	ctx context.Context, tx *loggedTx, fingerprint, actor string,
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id FROM persons p
			WHERE EXISTS (SELECT 1 FROM person_tracking pt WHERE pt.person_id = p.id)
			   OR EXISTS (SELECT 1 FROM person_enrichment_work w
			              WHERE w.person_id = p.id AND w.profile_fingerprint = ?)
			ORDER BY p.id`, fingerprint)
	if err != nil {
		return false, fmt.Errorf("list people affected by person enrichment consent revocation: %w", err)
	}
	personIDs := make([]int64, 0)
	for rows.Next() {
		var personID int64
		if err := rows.Scan(&personID); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("read person affected by person enrichment consent revocation: %w", err)
		}
		personIDs = append(personIDs, personID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate people affected by person enrichment consent revocation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close people affected by person enrichment consent revocation: %w", err)
	}
	if s.personEnrichmentTxBarrier != nil {
		s.personEnrichmentTxBarrier("revoke_affected_people_snapshotted")
	}
	for _, personID := range personIDs {
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("revoke_before_person_lock")
		}
		if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, personID); err != nil {
			return false, err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("revoke_person_locked")
		}
	}
	revoked, err := revokeConsent(ctx, tx, ConsentPersonEnrichment, fingerprint, actor)
	if err != nil || !revoked {
		return false, err
	}
	if s.personEnrichmentTxBarrier != nil {
		s.personEnrichmentTxBarrier("revoke_authority_removed")
	}
	for _, personID := range personIDs {
		if err := s.cancelPersonEnrichmentTx(ctx, tx, personID, fingerprint); err != nil {
			return false, err
		}
	}
	return true, nil
}

// RevokeAllPersonEnrichmentConsents revokes each currently active exact
// policy through the same cancellation path as an individual revocation.
func (s *Store) RevokeAllPersonEnrichmentConsents(ctx context.Context, actor string) (int64, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return 0, errors.New("person enrichment consent actor is required")
	}
	revoked, err := retryContendedWrite(ctx, s, "revoke all person enrichment consents", func() (*int64, error) {
		count, revokeErr := s.revokeAllPersonEnrichmentConsentsOnce(ctx, actor)
		return &count, revokeErr
	})
	if err != nil {
		return 0, err
	}
	return *revoked, nil
}

func (s *Store) revokeAllPersonEnrichmentConsentsOnce(
	ctx context.Context, actor string,
) (int64, error) {
	var revoked int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		fingerprints, err := activeConsentFingerprints(ctx, tx, ConsentPersonEnrichment)
		if err != nil {
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("revoke_all_consents_snapshotted")
		}
		for _, fingerprint := range fingerprints {
			changed, err := s.revokePersonEnrichmentConsentTx(ctx, tx, fingerprint, actor)
			if err != nil {
				return err
			}
			if changed {
				revoked++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return revoked, nil
}

// PersonEnrichmentConsentStatus reports exact current and historical state.
func (s *Store) PersonEnrichmentConsentStatus(ctx context.Context, fingerprint string) (*ProviderConsentStatus, error) {
	return consentStatus(ctx, s.db, ConsentPersonEnrichment, fingerprint)
}

// HasActivePersonEnrichmentConsent is the narrow exact-purpose egress gate.
func (s *Store) HasActivePersonEnrichmentConsent(ctx context.Context, fingerprint string) (bool, error) {
	return s.hasActiveConsent(ctx, ConsentPersonEnrichment, fingerprint)
}

func validPersonEnrichmentProviderNamespace(namespace, kind string) bool {
	prefix, fingerprint, ok := strings.Cut(namespace, ":")
	return ok && prefix == kind && validLowerSHA256(fingerprint)
}
