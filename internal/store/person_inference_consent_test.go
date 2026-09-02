package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func inferenceTestProfile(t *testing.T) peoplesweep.ProviderProfile {
	t.Helper()
	config := peoplesweep.Config{
		Enabled:  true,
		Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol:            peoplesweep.ProtocolOpenAIChat,
			Endpoint:            "https://api.example.test/v1",
			Model:               "gpt-test",
			Auth:                peoplesweep.AuthBearer,
			Credential:          peoplesweep.CredentialEnv,
			CredentialEnv:       "TEST_KEY",
			OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
			TokenLimitParameter: "max_completion_tokens",
			RetentionPosture:    "zero_retention",
			TrainingPosture:     "no_training",
			AllowedSources: []peoplesweep.SourceClass{
				peoplesweep.SourceConversationText,
			},
			SourceSince:    "2025-01-01",
			RequestTimeout: time.Minute,
		}},
	}
	config.ApplyDefaults()
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}

func TestPersonInferenceConsentLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)

	status, err := st.GetPersonInferenceConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.Equal(profile.Fingerprint, status.Fingerprint)
	assert.False(status.ProfileExists)
	assert.False(status.Active)
	assert.Nil(status.Consent)

	created, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	assert.True(created)
	created, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	assert.False(created)

	consent, created, err := st.GrantPersonInferenceConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.True(created)
	assert.Equal(profile.Fingerprint, consent.ProfileFingerprint)
	assert.Equal("cli", consent.GrantedBy)
	assert.Nil(consent.RevokedAt)

	again, created, err := st.GrantPersonInferenceConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.False(created)
	assert.Equal(consent.ID, again.ID)

	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(active)

	changed, err := st.RevokePersonInferenceConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.True(changed)
	changed, err = st.RevokePersonInferenceConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.False(changed)

	status, err = st.GetPersonInferenceConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(status.ProfileExists)
	assert.False(status.Active)
	assert.Nil(status.Consent)
	require.NotNil(status.LastRevoked)
	assert.Equal(consent.ID, status.LastRevoked.ID)
	require.NotNil(status.LastRevoked.RevokedAt)
	require.NotNil(status.LastRevoked.RevokedBy)
	assert.Equal("cli", *status.LastRevoked.RevokedBy)

	second, created, err := st.GrantPersonInferenceConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.True(created)
	assert.NotEqual(consent.ID, second.ID)
	active, err = st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(active)

	var history int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM person_inference_consents
		WHERE profile_fingerprint = ?`), profile.Fingerprint).Scan(&history))
	assert.Equal(2, history)
}

func TestPersonInferenceProfileRejectsMismatchAndUnknownConsent(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)

	_, _, err := st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.ErrorContains(err, "does not exist")
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, " ")
	require.ErrorContains(err, "actor")
	_, err = st.RevokePersonInferenceConsent(t.Context(), profile.Fingerprint, "")
	require.ErrorContains(err, "actor")

	tampered := profile
	tampered.Model = "different"
	_, err = st.EnsurePersonInferenceProfile(t.Context(), tampered)
	require.ErrorContains(err, "fingerprint")

	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)

	_, _, err = st.GrantPersonInferenceConsent(t.Context(), "not-a-fingerprint", "cli")
	require.ErrorContains(err, "fingerprint")
	_, err = st.RevokePersonInferenceConsent(t.Context(), "not-a-fingerprint", "cli")
	require.ErrorContains(err, "fingerprint")
}

func TestPersonInferenceProfilesCanBeListedAndRevokedWithoutRuntimeConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	var policy map[string]any
	require.NoError(json.Unmarshal(profile.PolicyJSON, &policy))
	normalizedPolicy, err := json.Marshal(policy)
	require.NoError(err)
	require.NotEqual(string(profile.PolicyJSON), string(normalizedPolicy))
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_inference_profiles SET policy_json = ? WHERE fingerprint = ?`),
		string(normalizedPolicy), profile.Fingerprint)
	require.NoError(err)

	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	require.NoError(err)
	require.Len(profiles, 1)
	assert.Equal(profile.Fingerprint, profiles[0].Fingerprint)
	assert.JSONEq(string(profile.PolicyJSON), string(profiles[0].PolicyJSON))
	assert.Equal(string(profile.PolicyJSON), string(profiles[0].PolicyJSON))

	changed, err := st.RevokeAllPersonInferenceConsents(t.Context(), "cli")
	require.NoError(err)
	assert.Equal(int64(1), changed)
	changed, err = st.RevokeAllPersonInferenceConsents(t.Context(), "cli")
	require.NoError(err)
	assert.Zero(changed)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(active)
}

func TestPersonInferenceProfilesRestoreCodexPolicyFields(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	config := peoplesweep.Config{
		Enabled:  true,
		Provider: peoplesweep.ProviderSelection{Name: "codex"},
		Providers: map[string]peoplesweep.ProviderConfig{"codex": {
			Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test",
			Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
			OutputMode:      peoplesweep.OutputModeNativeJSONSchema,
			ReasoningEffort: "high", ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
			RetentionPosture: "zero_retention", TrainingPosture: "no_training",
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
		}},
	}
	config.ApplyDefaults()
	profile, err := config.Profile()
	requirements.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)

	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	requirements.NoError(err)
	requirements.Len(profiles, 1)
	checks.Equal(profile, profiles[0])
}

func TestPersonInferenceProfilesLoadLegacyPersistedPolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	const legacyProgramFingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
	policyJSON := fmt.Sprintf(`{"kind":"openai_compatible","endpoint":"https://api.example.test/v1","model":"gpt-legacy","api_key_env":"LEGACY_KEY","allow_anonymous":false,"retention_posture":"zero_retention","training_posture":"no_training","allowed_sources":["conversation_text"],"source_since":"2025-01-01","source_until":"","allow_sensitive":false,"reasoning_effort":"","execution_boundary":"","packet_renderer_policy":"person-sweep-packet-v1","program_fingerprint":"%s","disclosed_packet_fields":["person_id","program_identity","catalog","current_projection","unresolved_claims","seed_evidence","retrieved_context"]}`,
		legacyProgramFingerprint)
	digest := sha256.Sum256([]byte(policyJSON))
	fingerprint := hex.EncodeToString(digest[:])

	_, err := st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 allow_anonymous, retention_posture, training_posture,
			 allowed_sources, source_since, source_until, allow_sensitive,
			 policy_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`),
		fingerprint, peoplesweep.ProviderOpenAICompatible,
		"https://api.example.test/v1", "gpt-legacy", "LEGACY_KEY", false,
		"zero_retention", "no_training", `["conversation_text"]`,
		"2025-01-01", false, policyJSON)
	require.NoError(err)

	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	require.NoError(err)
	require.Len(profiles, 1)
	profile := profiles[0]
	assert.Equal(fingerprint, profile.Fingerprint)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, profile.Protocol)
	assert.Equal(peoplesweep.AuthBearer, profile.Auth)
	assert.Equal(peoplesweep.CredentialEnv, profile.Credential)
	assert.Equal("LEGACY_KEY", profile.CredentialRef)
	assert.Equal(legacyProgramFingerprint, profile.ProgramFingerprint)
	assert.JSONEq(policyJSON, string(profile.PolicyJSON))
}

func TestPersonInferenceConsentConcurrentGrantAndRevoke(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)

	const workers = 8
	var grants sync.WaitGroup
	var grantCreated atomic.Int64
	grantIDs := make(chan int64, workers)
	grantErrors := make(chan error, workers)
	for range workers {
		grants.Go(func() {
			consent, created, grantErr := st.GrantPersonInferenceConsent(
				t.Context(), profile.Fingerprint, "cli")
			if grantErr != nil {
				grantErrors <- grantErr
				return
			}
			if created {
				grantCreated.Add(1)
			}
			grantIDs <- consent.ID
		})
	}
	grants.Wait()
	close(grantErrors)
	close(grantIDs)
	for grantErr := range grantErrors {
		require.NoError(grantErr)
	}
	assert.Equal(int64(1), grantCreated.Load())
	var firstID int64
	for id := range grantIDs {
		if firstID == 0 {
			firstID = id
		}
		assert.Equal(firstID, id)
	}

	var revokes sync.WaitGroup
	var revokeChanged atomic.Int64
	revokeErrors := make(chan error, workers)
	for range workers {
		revokes.Go(func() {
			changed, revokeErr := st.RevokePersonInferenceConsent(
				t.Context(), profile.Fingerprint, "cli")
			if revokeErr != nil {
				revokeErrors <- revokeErr
				return
			}
			if changed {
				revokeChanged.Add(1)
			}
		})
	}
	revokes.Wait()
	close(revokeErrors)
	for revokeErr := range revokeErrors {
		require.NoError(revokeErr)
	}
	assert.Equal(int64(1), revokeChanged.Load())

	status, err := st.GetPersonInferenceConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(status.Active)
	require.NotNil(status.LastRevoked)
	assert.Equal(firstID, status.LastRevoked.ID)
}

var _ interface {
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
} = (*store.Store)(nil)
