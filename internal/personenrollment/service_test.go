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

func TestServiceRemoveStoredProfileCredentials(t *testing.T) {
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored credentials require Unix permissions")
	}
	for _, state := range []string{"never saved", "already cleared", "missing lock marker"} {
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
			credentials := peoplesweep.NewFileCredentialStore(configured.TokensDir())
			if state != "never saved" {
				require.NoError(os.MkdirAll(configured.TokensDir(), 0o700))
				require.NoError(credentials.Save("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "synthetic-key")))
				if state == "already cleared" {
					guard, err := credentials.PreflightDelete("remote")
					require.NoError(err)
					require.NoError(credentials.Delete("remote", guard))
					require.NoError(guard.Close())
				} else {
					require.NoError(os.Remove(filepath.Join(configured.TokensDir(), "people-providers", ".credentials.lock")))
				}
			}
			_, err = service.RemoveProfile(t.Context(), created.ETag, "remote", "", "test", credentials)
			if state == "missing lock marker" {
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
		})
	}
}

func TestServiceRemoveAndRecreateRequiresFreshCheck(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored credentials require Unix permissions")
	}
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
	credentials := peoplesweep.NewFileCredentialStore(configured.TokensDir())
	require.NoError(os.MkdirAll(configured.TokensDir(), 0o700))
	require.NoError(credentials.Save("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "synthetic-old-key")))
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
	require.NoError(credentials.Save("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "synthetic-new-key")))
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
