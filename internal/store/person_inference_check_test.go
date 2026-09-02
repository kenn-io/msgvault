package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonInferenceProviderV2PersistsCanonicalFields(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)

	var (
		protocol, apiKeyEnv, auth, credential, credentialRef   string
		outputMode, tokenLimit, reasoningEffort, reasoningMode string
		driverVersion, executionBoundary, rendererPolicy       string
		programFingerprint, disclosedFields                    string
		allowAnonymous                                         bool
	)
	err = st.DB().QueryRow(st.Rebind(`
		SELECT provider_kind, api_key_env, allow_anonymous, auth_scheme,
		       credential_source, credential_ref, output_mode,
		       token_limit_parameter, reasoning_effort, reasoning_mode,
		       driver_version, execution_boundary, packet_renderer_policy,
		       program_fingerprint, CAST(disclosed_packet_fields AS TEXT)
		FROM person_inference_profiles WHERE fingerprint = ?`), profile.Fingerprint).Scan(
		&protocol, &apiKeyEnv, &allowAnonymous, &auth, &credential, &credentialRef,
		&outputMode, &tokenLimit, &reasoningEffort, &reasoningMode, &driverVersion,
		&executionBoundary, &rendererPolicy, &programFingerprint, &disclosedFields,
	)
	requirements.NoError(err)
	checks.Equal(string(profile.Protocol), protocol)
	checks.Equal(profile.CredentialRef, apiKeyEnv)
	checks.Equal(profile.Auth == peoplesweep.AuthNone, allowAnonymous)
	checks.Equal(string(profile.Auth), auth)
	checks.Equal(string(profile.Credential), credential)
	checks.Equal(profile.CredentialRef, credentialRef)
	checks.Equal(string(profile.OutputMode), outputMode)
	checks.Equal(profile.TokenLimitParameter, tokenLimit)
	checks.Equal(profile.ReasoningEffort, reasoningEffort)
	checks.Equal(profile.ReasoningMode, reasoningMode)
	checks.Equal(profile.DriverVersion, driverVersion)
	checks.Equal(profile.ExecutionBoundary, executionBoundary)
	checks.Equal(profile.PacketRendererPolicy, rendererPolicy)
	checks.Equal(profile.ProgramFingerprint, programFingerprint)
	checks.JSONEq(`[
		"person_id", "program_identity", "catalog", "current_projection",
		"unresolved_claims", "seed_evidence", "retrieved_context"
	]`, disclosedFields)

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_inference_profiles SET reasoning_mode = 'tampered'
		WHERE fingerprint = ?`), profile.Fingerprint)
	requirements.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.ErrorContains(err, "different immutable policy")

	var credentialReference string
	err = st.DB().QueryRow(st.Rebind(`
		SELECT credential_ref FROM person_inference_profiles WHERE fingerprint = ?`),
		profile.Fingerprint).Scan(&credentialReference)
	requirements.NoError(err)
	checks.Equal("TEST_KEY", credentialReference, "only the environment variable name is persisted")
}

func TestPersonInferenceCheckPinsAndReplacesExactProfile(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)

	checkedAt := time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC)
	first := store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint,
		CheckedAt:          checkedAt,
		DriverVersion:      profile.DriverVersion,
		OutputMode:         profile.OutputMode,
		ProviderRequestID:  "request-one",
		ModelVersion:       "model-one",
	}
	requirements.NoError(st.RecordPersonInferenceCheck(t.Context(), first))

	got, err := st.GetPersonInferenceCheck(t.Context(), profile.Fingerprint)
	requirements.NoError(err)
	checks.Equal(first, *got)
	verified, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
	requirements.NoError(err)
	checks.True(verified)

	replacement := first
	replacement.CheckedAt = checkedAt.Add(time.Minute)
	replacement.ProviderRequestID = "request-two"
	replacement.ModelVersion = "model-two"
	requirements.NoError(st.RecordPersonInferenceCheck(t.Context(), replacement))
	got, err = st.GetPersonInferenceCheck(t.Context(), profile.Fingerprint)
	requirements.NoError(err)
	checks.Equal(replacement, *got)

	var rows int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM person_inference_checks
		WHERE profile_fingerprint = ?`), profile.Fingerprint).Scan(&rows))
	checks.Equal(1, rows)
}

func TestPersonInferenceCheckRejectsUnknownProfileAndUnsafeMetadata(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	base := store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint,
		CheckedAt:          time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC),
		DriverVersion:      profile.DriverVersion,
		OutputMode:         profile.OutputMode,
		ProviderRequestID:  "request-safe",
		ModelVersion:       "model-safe",
	}

	err := st.RecordPersonInferenceCheck(t.Context(), base)
	requirements.ErrorContains(err, "does not exist")

	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)
	for _, test := range []struct {
		name   string
		mutate func(*store.PersonInferenceCheck)
	}{
		{"fingerprint", func(check *store.PersonInferenceCheck) { check.ProfileFingerprint = "not-a-fingerprint" }},
		{"checked_at", func(check *store.PersonInferenceCheck) { check.CheckedAt = time.Time{} }},
		{"driver_version", func(check *store.PersonInferenceCheck) { check.DriverVersion = "unsafe\nversion" }},
		{"output_mode", func(check *store.PersonInferenceCheck) { check.OutputMode = peoplesweep.OutputMode("unknown") }},
		{"provider_request_id", func(check *store.PersonInferenceCheck) { check.ProviderRequestID = strings.Repeat("x", 129) }},
		{"model_version", func(check *store.PersonInferenceCheck) { check.ModelVersion = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			err := st.RecordPersonInferenceCheck(t.Context(), candidate)
			require.Error(t, err)
		})
	}
}

func TestPersonInferenceCheckLookupRequiresExactFingerprint(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	verifiedProfile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), verifiedProfile)
	requirements.NoError(err)
	requirements.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: verifiedProfile.Fingerprint,
		CheckedAt:          time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC),
		DriverVersion:      verifiedProfile.DriverVersion,
		OutputMode:         verifiedProfile.OutputMode,
		ModelVersion:       "model-safe",
	}))

	otherConfig := peoplesweep.Config{
		Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "other"},
		Providers: map[string]peoplesweep.ProviderConfig{"other": {
			Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
			Model: "gpt-other", Auth: peoplesweep.AuthBearer,
			Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
			TokenLimitParameter: "max_completion_tokens", RetentionPosture: "zero_retention",
			TrainingPosture: "no_training",
			AllowedSources:  []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			SourceSince:     "2025-01-01", RequestTimeout: time.Minute,
		}},
	}
	otherConfig.ApplyDefaults()
	otherProfile, err := otherConfig.Profile()
	requirements.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), otherProfile)
	requirements.NoError(err)

	got, err := st.GetPersonInferenceCheck(t.Context(), otherProfile.Fingerprint)
	requirements.NoError(err)
	checks.Nil(got)
	verified, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), otherProfile.Fingerprint)
	requirements.NoError(err)
	checks.False(verified)
}

func TestPersonInferenceCheckRejectsStoredProfileMetadataMismatch(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint,
		CheckedAt:          time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC),
		DriverVersion:      profile.DriverVersion,
		OutputMode:         profile.OutputMode,
		ModelVersion:       "model-safe",
	}))
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_inference_checks SET driver_version = 'different-safe-driver'
		WHERE profile_fingerprint = ?`), profile.Fingerprint)
	require.NoError(err)

	verified, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.ErrorContains(err, "immutable provider profile")
	assert.False(t, verified)
}
