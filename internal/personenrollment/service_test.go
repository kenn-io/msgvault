package personenrollment

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func saveTestCredential(t *testing.T, credentials peoplesweep.StoredCredentials, endpoint, value string) {
	t.Helper()
	revision, _, err := credentials.Revision("remote", endpoint)
	require.NoError(t, err)
	_, err = credentials.SaveIfRevision("remote", endpoint, value, revision)
	require.NoError(t, err)
}

func TestServiceRemoveStoredProfileCredentials(t *testing.T) {
	for _, state := range []string{"never saved", "saved", "already cleared", "unreadable store"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			configured := config.NewDefaultConfig()
			configured.HomeDir = t.TempDir()
			configured.Data.DataDir = configured.HomeDir
			require.NoError(configured.Save())
			service := NewService(configured.ConfigFilePath(), testutil.NewTestStore(t))
			before, err := config.ReadConfigFile(configured.ConfigFilePath())
			require.NoError(err)
			provider, err := peoplesweep.PresetProviderConfig("openai", "example-model")
			require.NoError(err)
			provider.RetentionPosture = "operator-confirmed"
			provider.TrainingPosture = "operator-confirmed"
			provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
			provider.SourceSince = "2025-01-01"
			created, err := service.CreateProfile(before.ETag, "remote", provider)
			require.NoError(err)
			credentials := peoplesweep.NewStoredCredentials(configured.TokensDir())
			switch state {
			case "saved", "already cleared":
				saveTestCredential(t, credentials, provider.Endpoint, "synthetic-key")
				if state == "already cleared" {
					revision, _, err := credentials.Revision("remote", provider.Endpoint)
					require.NoError(err)
					_, err = credentials.DeleteIfRevision("remote", revision)
					require.NoError(err)
				}
			case "unreadable store":
				require.NoError(os.MkdirAll(configured.TokensDir(), 0o700))
				require.NoError(os.WriteFile(filepath.Join(configured.TokensDir(), "provider-credentials.json"), []byte("{"), 0o600))
			}
			_, err = service.RemoveProfile(t.Context(), created.ETag, "remote", "", "test", credentials)
			if state == "unreadable store" {
				require.Error(err)
				after, err := config.ReadConfigFile(configured.ConfigFilePath())
				require.NoError(err)
				assert.Equal(created.ETag, after.ETag)
				return
			}
			require.NoError(err)
			after, err := config.Load(configured.ConfigFilePath(), "")
			require.NoError(err)
			assert.NotContains(after.People.Sweep.Providers, "remote")
			_, present, err := credentials.Revision("remote", provider.Endpoint)
			require.NoError(err)
			assert.False(present)
		})
	}
}

func TestServiceRemoveAndRecreateRequiresFreshCheck(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	configured.Data.DataDir = configured.HomeDir
	require.NoError(configured.Save())
	st := testutil.NewTestStore(t)
	service := NewService(configured.ConfigFilePath(), st)
	before, err := config.ReadConfigFile(configured.ConfigFilePath())
	require.NoError(err)
	provider, err := peoplesweep.PresetProviderConfig("openai", "example-model")
	require.NoError(err)
	provider.RetentionPosture, provider.TrainingPosture = "operator-confirmed", "operator-confirmed"
	provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	provider.SourceSince = "2025-01-01"
	created, err := service.CreateProfile(before.ETag, "remote", provider)
	require.NoError(err)
	credentials := peoplesweep.NewStoredCredentials(configured.TokensDir())
	saveTestCredential(t, credentials, provider.Endpoint, "synthetic-old-key")
	loaded, err := config.Load(configured.ConfigFilePath(), "")
	require.NoError(err)
	selected := loaded.People.Sweep
	selected.Enabled = true
	selected.Provider.Name = "remote"
	profile, err := selected.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model,
	}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	removed, err := service.RemoveProfile(t.Context(), created.ETag, "remote", "", "test", credentials)
	require.NoError(err)
	recreated, err := service.CreateProfile(removed.ETag, "remote", provider)
	require.NoError(err)
	assert.Equal(created.Fingerprint, recreated.Fingerprint)
	saveTestCredential(t, credentials, provider.Endpoint, "synthetic-new-key")
	checked, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), recreated.Fingerprint)
	require.NoError(err)
	assert.False(checked, "recreation must not reuse the removed credential's check")
	_, err = service.SelectProfile(t.Context(), recreated.ETag, "remote")
	require.ErrorIs(err, ErrCheckRequired)
	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	require.NoError(err)
	assert.Len(profiles, 1, "immutable audit history is retained")
}

func TestServiceCreatesProfileWithoutSelectionAndRequiresCheckAndConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	require.NoError(configured.Save())
	st := testutil.NewTestStore(t)
	service := NewService(configured.ConfigFilePath(), st)
	before, err := config.ReadConfigFile(configured.ConfigFilePath())
	require.NoError(err)
	provider := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
		Model: "example-model", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "EXAMPLE_API_KEY",
		OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
		TokenLimitParameter: "max_completion_tokens",
		RetentionPosture:    "operator-confirmed", TrainingPosture: "operator-confirmed",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
	}
	created, err := service.CreateProfile(before.ETag, "remote", provider)
	require.NoError(err)
	assert.Equal("remote", created.Name)
	assert.NotEmpty(created.Fingerprint)
	after, err := config.Load(configured.ConfigFilePath(), "")
	require.NoError(err)
	assert.False(after.People.Sweep.Enabled)
	assert.NotEqual("remote", after.People.Sweep.Provider.Name)
	_, err = service.CreateProfile(before.ETag, "other", provider)
	require.ErrorIs(err, config.ErrConfigConflict)
	_, err = service.SelectProfile(context.Background(), created.ETag, "remote")
	require.ErrorIs(err, ErrCheckRequired)

	profileConfig := after.People.Sweep
	profileConfig.Enabled = true
	profileConfig.Provider = peoplesweep.ProviderSelection{Name: "remote"}
	profile, err := profileConfig.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(context.Background(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(context.Background(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode,
		ModelVersion: profile.Model,
	}))
	_, err = service.SelectProfile(context.Background(), created.ETag, "remote")
	require.ErrorIs(err, ErrConsentRequired)
	_, _, err = st.GrantPersonInferenceConsent(context.Background(), profile.Fingerprint, "test")
	require.NoError(err)
	selected, err := service.SelectProfile(context.Background(), created.ETag, "remote")
	require.NoError(err)
	assert.Equal(profile.Fingerprint, selected.Fingerprint)
	after, err = config.Load(configured.ConfigFilePath(), "")
	require.NoError(err)
	assert.True(after.People.Sweep.Enabled)
	assert.Equal("remote", after.People.Sweep.Provider.Name)
}

func TestServiceRemoveProfileRevokesAndKeepsSelectedFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	require.NoError(configured.Save())
	st := testutil.NewTestStore(t)
	service := NewService(configured.ConfigFilePath(), st)
	before, err := config.ReadConfigFile(configured.ConfigFilePath())
	require.NoError(err)
	provider := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
		Model: "example-model", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "EXAMPLE_API_KEY",
		OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
		RetentionPosture: "operator-confirmed", TrainingPosture: "operator-confirmed",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
	}
	first, err := service.CreateProfile(before.ETag, "first", provider)
	require.NoError(err)
	second, err := service.CreateProfile(first.ETag, "second", provider)
	require.NoError(err)
	_, err = service.RemoveProfile(t.Context(), second.ETag, "missing", "", "test", nil)
	require.ErrorIs(err, ErrProfileMissing)
	selected, err := config.EditConfigTables(configured.ConfigFilePath(), second.ETag,
		[]config.TableEdit{{Path: []string{"people", "sweep"}, Values: map[string]any{"provider": "second", "enabled": false}}})
	require.NoError(err)
	removed, err := service.RemoveProfile(t.Context(), selected.ETag, "second", "", "test", nil)
	require.NoError(err)
	assert.Equal("second", removed.Name)
	after, err := config.Load(configured.ConfigFilePath(), "")
	require.NoError(err)
	assert.NotContains(after.People.Sweep.Providers, "second")
	assert.Equal("default", after.People.Sweep.Provider.Name)
	removedFirst, err := service.RemoveProfile(t.Context(), removed.ETag, "first", "", "test", nil)
	require.NoError(err)
	_, err = service.RemoveProfile(t.Context(), removedFirst.ETag, "default", "", "test", nil)
	assert.ErrorIs(err, ErrOnlyProfile)
}

// TestServiceRemoveLeftoverKeyThenRecreateRequiresFreshConsent covers a
// profile deleted from the config by hand. Removing its leftover key must
// revoke the consent and check of every policy that used the key, so the same
// policy recreated with a new key cannot inherit them.
func TestServiceRemoveLeftoverKeyThenRecreateRequiresFreshConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	configured.Data.DataDir = configured.HomeDir
	require.NoError(configured.Save())
	st := testutil.NewTestStore(t)
	service := NewService(configured.ConfigFilePath(), st)
	provider := leftoverKeyTestProvider(t)
	before, err := config.ReadConfigFile(configured.ConfigFilePath())
	require.NoError(err)
	created, err := service.CreateProfile(before.ETag, "remote", provider)
	require.NoError(err)
	withSibling, err := service.CreateProfile(created.ETag, "sibling", provider)
	require.NoError(err)
	credentials := peoplesweep.NewStoredCredentials(configured.TokensDir())
	saveTestCredential(t, credentials, provider.Endpoint, "synthetic-old-key")
	remote := grantLeftoverKeyTestAuthority(t, st, configured.ConfigFilePath(), "remote")
	sibling := grantLeftoverKeyTestAuthority(t, st, configured.ConfigFilePath(), "sibling")

	edited, err := config.EditConfigTables(configured.ConfigFilePath(), withSibling.ETag, []config.TableEdit{{
		Path: []string{"people", "sweep", "providers", "remote"}, Remove: true,
	}})
	require.NoError(err)
	_, err = service.RemoveProfile(t.Context(), edited.ETag, "remote", "", "test", credentials)
	require.NoError(err)

	_, present, err := credentials.Revision("remote", provider.Endpoint)
	require.NoError(err)
	assert.False(present)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), remote)
	require.NoError(err)
	assert.False(active, "a recreated policy must not inherit consent granted for the deleted key")
	checked, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), remote)
	require.NoError(err)
	assert.False(checked)
	active, err = st.HasActivePersonInferenceConsent(t.Context(), sibling)
	require.NoError(err)
	assert.True(active, "a policy that used another key keeps its consent")

	recreated, err := service.CreateProfile(edited.ETag, "remote", provider)
	require.NoError(err)
	assert.Equal(remote, recreated.Fingerprint)
	_, err = service.SelectProfile(t.Context(), recreated.ETag, "remote")
	require.ErrorIs(err, ErrCheckRequired)
}

func TestRemoveUnconfiguredKeyRacesWithAdd(t *testing.T) {
	tests := []struct {
		name string
		// concurrentAdd runs at the given config check: 1 before the delete,
		// 2 after it.
		concurrentAdd   func(t *testing.T, credentials peoplesweep.StoredCredentials, check int) bool
		wantErr         error
		wantErrContains string
		wantKey         string
	}{
		{
			name: "profile already added keeps its key",
			concurrentAdd: func(*testing.T, peoplesweep.StoredCredentials, int) bool {
				return true
			},
			wantErr: ErrProfileExists,
			wantKey: "synthetic-old-key",
		},
		{
			name: "key saved after it was observed is kept",
			concurrentAdd: func(t *testing.T, credentials peoplesweep.StoredCredentials, check int) bool {
				t.Helper()
				if check == 1 {
					revision, _, err := credentials.Revision("remote", leftoverKeyTestEndpoint)
					require.NoError(t, err)
					_, err = credentials.SaveIfRevision("remote", leftoverKeyTestEndpoint, "synthetic-new-key", revision)
					require.NoError(t, err)
				}
				return false
			},
			wantErr: peoplesweep.ErrCredentialRevisionConflict,
			wantKey: "synthetic-new-key",
		},
		{
			name: "profile added after the delete is reported",
			concurrentAdd: func(_ *testing.T, _ peoplesweep.StoredCredentials, check int) bool {
				return check == 2
			},
			wantErrContains: "store its key again",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			credentials := peoplesweep.NewStoredCredentials(t.TempDir())
			saveTestCredential(t, credentials, leftoverKeyTestEndpoint, "synthetic-old-key")
			checks := 0
			err := RemoveUnconfiguredKey(t.Context(), UnconfiguredKeyRemoval{
				Name: "remote", Actor: "test", Store: testutil.NewTestStore(t), Credentials: credentials,
				Unconfigured: func() (bool, error) {
					checks++
					return !test.concurrentAdd(t, credentials, checks), nil
				},
			})
			if test.wantErr != nil {
				require.ErrorIs(err, test.wantErr)
			} else {
				require.ErrorContains(err, test.wantErrContains)
			}
			if test.wantKey == "" {
				_, present, err := credentials.UnconfiguredRevision("remote")
				require.NoError(err)
				require.False(present)
				return
			}
			value, err := credentials.Load("remote", leftoverKeyTestEndpoint)
			require.NoError(err)
			require.Equal(test.wantKey, value)
		})
	}
}

// leftoverKeyTestEndpoint is the endpoint of the openai preset.
const leftoverKeyTestEndpoint = "https://api.openai.com/v1"

func leftoverKeyTestProvider(t *testing.T) peoplesweep.ProviderConfig {
	t.Helper()
	provider, err := peoplesweep.PresetProviderConfig("openai", "example-model")
	require.NoError(t, err)
	provider.RetentionPosture, provider.TrainingPosture = "operator-confirmed", "operator-confirmed"
	provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	provider.SourceSince = "2025-01-01"
	return provider
}

func grantLeftoverKeyTestAuthority(t *testing.T, st *store.Store, configPath, name string) string {
	t.Helper()
	require := require.New(t)
	loaded, err := config.Load(configPath, "")
	require.NoError(err)
	selected := loaded.People.Sweep
	selected.Enabled = true
	selected.Provider.Name = name
	profile, err := selected.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model,
	}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	return profile.Fingerprint
}
