package personenrollment

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

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
	_, err = service.RemoveProfile(t.Context(), second.ETag, "missing", "test", nil)
	require.ErrorIs(err, ErrProfileMissing)
	selected, err := config.EditConfigTables(configured.ConfigFilePath(), second.ETag,
		[]config.TableEdit{{Path: []string{"people", "sweep"}, Values: map[string]any{"provider": "second", "enabled": false}}})
	require.NoError(err)
	removed, err := service.RemoveProfile(t.Context(), selected.ETag, "second", "test", nil)
	require.NoError(err)
	assert.Equal("second", removed.Name)
	after, err := config.Load(configured.ConfigFilePath(), "")
	require.NoError(err)
	assert.NotContains(after.People.Sweep.Providers, "second")
	assert.Equal("default", after.People.Sweep.Provider.Name)
	removedFirst, err := service.RemoveProfile(t.Context(), removed.ETag, "first", "test", nil)
	require.NoError(err)
	_, err = service.RemoveProfile(t.Context(), removedFirst.ETag, "default", "test", nil)
	assert.ErrorIs(err, ErrOnlyProfile)
}
