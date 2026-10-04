package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConsentPurpose names one exact egress purpose. A grant authorizes only the
// purpose it was recorded under, even when fingerprints coincide.
type ConsentPurpose string

const (
	ConsentPeopleInference         ConsentPurpose = "people_inference"
	ConsentPersonEnrichment        ConsentPurpose = "person_enrichment"
	ConsentPersonSemanticEmbedding ConsentPurpose = "person_semantic_embedding"
)

var consentPurposes = map[ConsentPurpose]struct{ profileTable, label, legacyTable string }{
	ConsentPeopleInference:         {"person_inference_profiles", "people inference", "person_inference_consents"},
	ConsentPersonEnrichment:        {"person_enrichment_profiles", "person enrichment", "person_enrichment_consents"},
	ConsentPersonSemanticEmbedding: {"person_semantic_embedding_profiles", "semantic person embedding", "person_semantic_embedding_consents"},
}

var errConsentChangedConcurrent = errors.New("provider consent changed concurrently")

// ProviderConsent is one preserved grant and its optional revocation.
type ProviderConsent struct {
	ID                 int64      `json:"id"`
	ProfileFingerprint string     `json:"profile_fingerprint"`
	GrantedBy          string     `json:"granted_by"`
	GrantedAt          time.Time  `json:"granted_at"`
	RevokedBy          *string    `json:"revoked_by,omitzero" nullable:"false"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
}

// ProviderConsentStatus reports authority for one exact purpose and
// fingerprint without exposing any credential value.
type ProviderConsentStatus struct {
	Fingerprint   string           `json:"fingerprint"`
	ProfileExists bool             `json:"profile_exists"`
	Active        bool             `json:"active"`
	Consent       *ProviderConsent `json:"consent,omitzero" nullable:"false"`
	LastRevoked   *ProviderConsent `json:"last_revoked,omitzero" nullable:"false"`
}

type (
	PersonInferenceConsent        = ProviderConsent
	PersonInferenceConsentStatus  = ProviderConsentStatus
	PersonEnrichmentConsent       = ProviderConsent
	PersonEnrichmentConsentStatus = ProviderConsentStatus
)

const providerConsentColumns = `id, fingerprint, granted_by, granted_at, revoked_by, revoked_at`

// activeConsentsSQL is a derived table of active grants for one purpose.
// purpose comes only from the constants above, never from input.
func activeConsentsSQL(purpose ConsentPurpose) string { //nolint:unparam // each join names its purpose so a new purpose cannot inherit another's grants
	return `(SELECT id, fingerprint FROM provider_consents WHERE purpose = '` +
		string(purpose) + `' AND revoked_at IS NULL)`
}

func validateConsentFingerprint(purpose ConsentPurpose, fingerprint string) error {
	if !validLowerSHA256(fingerprint) {
		return fmt.Errorf("%s consent requires a lowercase SHA-256 fingerprint", consentPurposes[purpose].label)
	}
	return nil
}

func validateConsentInput(purpose ConsentPurpose, fingerprint, actor string) (string, error) {
	if err := validateConsentFingerprint(purpose, fingerprint); err != nil {
		return "", err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", fmt.Errorf("%s consent actor is required", consentPurposes[purpose].label)
	}
	return actor, nil
}

func consentProfileExists(
	ctx context.Context, q contextStatementQuerier, purpose ConsentPurpose, fingerprint string,
) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+
		consentPurposes[purpose].profileTable+` WHERE fingerprint = ?)`, fingerprint).Scan(&exists)
	return exists, err
}

// grantConsentOnce inserts the next per-purpose ID. A concurrent grant for the
// same fingerprint or a colliding ID leaves the insert empty; the active row,
// if any, is then returned as an idempotent success.
func grantConsentOnce(
	ctx context.Context, q contextStatementQuerier, purpose ConsentPurpose, fingerprint, actor string,
) (*ProviderConsent, bool, error) {
	label := consentPurposes[purpose].label
	consent, err := scanProviderConsent(q.QueryRowContext(ctx, `
		INSERT INTO provider_consents (purpose, id, fingerprint, granted_by)
		SELECT ?, COALESCE(MAX(id), 0) + 1, ?, ? FROM provider_consents WHERE purpose = ?
		ON CONFLICT DO NOTHING
		RETURNING `+providerConsentColumns,
		string(purpose), fingerprint, actor, string(purpose)))
	if err == nil {
		return consent, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("grant %s consent: %w", label, err)
	}
	consent, err = activeConsent(ctx, q, purpose, fingerprint)
	if err == nil {
		return consent, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read active %s consent: %w", label, err)
	}
	return nil, false, errConsentChangedConcurrent
}

// grantConsent grants one exact existing profile outside a transaction.
func (s *Store) grantConsent(
	ctx context.Context, purpose ConsentPurpose, fingerprint, actor string,
) (*ProviderConsent, bool, error) {
	actor, err := validateConsentInput(purpose, fingerprint, actor)
	if err != nil {
		return nil, false, err
	}
	label := consentPurposes[purpose].label
	exists, err := consentProfileExists(ctx, s.db, purpose, fingerprint)
	if err != nil {
		return nil, false, fmt.Errorf("check %s profile: %w", label, err)
	}
	if !exists {
		return nil, false, fmt.Errorf("%s consent profile does not exist", label)
	}
	for range 3 {
		consent, created, err := grantConsentOnce(ctx, s.db, purpose, fingerprint, actor)
		if !errors.Is(err, errConsentChangedConcurrent) {
			return consent, created, err
		}
	}
	return nil, false, fmt.Errorf("%s consent changed concurrently; retry", label)
}

// revokeConsent stamps the current exact grant. Missing or already-revoked
// consent is an idempotent no-op.
func revokeConsent(
	ctx context.Context, q contextStatementQuerier, purpose ConsentPurpose, fingerprint, actor string,
) (bool, error) {
	actor, err := validateConsentInput(purpose, fingerprint, actor)
	if err != nil {
		return false, err
	}
	var id int64
	err = q.QueryRowContext(ctx, `
		UPDATE provider_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE purpose = ? AND fingerprint = ? AND revoked_at IS NULL
		RETURNING id`, actor, string(purpose), fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke %s consent: %w", consentPurposes[purpose].label, err)
	}
	return true, nil
}

// revokeAllConsents stamps every active grant for one purpose, including
// grants for profiles no longer present in the runtime configuration.
func revokeAllConsents(
	ctx context.Context, q contextStatementQuerier, purpose ConsentPurpose, actor string,
) (int64, error) {
	label := consentPurposes[purpose].label
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return 0, fmt.Errorf("%s consent actor is required", label)
	}
	result, err := q.ExecContext(ctx, `
		UPDATE provider_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE purpose = ? AND revoked_at IS NULL`, actor, string(purpose))
	if err != nil {
		return 0, fmt.Errorf("revoke all %s consents: %w", label, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revoked %s consent count: %w", label, err)
	}
	return changed, nil
}

func activeConsentFingerprints(ctx context.Context, tx *loggedTx, purpose ConsentPurpose) ([]string, error) {
	label := consentPurposes[purpose].label
	rows, err := tx.QueryContext(ctx, `SELECT fingerprint FROM provider_consents
		WHERE purpose = ? AND revoked_at IS NULL ORDER BY fingerprint`, string(purpose))
	if err != nil {
		return nil, fmt.Errorf("list active %s consents: %w", label, err)
	}
	defer func() { _ = rows.Close() }()
	fingerprints := make([]string, 0)
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			return nil, fmt.Errorf("read active %s consent: %w", label, err)
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active %s consents: %w", label, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close active %s consents: %w", label, err)
	}
	return fingerprints, nil
}

func (s *Store) hasActiveConsent(ctx context.Context, purpose ConsentPurpose, fingerprint string) (bool, error) {
	if err := validateConsentFingerprint(purpose, fingerprint); err != nil {
		return false, err
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM provider_consents
		WHERE purpose = ? AND fingerprint = ? AND revoked_at IS NULL)`,
		string(purpose), fingerprint).Scan(&active)
	if legacy := consentPurposes[purpose].legacyTable; err != nil && s.onlyLegacyConsents(ctx, legacy) {
		// A read-only open skips migration, so an old archive keeps grants here.
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+legacy+`
			WHERE profile_fingerprint = ? AND revoked_at IS NULL)`, fingerprint).Scan(&active)
	}
	if err != nil {
		return false, fmt.Errorf("check active %s consent: %w", consentPurposes[purpose].label, err)
	}
	return active, nil
}

func (s *Store) onlyLegacyConsents(ctx context.Context, legacy string) bool {
	current, err := s.tableExistsContext(ctx, "provider_consents")
	if err != nil || current {
		return false
	}
	old, err := s.tableExistsContext(ctx, legacy)
	return err == nil && old
}

// lockActiveConsentTx reads the active grant under a row lock so a commit
// that depends on it linearizes with a concurrent revoke.
func lockActiveConsentTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, purpose ConsentPurpose, fingerprint string,
) (bool, error) {
	if err := validateConsentFingerprint(purpose, fingerprint); err != nil {
		return false, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM provider_consents
		WHERE purpose = ? AND fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`+dialect.SelectForUpdate(), string(purpose), fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check active %s consent in transaction: %w", consentPurposes[purpose].label, err)
	}
	return id > 0, nil
}

// consentStatus reports exact current and historical state for one purpose.
func consentStatus(
	ctx context.Context, q contextStatementQuerier, purpose ConsentPurpose, fingerprint string,
) (*ProviderConsentStatus, error) {
	if err := validateConsentFingerprint(purpose, fingerprint); err != nil {
		return nil, err
	}
	label := consentPurposes[purpose].label
	status := &ProviderConsentStatus{Fingerprint: fingerprint}
	exists, err := consentProfileExists(ctx, q, purpose, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("check %s profile status: %w", label, err)
	}
	status.ProfileExists = exists
	if !exists {
		return status, nil
	}
	active, err := activeConsent(ctx, q, purpose, fingerprint)
	if err == nil {
		status.Active = true
		status.Consent = active
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read active %s consent status: %w", label, err)
	}
	lastRevoked, err := scanProviderConsent(q.QueryRowContext(ctx, `
		SELECT `+providerConsentColumns+` FROM provider_consents
		WHERE purpose = ? AND fingerprint = ? AND revoked_at IS NOT NULL
		ORDER BY revoked_at DESC, id DESC LIMIT 1`, string(purpose), fingerprint))
	if err == nil {
		status.LastRevoked = lastRevoked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read revoked %s consent status: %w", label, err)
	}
	return status, nil
}

func activeConsent(
	ctx context.Context, q contextStatementQuerier, purpose ConsentPurpose, fingerprint string,
) (*ProviderConsent, error) {
	return scanProviderConsent(q.QueryRowContext(ctx, `
		SELECT `+providerConsentColumns+` FROM provider_consents
		WHERE purpose = ? AND fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`, string(purpose), fingerprint))
}

func scanProviderConsent(row scanner) (*ProviderConsent, error) {
	var (
		consent              ProviderConsent
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
		return nil, errors.New("provider consent has invalid granted_at")
	}
	consent.GrantedAt = grantedAt.Time
	if revokedBy.Valid {
		value := revokedBy.String
		consent.RevokedBy = &value
	}
	consent.RevokedAt = optionalTimestamp(revokedAt)
	return &consent, nil
}
