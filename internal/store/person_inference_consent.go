package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// PersonInferenceConsent is one preserved grant and its optional revocation.
type PersonInferenceConsent struct {
	ID                 int64      `json:"id"`
	ProfileFingerprint string     `json:"profile_fingerprint"`
	GrantedBy          string     `json:"granted_by"`
	GrantedAt          time.Time  `json:"granted_at"`
	RevokedBy          *string    `json:"revoked_by,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
}

// PersonInferenceConsentStatus reports authority for one exact runtime
// fingerprint without exposing any credential value.
type PersonInferenceConsentStatus struct {
	Fingerprint   string                  `json:"fingerprint"`
	ProfileExists bool                    `json:"profile_exists"`
	Active        bool                    `json:"active"`
	Consent       *PersonInferenceConsent `json:"consent,omitempty"`
	LastRevoked   *PersonInferenceConsent `json:"last_revoked,omitempty"`
}

const personInferenceConsentColumns = `
	id, profile_fingerprint, granted_by, granted_at, revoked_by, revoked_at`

// EnsurePersonInferenceProfile persists one immutable canonical policy or
// verifies the already-stored row has the same content.
func (s *Store) EnsurePersonInferenceProfile(
	ctx context.Context,
	profile peoplesweep.ProviderProfile,
) (bool, error) {
	if err := profile.Validate(); err != nil {
		return false, err
	}
	allowedSources, err := json.Marshal(profile.AllowedSources)
	if err != nil {
		return false, fmt.Errorf("encode people inference allowed sources: %w", err)
	}
	var sourceUntil any
	if profile.SourceUntil != "" {
		sourceUntil = profile.SourceUntil
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 allow_anonymous, retention_posture, training_posture,
			 allowed_sources, source_since, source_until, allow_sensitive,
			 policy_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, `+s.dialect.JSONBindExpr()+`)
		ON CONFLICT (fingerprint) DO NOTHING`,
		profile.Fingerprint, profile.Kind, profile.Endpoint, profile.Model,
		profile.APIKeyEnv, profile.AllowAnonymous, profile.RetentionPosture,
		profile.TrainingPosture, string(allowedSources), profile.SourceSince,
		sourceUntil, profile.AllowSensitive, string(profile.PolicyJSON),
	)
	if err != nil {
		return false, fmt.Errorf("insert people inference profile: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read people inference profile insert result: %w", err)
	}
	if err := s.verifyPersonInferenceProfile(ctx, profile, allowedSources); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) verifyPersonInferenceProfile(
	ctx context.Context,
	profile peoplesweep.ProviderProfile,
	allowedSources []byte,
) error {
	var (
		fingerprint, kind, endpoint, model, apiKeyEnv string
		retention, training, storedSources, since     string
		storedPolicy                                  string
		anonymous, sensitive                          bool
		until                                         sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT fingerprint, provider_kind, endpoint, model, api_key_env,
		       allow_anonymous, retention_posture, training_posture,
		       CAST(allowed_sources AS TEXT), source_since, source_until,
		       allow_sensitive, CAST(policy_json AS TEXT)
		FROM person_inference_profiles WHERE fingerprint = ?`, profile.Fingerprint).Scan(
		&fingerprint, &kind, &endpoint, &model, &apiKeyEnv, &anonymous,
		&retention, &training, &storedSources, &since, &until, &sensitive,
		&storedPolicy,
	)
	if err != nil {
		return fmt.Errorf("read people inference profile: %w", err)
	}
	storedUntil := ""
	if until.Valid {
		storedUntil = until.String
	}
	if fingerprint != profile.Fingerprint || kind != profile.Kind ||
		endpoint != profile.Endpoint || model != profile.Model ||
		apiKeyEnv != profile.APIKeyEnv || anonymous != profile.AllowAnonymous ||
		retention != profile.RetentionPosture || training != profile.TrainingPosture ||
		since != profile.SourceSince || storedUntil != profile.SourceUntil ||
		sensitive != profile.AllowSensitive ||
		!equalJSON([]byte(storedSources), allowedSources) ||
		!equalJSON([]byte(storedPolicy), profile.PolicyJSON) {
		return errors.New("people inference profile fingerprint already has different immutable policy")
	}
	return nil
}

// ListPersonInferenceProfiles returns every immutable policy that has been
// persisted for consent. It does not depend on the current runtime policy, so
// operators can audit and revoke old grants after configuration changes.
func (s *Store) ListPersonInferenceProfiles(
	ctx context.Context,
) ([]peoplesweep.ProviderProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fingerprint, provider_kind, endpoint, model, api_key_env,
		       allow_anonymous, retention_posture, training_posture,
		       CAST(allowed_sources AS TEXT), source_since, source_until,
		       allow_sensitive, CAST(policy_json AS TEXT)
		FROM person_inference_profiles
		ORDER BY fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("list people inference profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	profiles := make([]peoplesweep.ProviderProfile, 0)
	for rows.Next() {
		profile, scanErr := scanPersonInferenceProfile(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("read people inference profile: %w", scanErr)
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list people inference profiles: %w", err)
	}
	return profiles, nil
}

// GrantPersonInferenceConsent grants one exact existing profile. An already
// active grant is returned as an idempotent success.
func (s *Store) GrantPersonInferenceConsent(
	ctx context.Context,
	fingerprint, actor string,
) (*PersonInferenceConsent, bool, error) {
	actor, err := validatePersonInferenceConsentInput(fingerprint, actor)
	if err != nil {
		return nil, false, err
	}
	var profileExists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM person_inference_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&profileExists); err != nil {
		return nil, false, fmt.Errorf("check people inference profile: %w", err)
	}
	if !profileExists {
		return nil, false, errors.New("people inference consent profile does not exist")
	}

	for range 3 {
		consent, insertErr := scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
			INSERT INTO person_inference_consents
				(profile_fingerprint, granted_by)
			VALUES (?, ?)
			ON CONFLICT DO NOTHING
			RETURNING `+personInferenceConsentColumns,
			fingerprint, actor,
		))
		if insertErr == nil {
			return consent, true, nil
		}
		if !errors.Is(insertErr, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("grant people inference consent: %w", insertErr)
		}
		consent, readErr := s.activePersonInferenceConsent(ctx, fingerprint)
		if readErr == nil {
			return consent, false, nil
		}
		if !errors.Is(readErr, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("read active people inference consent: %w", readErr)
		}
	}
	return nil, false, errors.New("people inference consent changed concurrently; retry")
}

// RevokePersonInferenceConsent stamps the current exact grant. Missing or
// already-revoked consent is an idempotent no-op.
func (s *Store) RevokePersonInferenceConsent(
	ctx context.Context,
	fingerprint, actor string,
) (bool, error) {
	actor, err := validatePersonInferenceConsentInput(fingerprint, actor)
	if err != nil {
		return false, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
		UPDATE person_inference_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		RETURNING id`, actor, fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke people inference consent: %w", err)
	}
	return true, nil
}

// RevokeAllPersonInferenceConsents stamps every active grant, including grants
// for policies that are no longer present in the runtime configuration.
func (s *Store) RevokeAllPersonInferenceConsents(
	ctx context.Context,
	actor string,
) (int64, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return 0, errors.New("people inference consent actor is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE person_inference_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE revoked_at IS NULL`, actor)
	if err != nil {
		return 0, fmt.Errorf("revoke all people inference consents: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revoked people inference consent count: %w", err)
	}
	return changed, nil
}

// HasActivePersonInferenceConsent implements the runner's narrow privacy gate.
func (s *Store) HasActivePersonInferenceConsent(
	ctx context.Context,
	fingerprint string,
) (bool, error) {
	if !validLowerSHA256(fingerprint) {
		return false, errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM person_inference_consents
			WHERE profile_fingerprint = ? AND revoked_at IS NULL
		)`, fingerprint).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active people inference consent: %w", err)
	}
	return active, nil
}

func (s *Store) hasActivePersonInferenceConsentTx(
	ctx context.Context, tx *loggedTx, fingerprint string,
) (bool, error) {
	if !validLowerSHA256(fingerprint) {
		return false, errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM person_inference_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`+s.dialect.SelectForUpdate(), fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check active people inference consent in transaction: %w", err)
	}
	return id > 0, nil
}

// GetPersonInferenceConsentStatus reports exact current and historical state.
func (s *Store) GetPersonInferenceConsentStatus(
	ctx context.Context,
	fingerprint string,
) (*PersonInferenceConsentStatus, error) {
	if !validLowerSHA256(fingerprint) {
		return nil, errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	status := &PersonInferenceConsentStatus{Fingerprint: fingerprint}
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM person_inference_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&status.ProfileExists); err != nil {
		return nil, fmt.Errorf("check people inference profile status: %w", err)
	}
	if !status.ProfileExists {
		return status, nil
	}
	active, err := s.activePersonInferenceConsent(ctx, fingerprint)
	if err == nil {
		status.Active = true
		status.Consent = active
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read active people inference consent status: %w", err)
	}
	lastRevoked, err := scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personInferenceConsentColumns+`
		FROM person_inference_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NOT NULL
		ORDER BY revoked_at DESC, id DESC LIMIT 1`, fingerprint))
	if err == nil {
		status.LastRevoked = lastRevoked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read revoked people inference consent status: %w", err)
	}
	return status, nil
}

func (s *Store) activePersonInferenceConsent(
	ctx context.Context,
	fingerprint string,
) (*PersonInferenceConsent, error) {
	return scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personInferenceConsentColumns+`
		FROM person_inference_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`, fingerprint))
}

func scanPersonInferenceConsent(row scanner) (*PersonInferenceConsent, error) {
	var (
		consent              PersonInferenceConsent
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
		return nil, errors.New("people inference consent has invalid granted_at")
	}
	consent.GrantedAt = grantedAt.Time
	if revokedBy.Valid {
		value := revokedBy.String
		consent.RevokedBy = &value
	}
	consent.RevokedAt = optionalTimestamp(revokedAt)
	return &consent, nil
}

func scanPersonInferenceProfile(row scanner) (peoplesweep.ProviderProfile, error) {
	var (
		profile                    peoplesweep.ProviderProfile
		allowedSources, policyJSON string
		sourceUntil                sql.NullString
	)
	if err := row.Scan(
		&profile.Fingerprint, &profile.Kind, &profile.Endpoint, &profile.Model,
		&profile.APIKeyEnv, &profile.AllowAnonymous, &profile.RetentionPosture,
		&profile.TrainingPosture, &allowedSources, &profile.SourceSince,
		&sourceUntil, &profile.AllowSensitive, &policyJSON,
	); err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	if sourceUntil.Valid {
		profile.SourceUntil = sourceUntil.String
	}
	if err := json.Unmarshal([]byte(allowedSources), &profile.AllowedSources); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode allowed sources: %w", err)
	}
	var codexPolicy struct {
		ReasoningEffort   string `json:"reasoning_effort"`
		ExecutionBoundary string `json:"execution_boundary"`
	}
	if err := json.Unmarshal([]byte(policyJSON), &codexPolicy); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode people inference policy: %w", err)
	}
	profileConfig := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderConfig{
		Kind: profile.Kind, Endpoint: profile.Endpoint, Model: profile.Model,
		APIKeyEnv: profile.APIKeyEnv, AllowAnonymous: profile.AllowAnonymous,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
		AllowedSources: profile.AllowedSources, SourceSince: profile.SourceSince,
		SourceUntil: profile.SourceUntil, AllowSensitive: profile.AllowSensitive,
		ReasoningEffort: codexPolicy.ReasoningEffort, ExecutionBoundary: codexPolicy.ExecutionBoundary,
		RequestTimeout: time.Second,
	}}
	profileConfig.ApplyDefaults()
	canonical, err := profileConfig.Profile()
	if err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	if profile.Fingerprint != canonical.Fingerprint ||
		!equalJSON([]byte(policyJSON), canonical.PolicyJSON) {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile does not match its immutable policy")
	}
	profile = canonical
	if err := profile.Validate(); err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	return profile, nil
}

func validatePersonInferenceConsentInput(fingerprint, actor string) (string, error) {
	if !validLowerSHA256(fingerprint) {
		return "", errors.New("people inference consent requires a lowercase SHA-256 fingerprint")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("people inference consent actor is required")
	}
	return actor, nil
}
