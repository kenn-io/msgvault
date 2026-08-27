package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personenrichment"
)

// PersonEnrichmentConsent is one preserved grant and its optional revocation.
type PersonEnrichmentConsent struct {
	ID                 int64      `json:"id"`
	ProfileFingerprint string     `json:"profile_fingerprint"`
	GrantedBy          string     `json:"granted_by"`
	GrantedAt          time.Time  `json:"granted_at"`
	RevokedBy          *string    `json:"revoked_by,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
}

// PersonEnrichmentConsentStatus reports authority for one exact enrichment
// policy fingerprint without exposing credential values.
type PersonEnrichmentConsentStatus struct {
	Fingerprint   string                   `json:"fingerprint"`
	ProfileExists bool                     `json:"profile_exists"`
	Active        bool                     `json:"active"`
	Consent       *PersonEnrichmentConsent `json:"consent,omitempty"`
	LastRevoked   *PersonEnrichmentConsent `json:"last_revoked,omitempty"`
}

const personEnrichmentConsentColumns = `
	id, profile_fingerprint, granted_by, granted_at, revoked_by, revoked_at`

var errPersonEnrichmentConsentChangedConcurrent = errors.New(
	"person enrichment consent changed concurrently")

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
) (*PersonEnrichmentConsent, bool, error) {
	actor, err := validatePersonEnrichmentConsentInput(fingerprint, actor)
	if err != nil {
		return nil, false, err
	}
	const maxAttempts = 5
	for range maxAttempts {
		consent, created, grantErr := s.grantPersonEnrichmentConsentOnce(ctx, fingerprint, actor)
		if grantErr == nil {
			return consent, created, nil
		}
		if !errors.Is(grantErr, errPersonEnrichmentConsentChangedConcurrent) &&
			!s.dialect.IsBusyError(grantErr) {
			return nil, false, grantErr
		}
	}
	return nil, false, fmt.Errorf(
		"grant person enrichment consent: gave up after %d contention retries", maxAttempts)
}

func (s *Store) grantPersonEnrichmentConsentOnce(
	ctx context.Context, fingerprint, actor string,
) (*PersonEnrichmentConsent, bool, error) {
	var consent *PersonEnrichmentConsent
	created := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		var profileExists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM person_enrichment_profiles WHERE fingerprint = ?)`,
			fingerprint,
		).Scan(&profileExists); err != nil {
			return fmt.Errorf("check person enrichment profile: %w", err)
		}
		if !profileExists {
			return errors.New("person enrichment consent profile does not exist")
		}
		var insertErr error
		consent, insertErr = scanPersonEnrichmentConsent(tx.QueryRowContext(ctx, `
			INSERT INTO person_enrichment_consents
				(profile_fingerprint, granted_by)
			VALUES (?, ?)
			ON CONFLICT DO NOTHING
			RETURNING `+personEnrichmentConsentColumns,
			fingerprint, actor,
		))
		if insertErr == nil {
			created = true
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
			return nil
		}
		if !errors.Is(insertErr, sql.ErrNoRows) {
			return fmt.Errorf("grant person enrichment consent: %w", insertErr)
		}
		var readErr error
		consent, readErr = scanPersonEnrichmentConsent(tx.QueryRowContext(ctx, `
			SELECT `+personEnrichmentConsentColumns+`
			FROM person_enrichment_consents
			WHERE profile_fingerprint = ? AND revoked_at IS NULL
			ORDER BY id DESC LIMIT 1`, fingerprint))
		if readErr == nil {
			return nil
		}
		if !errors.Is(readErr, sql.ErrNoRows) {
			return fmt.Errorf("read active person enrichment consent: %w", readErr)
		}
		return errPersonEnrichmentConsentChangedConcurrent
	})
	return consent, created, err
}

// RevokePersonEnrichmentConsent stamps the current exact grant. Missing or
// already-revoked consent is an idempotent no-op.
func (s *Store) RevokePersonEnrichmentConsent(
	ctx context.Context,
	fingerprint, actor string,
) (bool, error) {
	actor, err := validatePersonEnrichmentConsentInput(fingerprint, actor)
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
	var id int64
	err = tx.QueryRowContext(ctx, `
			UPDATE person_enrichment_consents
			SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
			WHERE profile_fingerprint = ? AND revoked_at IS NULL
			RETURNING id`, actor, fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke person enrichment consent: %w", err)
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
		rows, err := tx.QueryContext(ctx, `SELECT profile_fingerprint
			FROM person_enrichment_consents WHERE revoked_at IS NULL
			ORDER BY profile_fingerprint`)
		if err != nil {
			return fmt.Errorf("list active person enrichment consents: %w", err)
		}
		fingerprints := make([]string, 0)
		for rows.Next() {
			var fingerprint string
			if err := rows.Scan(&fingerprint); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read active person enrichment consent: %w", err)
			}
			fingerprints = append(fingerprints, fingerprint)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate active person enrichment consents: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close active person enrichment consents: %w", err)
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
func (s *Store) PersonEnrichmentConsentStatus(
	ctx context.Context,
	fingerprint string,
) (*PersonEnrichmentConsentStatus, error) {
	if !validLowerSHA256(fingerprint) {
		return nil, errors.New("person enrichment consent requires a lowercase SHA-256 fingerprint")
	}
	status := &PersonEnrichmentConsentStatus{Fingerprint: fingerprint}
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM person_enrichment_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&status.ProfileExists); err != nil {
		return nil, fmt.Errorf("check person enrichment profile status: %w", err)
	}
	if !status.ProfileExists {
		return status, nil
	}
	active, err := s.activePersonEnrichmentConsent(ctx, fingerprint)
	if err == nil {
		status.Active = true
		status.Consent = active
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read active person enrichment consent status: %w", err)
	}
	lastRevoked, err := scanPersonEnrichmentConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personEnrichmentConsentColumns+`
		FROM person_enrichment_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NOT NULL
		ORDER BY revoked_at DESC, id DESC LIMIT 1`, fingerprint))
	if err == nil {
		status.LastRevoked = lastRevoked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read revoked person enrichment consent status: %w", err)
	}
	return status, nil
}

// HasActivePersonEnrichmentConsent is the narrow exact-purpose egress gate.
func (s *Store) HasActivePersonEnrichmentConsent(
	ctx context.Context,
	fingerprint string,
) (bool, error) {
	if !validLowerSHA256(fingerprint) {
		return false, errors.New("person enrichment consent requires a lowercase SHA-256 fingerprint")
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM person_enrichment_consents
			WHERE profile_fingerprint = ? AND revoked_at IS NULL
		)`, fingerprint).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active person enrichment consent: %w", err)
	}
	return active, nil
}

func (s *Store) activePersonEnrichmentConsent(
	ctx context.Context,
	fingerprint string,
) (*PersonEnrichmentConsent, error) {
	return scanPersonEnrichmentConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personEnrichmentConsentColumns+`
		FROM person_enrichment_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`, fingerprint))
}

func scanPersonEnrichmentConsent(row scanner) (*PersonEnrichmentConsent, error) {
	var (
		consent              PersonEnrichmentConsent
		grantedAt, revokedAt nullableTimestamp
		revokedBy            sql.NullString
	)
	if err := row.Scan(
		&consent.ID, &consent.ProfileFingerprint, &consent.GrantedBy,
		&grantedAt, &revokedBy, &revokedAt,
	); err != nil {
		return nil, err
	}
	if !grantedAt.Valid {
		return nil, errors.New("person enrichment consent has invalid granted_at")
	}
	consent.GrantedAt = grantedAt.Time
	if revokedBy.Valid {
		value := revokedBy.String
		consent.RevokedBy = &value
	}
	consent.RevokedAt = optionalTimestamp(revokedAt)
	return &consent, nil
}

func validatePersonEnrichmentConsentInput(fingerprint, actor string) (string, error) {
	if !validLowerSHA256(fingerprint) {
		return "", errors.New("person enrichment consent requires a lowercase SHA-256 fingerprint")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("person enrichment consent actor is required")
	}
	return actor, nil
}

func validPersonEnrichmentProviderNamespace(namespace, kind string) bool {
	prefix, fingerprint, ok := strings.Cut(namespace, ":")
	return ok && prefix == kind && validLowerSHA256(fingerprint)
}
