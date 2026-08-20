package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func personProviderTestConfig() peoplesweep.Config {
	return peoplesweep.Config{
		Enabled: true,
		Provider: peoplesweep.ProviderConfig{
			Kind: peoplesweep.ProviderOpenAICompatible, Endpoint: "https://provider.example/v1",
			Model: "test-model", APIKeyEnv: "TEST_PROVIDER_KEY",
			RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
			AllowedSources: []peoplesweep.SourceClass{
				peoplesweep.SourceMeetingText, peoplesweep.SourceConversationText,
			},
			SourceSince: "2025-01-01", SourceUntil: "2025-12-31",
			RequestTimeout: time.Second,
		},
	}
}

type fixedPersonProviderChecker struct {
	response peoplesweep.StructuredResponse
	err      error
	calls    atomic.Int64
}

func (c *fixedPersonProviderChecker) Check(context.Context) (peoplesweep.StructuredResponse, error) {
	c.calls.Add(1)
	return c.response, c.err
}

func localPersonProviderDeps(
	config peoplesweep.Config,
	st personProviderStore,
	checker personProviderChecker,
) personProviderCommandDeps {
	return personProviderCommandDeps{
		config: func() peoplesweep.Config { return config },
		openStore: func() (personProviderStore, func(), error) {
			return st, func() {}, nil
		},
		newChecker: func(peoplesweep.Config, personProviderStore) (personProviderChecker, error) {
			return checker, nil
		},
		isDaemonSubprocess: func() bool { return true },
	}
}

func executePersonProviderCommand(
	t *testing.T,
	deps personProviderCommandDeps,
	args ...string,
) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonProviderCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "provider"}, args...))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err := root.ExecuteContext(t.Context())
	return output.String(), err
}

func TestPersonProviderStatusReportsExactPolicyWithoutMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := personProviderTestConfig()
	profile, err := config.Profile()
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(config, st, nil)

	human, err := executePersonProviderCommand(t, deps, "status")
	require.NoError(err)
	assert.Contains(human, profile.Fingerprint)
	assert.Contains(human, "https://provider.example/v1")
	assert.Contains(human, "test-model")
	assert.Contains(human, "zero_data_retention")
	assert.Contains(human, "conversation_text, meeting_text")
	assert.Contains(human, "2025-01-01 through 2025-12-31")
	assert.Contains(human, "Sensitive content: denied")
	assert.Contains(human, "Consent: inactive")

	jsonOutput, err := executePersonProviderCommand(t, deps, "status", "--json")
	require.NoError(err)
	var got personProviderStatusOutput
	require.NoError(json.Unmarshal([]byte(jsonOutput), &got))
	assert.Equal(profile.Fingerprint, got.Profile.Fingerprint)
	assert.False(got.Consent.Active)
	assert.False(got.Consent.ProfileExists)
}

func TestPersonProviderConsentDisclosesBeforeConfirmationAndIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := personProviderTestConfig()
	profile, err := config.Profile()
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(config, st, nil)

	disclosure, err := executePersonProviderCommand(t, deps, "consent")
	require.ErrorContains(err, "--yes")
	assert.Contains(disclosure, "People inference provider disclosure")
	assert.Contains(disclosure, "Authentication: environment variable TEST_PROVIDER_KEY")
	status, err := st.GetPersonInferenceConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(status.ProfileExists, "unconfirmed disclosure must not mutate the store")

	first, err := executePersonProviderCommand(t, deps, "consent", "--yes", "--json")
	require.NoError(err)
	var firstStatus personProviderStatusOutput
	require.NoError(json.Unmarshal([]byte(first), &firstStatus))
	require.NotNil(firstStatus.Consent.Consent)
	assert.True(firstStatus.Consent.Active)
	assert.Equal("cli", firstStatus.Consent.Consent.GrantedBy)

	second, err := executePersonProviderCommand(t, deps, "consent", "--yes", "--json")
	require.NoError(err)
	var secondStatus personProviderStatusOutput
	require.NoError(json.Unmarshal([]byte(second), &secondStatus))
	require.NotNil(secondStatus.Consent.Consent)
	assert.Equal(firstStatus.Consent.Consent.ID, secondStatus.Consent.Consent.ID)
}

func TestPersonProviderRevokeIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := personProviderTestConfig()
	profile, err := config.Profile()
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	deps := localPersonProviderDeps(config, st, nil)

	first, err := executePersonProviderCommand(t, deps, "revoke", "--json")
	require.NoError(err)
	var firstStatus personProviderStatusOutput
	require.NoError(json.Unmarshal([]byte(first), &firstStatus))
	assert.False(firstStatus.Consent.Active)
	require.NotNil(firstStatus.Consent.LastRevoked)
	assert.Equal("cli", *firstStatus.Consent.LastRevoked.RevokedBy)

	second, err := executePersonProviderCommand(t, deps, "revoke", "--json")
	require.NoError(err)
	var secondStatus personProviderStatusOutput
	require.NoError(json.Unmarshal([]byte(second), &secondStatus))
	assert.Equal(firstStatus.Consent.LastRevoked.ID, secondStatus.Consent.LastRevoked.ID)
}

func TestPersonProviderListsAndRevokesAllGrantsWhenConfigIsDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := personProviderTestConfig()
	profile, err := config.Profile()
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)

	config.Enabled = false
	deps := localPersonProviderDeps(config, st, nil)
	listed, err := executePersonProviderCommand(t, deps, "status", "--all", "--json")
	require.NoError(err)
	var listOutput personProviderStatusesOutput
	require.NoError(json.Unmarshal([]byte(listed), &listOutput))
	require.Len(listOutput.Profiles, 1)
	assert.Equal(profile.Fingerprint, listOutput.Profiles[0].Profile.Fingerprint)
	assert.True(listOutput.Profiles[0].Consent.Active)

	revoked, err := executePersonProviderCommand(t, deps, "revoke", "--all", "--json")
	require.NoError(err)
	var revokeOutput personProviderRevokeAllOutput
	require.NoError(json.Unmarshal([]byte(revoked), &revokeOutput))
	assert.Equal(int64(1), revokeOutput.Revoked)
	require.Len(revokeOutput.Profiles, 1)
	assert.False(revokeOutput.Profiles[0].Consent.Active)
	require.NotNil(revokeOutput.Profiles[0].Consent.LastRevoked)

	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(active)
}

func TestPersonProviderCheckOmitsProviderOutput(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output:            json.RawMessage(`{"ok":true,"secret":"provider-output"}`),
		ProviderRequestID: "req-safe",
		Usage:             peoplesweep.TokenUsage{InputTokens: 12, OutputTokens: 3},
	}}
	deps := localPersonProviderDeps(personProviderTestConfig(), st, checker)

	output, err := executePersonProviderCommand(t, deps, "check", "--json")
	require.NoError(t, err)
	assert.JSONEq(`{
		"ok":true,
		"provider_request_id":"req-safe",
		"model":"test-model",
		"usage":{"input_tokens":12,"output_tokens":3}
	}`, output)
	assert.NotContains(output, "provider-output")
	assert.Equal(int64(1), checker.calls.Load())
}

func TestPersonProviderCommandsRejectInputAndDisabledConfigBeforeStore(t *testing.T) {
	config := personProviderTestConfig()
	var opens atomic.Int64
	deps := localPersonProviderDeps(config, nil, nil)
	deps.openStore = func() (personProviderStore, func(), error) {
		opens.Add(1)
		return nil, func() {}, nil
	}

	for _, operation := range []string{"status", "consent", "revoke", "check"} {
		t.Run(operation+" input", func(t *testing.T) {
			_, err := executePersonProviderCommand(t, deps, operation, "archive.txt")
			assert.ErrorContains(t, err, "unknown command")
		})
	}
	assert.Zero(t, opens.Load())

	config.Enabled = false
	disabled := localPersonProviderDeps(config, nil, nil)
	disabled.openStore = deps.openStore
	_, err := executePersonProviderCommand(t, disabled, "consent", "--yes")
	require.ErrorContains(t, err, "disabled")
	assert.Zero(t, opens.Load())
}

var _ personProviderStore = (*store.Store)(nil)
