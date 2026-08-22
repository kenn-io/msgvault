package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
)

// PersonSemanticEmbeddingConsent is one preserved semantic-person grant and
// its optional revocation. It intentionally shares the established audit
// shape without sharing the people-sweep authorization namespace.
type PersonSemanticEmbeddingConsent = PersonInferenceConsent

// PersonSemanticEmbeddingConsentStatus reports authority for one exact
// semantic-person profile.
type PersonSemanticEmbeddingConsentStatus = PersonInferenceConsentStatus

const personSemanticEmbeddingConsentColumns = `
	id, profile_fingerprint, granted_by, granted_at, revoked_by, revoked_at`

func (s *Store) EnsurePersonSemanticEmbeddingProfile(
	ctx context.Context,
	profile vector.SemanticPersonEmbeddingProfile,
) (bool, error) {
	canonical, err := profile.Canonical()
	if err != nil {
		return false, err
	}
	disclosed, err := json.Marshal(canonical.DisclosedFieldClasses)
	if err != nil {
		return false, fmt.Errorf("encode semantic person disclosed fields: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO person_semantic_embedding_profiles
			(fingerprint, purpose, destination, api_format, model, api_key_env,
			 retention_posture, training_posture, renderer_policy,
			 disclosed_field_classes, corpus_scope, policy_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, `+s.dialect.JSONBindExpr()+`)
		ON CONFLICT (fingerprint) DO NOTHING`,
		canonical.Fingerprint, canonical.Purpose, canonical.Destination,
		canonical.APIFormat, canonical.Model, canonical.APIKeyEnv,
		canonical.RetentionPosture, canonical.TrainingPosture,
		canonical.RendererPolicy, string(disclosed), canonical.CorpusScope,
		string(canonical.PolicyJSON),
	)
	if err != nil {
		return false, fmt.Errorf("insert semantic person embedding profile: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read semantic person embedding profile insert result: %w", err)
	}
	if err := s.verifyPersonSemanticEmbeddingProfile(ctx, canonical, disclosed); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) verifyPersonSemanticEmbeddingProfile(
	ctx context.Context,
	profile vector.SemanticPersonEmbeddingProfile,
	disclosed []byte,
) error {
	var (
		fingerprint, purpose, destination, apiFormat, model, apiKeyEnv string
		retention, training, renderer, storedDisclosed, scope          string
		storedPolicy                                                   string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT fingerprint, purpose, destination, api_format, model,
		       api_key_env, retention_posture, training_posture,
		       renderer_policy, CAST(disclosed_field_classes AS TEXT),
		       corpus_scope, CAST(policy_json AS TEXT)
		FROM person_semantic_embedding_profiles WHERE fingerprint = ?`,
		profile.Fingerprint,
	).Scan(
		&fingerprint, &purpose, &destination, &apiFormat, &model, &apiKeyEnv,
		&retention, &training, &renderer, &storedDisclosed, &scope, &storedPolicy,
	)
	if err != nil {
		return fmt.Errorf("read semantic person embedding profile: %w", err)
	}
	if fingerprint != profile.Fingerprint || purpose != profile.Purpose ||
		destination != profile.Destination || apiFormat != string(profile.APIFormat) ||
		model != profile.Model || apiKeyEnv != profile.APIKeyEnv ||
		retention != profile.RetentionPosture || training != profile.TrainingPosture ||
		renderer != profile.RendererPolicy || scope != profile.CorpusScope ||
		!equalJSON([]byte(storedDisclosed), disclosed) ||
		!equalJSON([]byte(storedPolicy), profile.PolicyJSON) {
		return errors.New("semantic person embedding profile fingerprint already has different immutable policy")
	}
	return nil
}

func (s *Store) ListPersonSemanticEmbeddingProfiles(
	ctx context.Context,
) ([]vector.SemanticPersonEmbeddingProfile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fingerprint, purpose, destination, api_format, model,
		       api_key_env, retention_posture, training_posture,
		       renderer_policy, CAST(disclosed_field_classes AS TEXT),
		       corpus_scope, CAST(policy_json AS TEXT)
		FROM person_semantic_embedding_profiles
		ORDER BY fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("list semantic person embedding profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	profiles := make([]vector.SemanticPersonEmbeddingProfile, 0)
	for rows.Next() {
		profile, scanErr := scanPersonSemanticEmbeddingProfile(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("read semantic person embedding profile: %w", scanErr)
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list semantic person embedding profiles: %w", err)
	}
	return profiles, nil
}

func (s *Store) GrantPersonSemanticEmbeddingConsent(
	ctx context.Context,
	fingerprint, actor string,
) (*PersonSemanticEmbeddingConsent, bool, error) {
	actor, err := validatePersonSemanticEmbeddingConsentInput(fingerprint, actor)
	if err != nil {
		return nil, false, err
	}
	var profileExists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_semantic_embedding_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&profileExists); err != nil {
		return nil, false, fmt.Errorf("check semantic person embedding profile: %w", err)
	}
	if !profileExists {
		return nil, false, errors.New("semantic person embedding consent profile does not exist")
	}

	for range 3 {
		consent, insertErr := scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
			INSERT INTO person_semantic_embedding_consents
				(profile_fingerprint, granted_by)
			VALUES (?, ?)
			ON CONFLICT DO NOTHING
			RETURNING `+personSemanticEmbeddingConsentColumns,
			fingerprint, actor,
		))
		if insertErr == nil {
			return consent, true, nil
		}
		if !errors.Is(insertErr, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("grant semantic person embedding consent: %w", insertErr)
		}
		consent, readErr := s.activePersonSemanticEmbeddingConsent(ctx, fingerprint)
		if readErr == nil {
			return consent, false, nil
		}
		if !errors.Is(readErr, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("read active semantic person embedding consent: %w", readErr)
		}
	}
	return nil, false, errors.New("semantic person embedding consent changed concurrently; retry")
}

func (s *Store) RevokePersonSemanticEmbeddingConsent(
	ctx context.Context,
	fingerprint, actor string,
) (bool, error) {
	actor, err := validatePersonSemanticEmbeddingConsentInput(fingerprint, actor)
	if err != nil {
		return false, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
		UPDATE person_semantic_embedding_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		RETURNING id`, actor, fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke semantic person embedding consent: %w", err)
	}
	return true, nil
}

func (s *Store) RevokeAllPersonSemanticEmbeddingConsents(
	ctx context.Context,
	actor string,
) (int64, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return 0, errors.New("semantic person embedding consent actor is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE person_semantic_embedding_consents
		SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
		WHERE revoked_at IS NULL`, actor)
	if err != nil {
		return 0, fmt.Errorf("revoke all semantic person embedding consents: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revoked semantic person embedding consent count: %w", err)
	}
	return changed, nil
}

func (s *Store) HasActivePersonSemanticEmbeddingConsent(
	ctx context.Context,
	fingerprint string,
) (bool, error) {
	if !validLowerSHA256(fingerprint) {
		return false, errors.New("semantic person embedding consent requires a lowercase SHA-256 fingerprint")
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_semantic_embedding_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL)`,
		fingerprint,
	).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active semantic person embedding consent: %w", err)
	}
	return active, nil
}

func (s *Store) GetPersonSemanticEmbeddingConsentStatus(
	ctx context.Context,
	fingerprint string,
) (*PersonSemanticEmbeddingConsentStatus, error) {
	if !validLowerSHA256(fingerprint) {
		return nil, errors.New("semantic person embedding consent requires a lowercase SHA-256 fingerprint")
	}
	status := &PersonSemanticEmbeddingConsentStatus{Fingerprint: fingerprint}
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_semantic_embedding_profiles WHERE fingerprint = ?)`,
		fingerprint,
	).Scan(&status.ProfileExists); err != nil {
		return nil, fmt.Errorf("check semantic person embedding profile status: %w", err)
	}
	if !status.ProfileExists {
		return status, nil
	}
	active, err := s.activePersonSemanticEmbeddingConsent(ctx, fingerprint)
	if err == nil {
		status.Active = true
		status.Consent = active
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read active semantic person embedding consent status: %w", err)
	}
	lastRevoked, err := scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personSemanticEmbeddingConsentColumns+`
		FROM person_semantic_embedding_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NOT NULL
		ORDER BY revoked_at DESC, id DESC LIMIT 1`, fingerprint))
	if err == nil {
		status.LastRevoked = lastRevoked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read revoked semantic person embedding consent status: %w", err)
	}
	return status, nil
}

func (s *Store) activePersonSemanticEmbeddingConsent(
	ctx context.Context,
	fingerprint string,
) (*PersonSemanticEmbeddingConsent, error) {
	return scanPersonInferenceConsent(s.db.QueryRowContext(ctx, `
		SELECT `+personSemanticEmbeddingConsentColumns+`
		FROM person_semantic_embedding_consents
		WHERE profile_fingerprint = ? AND revoked_at IS NULL
		ORDER BY id DESC LIMIT 1`, fingerprint))
}

func scanPersonSemanticEmbeddingProfile(row scanner) (
	vector.SemanticPersonEmbeddingProfile,
	error,
) {
	var (
		profile                      vector.SemanticPersonEmbeddingProfile
		apiFormat, disclosed, policy string
	)
	if err := row.Scan(
		&profile.Fingerprint, &profile.Purpose, &profile.Destination, &apiFormat,
		&profile.Model, &profile.APIKeyEnv, &profile.RetentionPosture,
		&profile.TrainingPosture, &profile.RendererPolicy, &disclosed,
		&profile.CorpusScope, &policy,
	); err != nil {
		return vector.SemanticPersonEmbeddingProfile{}, err
	}
	profile.APIFormat = vector.EmbeddingAPIFormat(apiFormat)
	if err := json.Unmarshal([]byte(disclosed), &profile.DisclosedFieldClasses); err != nil {
		return vector.SemanticPersonEmbeddingProfile{}, fmt.Errorf("decode disclosed fields: %w", err)
	}
	profile.PolicyJSON = json.RawMessage(policy)
	canonical, err := profile.Canonical()
	if err != nil {
		return vector.SemanticPersonEmbeddingProfile{}, fmt.Errorf(
			"stored semantic person embedding profile does not match its immutable policy: %w", err)
	}
	return canonical, nil
}

func validatePersonSemanticEmbeddingConsentInput(fingerprint, actor string) (string, error) {
	if !validLowerSHA256(fingerprint) {
		return "", errors.New("semantic person embedding consent requires a lowercase SHA-256 fingerprint")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("semantic person embedding consent actor is required")
	}
	return actor, nil
}
