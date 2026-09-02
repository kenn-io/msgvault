package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

const personInferenceProfileColumns = `
	fingerprint, provider_kind, endpoint, model, api_key_env, allow_anonymous,
	auth_scheme, credential_source, credential_ref, output_mode,
	token_limit_parameter, reasoning_effort, reasoning_mode, driver_version,
	retention_posture, training_posture, CAST(allowed_sources AS TEXT),
	source_since, source_until, allow_sensitive, execution_boundary,
	packet_renderer_policy, program_fingerprint,
	CAST(disclosed_packet_fields AS TEXT), CAST(policy_json AS TEXT)`

type legacyPersonInferencePolicy struct {
	Kind                  string                    `json:"kind"`
	Endpoint              string                    `json:"endpoint"`
	Model                 string                    `json:"model"`
	APIKeyEnv             string                    `json:"api_key_env"`
	AllowAnonymous        bool                      `json:"allow_anonymous"`
	RetentionPosture      string                    `json:"retention_posture"`
	TrainingPosture       string                    `json:"training_posture"`
	AllowedSources        []peoplesweep.SourceClass `json:"allowed_sources"`
	SourceSince           string                    `json:"source_since"`
	SourceUntil           string                    `json:"source_until"`
	AllowSensitive        bool                      `json:"allow_sensitive"`
	ReasoningEffort       string                    `json:"reasoning_effort"`
	ExecutionBoundary     string                    `json:"execution_boundary"`
	PacketRendererPolicy  string                    `json:"packet_renderer_policy"`
	ProgramFingerprint    string                    `json:"program_fingerprint"`
	DisclosedPacketFields []string                  `json:"disclosed_packet_fields"`
}

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
	disclosedPacketFields, err := json.Marshal(profile.DisclosedPacketFields)
	if err != nil {
		return false, fmt.Errorf("encode people inference disclosed packet fields: %w", err)
	}
	var sourceUntil any
	if profile.SourceUntil != "" {
		sourceUntil = profile.SourceUntil
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 allow_anonymous, auth_scheme, credential_source, credential_ref,
			 output_mode, token_limit_parameter, reasoning_effort, reasoning_mode,
			 driver_version, retention_posture, training_posture, allowed_sources,
			 source_since, source_until, allow_sensitive, execution_boundary,
			 packet_renderer_policy, program_fingerprint, disclosed_packet_fields,
			 policy_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`,
		        ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, `+s.dialect.JSONBindExpr()+`)
		ON CONFLICT (fingerprint) DO NOTHING`,
		profile.Fingerprint, profile.Protocol, profile.Endpoint, profile.Model,
		profile.CredentialRef, profile.Auth == peoplesweep.AuthNone, profile.Auth,
		profile.Credential, profile.CredentialRef, profile.OutputMode,
		profile.TokenLimitParameter, profile.ReasoningEffort, profile.ReasoningMode,
		profile.DriverVersion, profile.RetentionPosture, profile.TrainingPosture,
		string(allowedSources), profile.SourceSince, sourceUntil, profile.AllowSensitive,
		profile.ExecutionBoundary, profile.PacketRendererPolicy, profile.ProgramFingerprint,
		string(disclosedPacketFields), string(profile.PolicyJSON),
	)
	if err != nil {
		return false, fmt.Errorf("insert people inference profile: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read people inference profile insert result: %w", err)
	}
	if err := s.verifyPersonInferenceProfile(
		ctx, profile, allowedSources, disclosedPacketFields,
	); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) verifyPersonInferenceProfile(
	ctx context.Context,
	profile peoplesweep.ProviderProfile,
	allowedSources []byte,
	disclosedPacketFields []byte,
) error {
	var (
		fingerprint, protocol, endpoint, model, apiKeyEnv string
		auth, credential, credentialRef                   string
		outputMode, tokenLimit, reasoningEffort           string
		reasoningMode, driverVersion                      string
		retention, training, storedSources, since         string
		executionBoundary, packetRendererPolicy           string
		programFingerprint, storedDisclosed, storedPolicy string
		anonymous, sensitive                              bool
		until                                             sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT `+personInferenceProfileColumns+`
		FROM person_inference_profiles WHERE fingerprint = ?`, profile.Fingerprint).Scan(
		&fingerprint, &protocol, &endpoint, &model, &apiKeyEnv, &anonymous,
		&auth, &credential, &credentialRef, &outputMode, &tokenLimit,
		&reasoningEffort, &reasoningMode, &driverVersion,
		&retention, &training, &storedSources, &since, &until, &sensitive,
		&executionBoundary, &packetRendererPolicy, &programFingerprint,
		&storedDisclosed, &storedPolicy,
	)
	if err != nil {
		return fmt.Errorf("read people inference profile: %w", err)
	}
	storedUntil := ""
	if until.Valid {
		storedUntil = until.String
	}
	if fingerprint != profile.Fingerprint || protocol != string(profile.Protocol) ||
		endpoint != profile.Endpoint || model != profile.Model ||
		apiKeyEnv != profile.CredentialRef || anonymous != (profile.Auth == peoplesweep.AuthNone) ||
		auth != string(profile.Auth) || credential != string(profile.Credential) ||
		credentialRef != profile.CredentialRef || outputMode != string(profile.OutputMode) ||
		tokenLimit != profile.TokenLimitParameter || reasoningEffort != profile.ReasoningEffort ||
		reasoningMode != profile.ReasoningMode || driverVersion != profile.DriverVersion ||
		retention != profile.RetentionPosture || training != profile.TrainingPosture ||
		since != profile.SourceSince || storedUntil != profile.SourceUntil ||
		sensitive != profile.AllowSensitive || executionBoundary != profile.ExecutionBoundary ||
		packetRendererPolicy != profile.PacketRendererPolicy ||
		programFingerprint != profile.ProgramFingerprint ||
		!equalJSON([]byte(storedSources), allowedSources) ||
		!equalJSON([]byte(storedDisclosed), disclosedPacketFields) ||
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
		SELECT `+personInferenceProfileColumns+`
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
		fingerprint, protocol                 string
		endpoint, model                       string
		apiKeyEnv, auth, credential           string
		credentialRef, outputMode, tokenLimit string
		reasoningEffort, reasoningMode        string
		driverVersion, retention, training    string
		sourceSince, executionBoundary        string
		packetRendererPolicy                  string
		programFingerprint                    string
		allowAnonymous, sensitive             bool
		allowedSources, disclosedFields       string
		policyJSON                            string
		sourceUntil                           sql.NullString
	)
	if err := row.Scan(
		&fingerprint, &protocol, &endpoint, &model, &apiKeyEnv, &allowAnonymous,
		&auth, &credential, &credentialRef, &outputMode, &tokenLimit,
		&reasoningEffort, &reasoningMode, &driverVersion,
		&retention, &training, &allowedSources, &sourceSince, &sourceUntil, &sensitive,
		&executionBoundary, &packetRendererPolicy, &programFingerprint,
		&disclosedFields, &policyJSON,
	); err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	var storedSources []peoplesweep.SourceClass
	if err := json.Unmarshal([]byte(allowedSources), &storedSources); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode allowed sources: %w", err)
	}
	storedUntil := ""
	if sourceUntil.Valid {
		storedUntil = sourceUntil.String
	}
	var profile peoplesweep.ProviderProfile
	if err := json.Unmarshal([]byte(policyJSON), &profile); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode people inference policy: %w", err)
	}
	if profile.Protocol == "" {
		return scanLegacyPersonInferenceProfile(
			fingerprint, protocol, endpoint, model, apiKeyEnv, allowAnonymous,
			retention, training, storedSources, sourceSince, storedUntil, sensitive,
			policyJSON,
		)
	}
	profile.Fingerprint = fingerprint
	profile.PolicyJSON = json.RawMessage(policyJSON)
	storedDisclosedFields := []string(nil)
	if err := json.Unmarshal([]byte(disclosedFields), &storedDisclosedFields); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode disclosed packet fields: %w", err)
	}
	if string(profile.Protocol) != protocol || profile.Endpoint != endpoint || profile.Model != model ||
		profile.CredentialRef != apiKeyEnv || (profile.Auth == peoplesweep.AuthNone) != allowAnonymous ||
		string(profile.Auth) != auth || string(profile.Credential) != credential ||
		profile.CredentialRef != credentialRef || string(profile.OutputMode) != outputMode ||
		profile.TokenLimitParameter != tokenLimit || profile.ReasoningEffort != reasoningEffort ||
		profile.ReasoningMode != reasoningMode || profile.DriverVersion != driverVersion ||
		profile.RetentionPosture != retention || profile.TrainingPosture != training ||
		!slices.Equal(profile.AllowedSources, storedSources) || profile.SourceSince != sourceSince ||
		profile.SourceUntil != storedUntil || profile.AllowSensitive != sensitive ||
		profile.ExecutionBoundary != executionBoundary ||
		profile.PacketRendererPolicy != packetRendererPolicy ||
		profile.ProgramFingerprint != programFingerprint ||
		!slices.Equal(profile.DisclosedPacketFields, storedDisclosedFields) {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile does not match its immutable policy")
	}
	profileName := "stored"
	provider := peoplesweep.ProviderConfig{
		Protocol: profile.Protocol, Endpoint: profile.Endpoint, Model: profile.Model,
		Auth: profile.Auth, Credential: profile.Credential, OutputMode: profile.OutputMode,
		TokenLimitParameter: profile.TokenLimitParameter, ReasoningEffort: profile.ReasoningEffort,
		ReasoningMode: profile.ReasoningMode, DriverVersion: profile.DriverVersion,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
		AllowedSources: slices.Clone(profile.AllowedSources), SourceSince: profile.SourceSince,
		SourceUntil: profile.SourceUntil, AllowSensitive: profile.AllowSensitive,
		ExecutionBoundary: profile.ExecutionBoundary, RequestTimeout: time.Second,
	}
	switch profile.Credential {
	case peoplesweep.CredentialEnv:
		provider.CredentialEnv = profile.CredentialRef
	case peoplesweep.CredentialStored:
		profileName = profile.CredentialRef
	case peoplesweep.CredentialNone:
		provider.CredentialEnv = ""
	}
	profileConfig := peoplesweep.Config{
		Enabled: true, Provider: peoplesweep.ProviderSelection{Name: profileName},
		Providers: map[string]peoplesweep.ProviderConfig{profileName: provider},
	}
	profileConfig.ApplyDefaults()
	canonical, err := profileConfig.Profile()
	if err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	if fingerprint != canonical.Fingerprint || !equalJSON([]byte(policyJSON), canonical.PolicyJSON) {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile does not match its immutable policy")
	}
	if err := canonical.Validate(); err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	return canonical, nil
}

func scanLegacyPersonInferenceProfile(
	fingerprint, kind, endpoint, model, apiKeyEnv string,
	allowAnonymous bool,
	retention, training string,
	storedSources []peoplesweep.SourceClass,
	sourceSince, sourceUntil string,
	allowSensitive bool,
	policyJSON string,
) (peoplesweep.ProviderProfile, error) {
	var legacy legacyPersonInferencePolicy
	if err := json.Unmarshal([]byte(policyJSON), &legacy); err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("decode legacy people inference policy: %w", err)
	}
	canonicalPolicy, err := json.Marshal(legacy)
	if err != nil {
		return peoplesweep.ProviderProfile{}, fmt.Errorf("encode legacy people inference policy: %w", err)
	}
	digest := sha256.Sum256(canonicalPolicy)
	if fingerprint != hex.EncodeToString(digest[:]) || !equalJSON([]byte(policyJSON), canonicalPolicy) ||
		legacy.Kind != kind || legacy.Endpoint != endpoint || legacy.Model != model ||
		legacy.APIKeyEnv != apiKeyEnv || legacy.AllowAnonymous != allowAnonymous ||
		legacy.RetentionPosture != retention || legacy.TrainingPosture != training ||
		!slices.Equal(legacy.AllowedSources, storedSources) || legacy.SourceSince != sourceSince ||
		legacy.SourceUntil != sourceUntil || legacy.AllowSensitive != allowSensitive {
		return peoplesweep.ProviderProfile{}, errors.New(
			"stored people inference profile does not match its immutable policy")
	}

	profile := peoplesweep.ProviderProfile{
		Fingerprint: fingerprint, Endpoint: legacy.Endpoint, Model: legacy.Model,
		CredentialRef: legacy.APIKeyEnv, OutputMode: peoplesweep.OutputModeNativeJSONSchema,
		ReasoningEffort: legacy.ReasoningEffort, RetentionPosture: legacy.RetentionPosture,
		TrainingPosture: legacy.TrainingPosture, AllowedSources: slices.Clone(legacy.AllowedSources),
		SourceSince: legacy.SourceSince, SourceUntil: legacy.SourceUntil,
		AllowSensitive: legacy.AllowSensitive, ExecutionBoundary: legacy.ExecutionBoundary,
		PacketRendererPolicy:  legacy.PacketRendererPolicy,
		ProgramFingerprint:    legacy.ProgramFingerprint,
		DisclosedPacketFields: slices.Clone(legacy.DisclosedPacketFields),
		PolicyJSON:            canonicalPolicy,
	}
	providerName := "legacy"
	switch legacy.Kind {
	case peoplesweep.ProviderOpenAICompatible:
		profile.Protocol = peoplesweep.ProtocolOpenAIChat
		profile.TokenLimitParameter = "max_completion_tokens"
		if legacy.AllowAnonymous {
			profile.Auth = peoplesweep.AuthNone
			profile.Credential = peoplesweep.CredentialNone
			profile.CredentialRef = ""
		} else {
			profile.Auth = peoplesweep.AuthBearer
			profile.Credential = peoplesweep.CredentialEnv
		}
	case peoplesweep.ProviderCodexAppServer:
		profile.Protocol = peoplesweep.ProtocolCodexAppServer
		profile.Auth = peoplesweep.AuthNone
		profile.Credential = peoplesweep.CredentialNone
		profile.CredentialRef = ""
	default:
		return peoplesweep.ProviderProfile{}, fmt.Errorf(
			"unsupported legacy people inference provider kind %q", legacy.Kind)
	}
	provider := peoplesweep.ProviderConfig{
		Protocol: profile.Protocol, Endpoint: profile.Endpoint, Model: profile.Model,
		Auth: profile.Auth, Credential: profile.Credential, CredentialEnv: profile.CredentialRef,
		OutputMode: profile.OutputMode, TokenLimitParameter: profile.TokenLimitParameter,
		ReasoningEffort: profile.ReasoningEffort, RetentionPosture: profile.RetentionPosture,
		TrainingPosture: profile.TrainingPosture, AllowedSources: slices.Clone(profile.AllowedSources),
		SourceSince: profile.SourceSince, SourceUntil: profile.SourceUntil,
		AllowSensitive: profile.AllowSensitive, ExecutionBoundary: profile.ExecutionBoundary,
		RequestTimeout: time.Second,
	}
	config := peoplesweep.Config{
		Enabled: true, Provider: peoplesweep.ProviderSelection{Name: providerName},
		Providers: map[string]peoplesweep.ProviderConfig{providerName: provider},
	}
	config.ApplyDefaults()
	if err := config.Validate(); err != nil {
		return peoplesweep.ProviderProfile{}, err
	}
	if legacy.Kind == peoplesweep.ProviderOpenAICompatible &&
		(legacy.ReasoningEffort != "" || legacy.ExecutionBoundary != "") {
		return peoplesweep.ProviderProfile{}, errors.New(
			"legacy openai_compatible policy contains Codex-only fields")
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
