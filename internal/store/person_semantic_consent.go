package store

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
)

func (s *Store) EnsurePersonSemanticEmbeddingProfile(
	ctx context.Context,
	profile vector.SemanticPersonEmbeddingProfile,
) (bool, error) {
	canonical, err := profile.Canonical()
	if err != nil {
		return false, err
	}
	disclosed, err := json.Marshal(canonical.DisclosedFieldClasses, json.Deterministic(true))
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
	profile.PolicyJSON = jsontext.Value(policy)
	canonical, err := profile.Canonical()
	if err != nil {
		return vector.SemanticPersonEmbeddingProfile{}, fmt.Errorf(
			"stored semantic person embedding profile does not match its immutable policy: %w", err)
	}
	return canonical, nil
}

func (s *Store) GrantPersonSemanticEmbeddingConsent(ctx context.Context, fingerprint, actor string) (*ProviderConsent, bool, error) {
	return s.grantConsent(ctx, ConsentPersonSemanticEmbedding, fingerprint, actor)
}

func (s *Store) RevokePersonSemanticEmbeddingConsent(ctx context.Context, fingerprint, actor string) (bool, error) {
	return revokeConsent(ctx, s.db, ConsentPersonSemanticEmbedding, fingerprint, actor)
}

func (s *Store) RevokeAllPersonSemanticEmbeddingConsents(ctx context.Context, actor string) (int64, error) {
	return revokeAllConsents(ctx, s.db, ConsentPersonSemanticEmbedding, actor)
}

func (s *Store) HasActivePersonSemanticEmbeddingConsent(ctx context.Context, fingerprint string) (bool, error) {
	return s.hasActiveConsent(ctx, ConsentPersonSemanticEmbedding, fingerprint)
}

func (s *Store) GetPersonSemanticEmbeddingConsentStatus(ctx context.Context, fingerprint string) (*ProviderConsentStatus, error) {
	return consentStatus(ctx, s.db, ConsentPersonSemanticEmbedding, fingerprint)
}
