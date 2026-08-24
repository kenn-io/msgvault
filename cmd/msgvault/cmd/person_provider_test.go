package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func personProviderTestConfig() peoplesweep.Config {
	config := peoplesweep.Config{
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
	config.ApplyDefaults()
	return config
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

func semanticPersonProviderTestConfig() vector.Config {
	return vector.Config{
		Enabled: true,
		Backend: "sqlite-vec",
		Embeddings: vector.EmbeddingsConfig{
			Endpoint: "https://embedding.example.test/v1", APIFormat: vector.APIFormatOpenAI,
			Model: "semantic-person-model", APIKeyEnv: "SEMANTIC_PERSON_KEY",
			Dimension: 4, BatchSize: 8,
		},
		People: vector.PeopleConfig{
			Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		},
	}
}

func historicalSemanticPersonProviderProfile(t *testing.T) vector.SemanticPersonEmbeddingProfile {
	t.Helper()
	profile := vector.SemanticPersonEmbeddingProfile{
		Fingerprint:      "f002512c98b050443ebd3c113fc18199c48ed6ef2d27d70b62328c6bbac3d250",
		Purpose:          "semantic_person_embeddings",
		Destination:      "https://embedding.example.test/v1/embeddings",
		APIFormat:        vector.APIFormatOpenAI,
		Model:            "semantic-person-model",
		APIKeyEnv:        "SEMANTIC_PERSON_KEY",
		RetentionPosture: "zero_data_retention",
		TrainingPosture:  "no_training",
		RendererPolicy:   "person-semantic-v1",
		DisclosedFieldClasses: []string{
			"active_relationship_counterpart_labels_and_display_names",
			"current_employment_title_role_department_location_description",
			"current_organization_alternate_names_categories_description_domain_kind",
			"current_organization_coarse_locations",
			"current_organization_name",
			"person_alternate_names",
			"person_categories",
			"person_coarse_locations",
			"person_display_name",
			"person_searchable_non_sensitive_custom_attributes_excluding_email_phone_date_timestamp",
		},
		CorpusScope: "all_durable_people",
		PolicyJSON:  json.RawMessage(`{"purpose":"semantic_person_embeddings","destination":"https://embedding.example.test/v1/embeddings","api_format":"openai","model":"semantic-person-model","api_key_env":"SEMANTIC_PERSON_KEY","retention_posture":"zero_data_retention","training_posture":"no_training","renderer_policy":"person-semantic-v1","disclosed_field_classes":["active_relationship_counterpart_labels_and_display_names","current_employment_title_role_department_location_description","current_organization_alternate_names_categories_description_domain_kind","current_organization_coarse_locations","current_organization_name","person_alternate_names","person_categories","person_coarse_locations","person_display_name","person_searchable_non_sensitive_custom_attributes_excluding_email_phone_date_timestamp"],"corpus_scope":"all_durable_people"}`),
	}
	require.NoError(t, profile.Validate())
	return profile
}

func executePersonProviderCommand(
	t *testing.T,
	deps personProviderCommandDeps,
	args ...string,
) (string, error) {
	t.Helper()
	return executePersonProviderCommandContext(t.Context(), t, deps, args...)
}

func executePersonProviderCommandContext(
	ctx context.Context,
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
	err := root.ExecuteContext(ctx)
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
	assert.Contains(human, "Packet renderer: person-sweep-packet-v1")
	assert.Contains(human, "Extraction program fingerprint: "+peoplesweep.ProgramFingerprint())
	assert.Contains(human, "Disclosed packet field classes:")
	for _, field := range []string{
		"person_id", "program_identity", "catalog", "current_projection",
		"unresolved_claims", "seed_evidence", "retrieved_context",
	} {
		assert.Contains(human, "- "+field)
	}
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

// TestPersonProviderSemanticSelectorDisclosesGlobalCorpusAndPersistsOnlyExactPolicy
// catches the CLI hiding curated egress scope or granting people-sweep consent
// instead of the selected semantic-person policy.
func TestPersonProviderSemanticSelectorDisclosesGlobalCorpusAndPersistsOnlyExactPolicy(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(personProviderTestConfig(), st, nil)
	semanticConfig := semanticPersonProviderTestConfig()
	deps.vectorConfig = func() vector.Config { return semanticConfig }
	t.Setenv("SEMANTIC_PERSON_KEY", "credential-value-must-not-be-disclosed")

	disclosure, err := executePersonProviderCommand(
		t, deps, "consent", "--semantic-embeddings",
	)
	must.ErrorContains(err, "--yes")
	profile, profileErr := semanticConfig.SemanticPersonEmbeddingProfile()
	must.NoError(profileErr)
	check.Contains(disclosure, "Semantic person embedding provider disclosure")
	check.Contains(disclosure, "Purpose: semantic_person_embeddings")
	check.Contains(disclosure, "Fingerprint: "+profile.Fingerprint)
	check.Contains(disclosure, "Destination: https://embedding.example.test/v1/embeddings")
	check.Contains(disclosure, "API format: openai")
	check.Contains(disclosure, "Model: semantic-person-model")
	check.Contains(disclosure, "Authentication: environment variable SEMANTIC_PERSON_KEY")
	check.Contains(disclosure, "Provider assertions: retention=zero_data_retention, training=no_training")
	check.Contains(disclosure, "Renderer policy: person-semantic-v1")
	check.Contains(disclosure, "Disclosed curated document field classes:")
	check.Contains(disclosure,
		"Caller-supplied query egress: free-text semantic person search queries are sent to the embedding provider.")
	check.Contains(disclosure, "Corpus scope: all_durable_people")
	check.Contains(disclosure, "[vector.embed.scope] does not filter curated people")
	const queryDisclosureToken = "caller_supplied_free_text_query_for_semantic_person_search"
	for _, field := range vector.SemanticPersonDisclosedFieldClasses() {
		if field == queryDisclosureToken {
			continue
		}
		check.Contains(disclosure, "- "+field)
	}
	check.NotContains(disclosure, "- "+queryDisclosureToken)
	check.NotContains(disclosure, "credential-value-must-not-be-disclosed")
	statusDisclosure, err := executePersonProviderCommand(
		t, deps, "status", "--semantic-embeddings",
	)
	must.NoError(err)
	check.Contains(statusDisclosure, "Corpus scope: all_durable_people")
	check.Contains(statusDisclosure, "Consent: inactive")

	status, err := st.GetPersonSemanticEmbeddingConsentStatus(t.Context(), profile.Fingerprint)
	must.NoError(err)
	check.False(status.ProfileExists, "disclosure without --yes must not persist")

	output, err := executePersonProviderCommand(
		t, deps, "consent", "--semantic-embeddings", "--yes", "--json",
	)
	must.NoError(err)
	var got personSemanticProviderStatusOutput
	must.NoError(json.Unmarshal([]byte(output), &got))
	check.Equal(profile.Fingerprint, got.Profile.Fingerprint)
	check.True(got.Consent.Active)
	inferenceProfiles, err := st.ListPersonInferenceProfiles(t.Context())
	must.NoError(err)
	check.Empty(inferenceProfiles, "semantic selector must not create a people-sweep grant")

	_, err = executePersonProviderCommand(
		t, deps, "revoke", "--semantic-embeddings", "--json",
	)
	must.NoError(err)
	active, err := st.HasActivePersonSemanticEmbeddingConsent(t.Context(), profile.Fingerprint)
	must.NoError(err)
	check.False(active)
}

// TestSemanticPersonProviderStatusAllPreservesHistoricalQueryDisclosure
// catches status --all claiming that a stored pre-expansion profile disclosed
// caller search queries when its immutable policy did not.
func TestSemanticPersonProviderStatusAllPreservesHistoricalQueryDisclosure(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	historical := historicalSemanticPersonProviderProfile(t)
	_, err := st.EnsurePersonSemanticEmbeddingProfile(t.Context(), historical)
	must.NoError(err)
	deps := localPersonProviderDeps(personProviderTestConfig(), st, nil)

	output, err := executePersonProviderCommand(
		t, deps, "status", "--semantic-embeddings", "--all",
	)
	must.NoError(err)
	check.Contains(output, "Fingerprint: "+historical.Fingerprint)
	check.Contains(output, "Disclosed curated document field classes:")
	check.NotContains(output, "Caller-supplied query egress:")
	check.NotContains(output, "caller_supplied_free_text_query_for_semantic_person_search")

	profiles, err := st.ListPersonSemanticEmbeddingProfiles(t.Context())
	must.NoError(err)
	must.Len(profiles, 1)
	check.Equal(historical.Fingerprint, profiles[0].Fingerprint)
	check.Equal(historical.DisclosedFieldClasses, profiles[0].DisclosedFieldClasses)
	check.JSONEq(string(historical.PolicyJSON), string(profiles[0].PolicyJSON))
}

func TestSemanticPersonProviderStatusAndRevokeResolveDisabledCurrentPolicy(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	semanticConfig := semanticPersonProviderTestConfig()
	profile, err := semanticConfig.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
	must.NoError(err)
	_, _, err = st.GrantPersonSemanticEmbeddingConsent(
		t.Context(), profile.Fingerprint, "cli",
	)
	must.NoError(err)
	semanticConfig.Enabled = false
	semanticConfig.People.Enabled = false
	deps := localPersonProviderDeps(personProviderTestConfig(), st, nil)
	deps.vectorConfig = func() vector.Config { return semanticConfig }

	statusOutput, err := executePersonProviderCommand(
		t, deps, "status", "--semantic-embeddings", "--json",
	)
	must.NoError(err)
	var status personSemanticProviderStatusOutput
	must.NoError(json.Unmarshal([]byte(statusOutput), &status))
	checks.Equal(profile.Fingerprint, status.Profile.Fingerprint)
	checks.True(status.Consent.Active)

	revokeOutput, err := executePersonProviderCommand(
		t, deps, "revoke", "--semantic-embeddings", "--json",
	)
	must.NoError(err)
	var revoked personSemanticProviderStatusOutput
	must.NoError(json.Unmarshal([]byte(revokeOutput), &revoked))
	checks.Equal(profile.Fingerprint, revoked.Profile.Fingerprint)
	checks.False(revoked.Consent.Active)

	_, err = executePersonProviderCommand(
		t, deps, "consent", "--semantic-embeddings", "--yes",
	)
	must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingsDisabled,
		"only consent requires semantic person embeddings to be enabled")
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

type unreleasedOperationStarter struct {
	starts atomic.Int64
}

func (s *unreleasedOperationStarter) Start(
	context.Context, peoplesweep.CodexExecutable, []string, []string, string,
) (peoplesweep.RPCProcess, error) {
	s.starts.Add(1)
	return nil, errors.New("unexpected Codex process start")
}

func unreleasedCodexCommandConfig(t *testing.T) peoplesweep.Config {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	config := commandCodexConfig()
	config.Provider.Executable = executable
	return config
}

func unreleasedCodexCommandDeps(
	t *testing.T,
	config peoplesweep.Config,
	st personProviderStore,
	starter *unreleasedOperationStarter,
) personProviderCommandDeps {
	t.Helper()
	deps := localPersonProviderDeps(config, st, nil)
	deps.newChecker = func(config peoplesweep.Config, st personProviderStore) (personProviderChecker, error) {
		transport, err := peoplesweep.NewStructuredTransport(
			config.Provider, nil, starter, peoplesweep.NewReleasedCodexIsolationGate(),
		)
		if err != nil {
			return nil, err
		}
		return peoplesweep.NewRunner(config, st, transport, os.LookupEnv)
	}
	deps.newCodexClient = func(config peoplesweep.Config) (personProviderCodexClient, error) {
		transport, err := peoplesweep.NewStructuredTransport(
			config.Provider, nil, starter, peoplesweep.NewReleasedCodexIsolationGate(),
		)
		if err != nil {
			return nil, err
		}
		codex, ok := transport.(*peoplesweep.CodexAppServerTransport)
		if !ok {
			return nil, errors.New("expected Codex transport")
		}
		return codex, nil
	}
	return deps
}

// TestCodexUnreleasedOperationsLaunchNothing catches any process-capable CLI
// or transport path bypassing the empty production release registry. Consent
// and revoke remain durable host-only operations.
func TestCodexUnreleasedOperationsLaunchNothing(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	config := unreleasedCodexCommandConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	starter := &unreleasedOperationStarter{}
	deps := unreleasedCodexCommandDeps(t, config, st, starter)
	transport, err := peoplesweep.NewCodexAppServerTransport(
		config.Provider, starter, peoplesweep.NewReleasedCodexIsolationGate(),
	)
	must.NoError(err)
	profile, err := config.Profile()
	must.NoError(err)
	request := peoplesweep.StructuredRequest{
		ProgramID: "unreleased-test", ProgramVersion: "1",
		Sources:   []peoplesweep.SourceDescriptor{{Class: peoplesweep.SourceConversationText, ObservedOn: "2026-08-23"}},
		InputText: `{"synthetic":"packet"}`, SchemaName: "empty",
		JSONSchema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		MaxOutputTokens: 16,
	}
	prepared, err := transport.PrepareJSON(profile, request)
	must.NoError(err)

	operations := []struct {
		name        string
		processable bool
		run         func() error
	}{
		{name: "generation", processable: true, run: func() error {
			_, callErr := transport.GeneratePreparedJSON(t.Context(), profile, "", prepared)
			return callErr
		}},
		{name: "login", processable: true, run: func() error {
			_, callErr := executePersonProviderCommand(t, deps, "login")
			return callErr
		}},
		{name: "models", processable: true, run: func() error {
			_, callErr := executePersonProviderCommand(t, deps, "models")
			return callErr
		}},
		{name: "status", run: func() error {
			_, callErr := executePersonProviderCommand(t, deps, "status")
			return callErr
		}},
		{name: "check", processable: true, run: func() error {
			_, callErr := executePersonProviderCommand(t, deps, "check")
			return callErr
		}},
		{name: "consent", run: func() error {
			_, callErr := executePersonProviderCommand(t, deps, "consent", "--yes")
			return callErr
		}},
		{name: "revoke", run: func() error {
			_, callErr := executePersonProviderCommand(t, deps, "revoke")
			return callErr
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if operation.processable {
				require.ErrorIs(t, err, peoplesweep.ErrCodexIsolationUnreleased)
				return
			}
			require.NoError(t, err)
		})
	}
	checks.Zero(starter.starts.Load())
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	must.NoError(err)
	checks.False(active, "revoke must remain a durable operation after consent")
}

// TestPersonProviderStatusCodexUnreleased catches status leaking operational
// paths or environment values while reporting an unavailable boundary.
func TestPersonProviderStatusCodexUnreleased(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	config := unreleasedCodexCommandConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	starter := &unreleasedOperationStarter{}
	deps := unreleasedCodexCommandDeps(t, config, st, starter)
	authStore := filepath.Join(t.TempDir(), "auth-sensitive-path")
	t.Setenv("CODEX_HOME", authStore)
	t.Setenv("MSGVAULT_STATUS_SECRET", "environment-sensitive-value")

	output, err := executePersonProviderCommand(t, deps, "status")
	must.NoError(err)
	checks.Contains(output, "Codex isolation: unavailable")
	checks.Contains(output, "Execution boundary: "+peoplesweep.CodexExecutionBoundaryV1)
	checks.Contains(output, "Reason: "+peoplesweep.ErrCodexIsolationUnreleased.Error())
	checks.NotContains(output, config.Provider.Executable)
	checks.NotContains(output, authStore)
	checks.NotContains(output, "environment-sensitive-value")
	checks.Zero(starter.starts.Load())
}

type commandCodexProcess struct {
	stdin     *io.PipeWriter
	stdout    *io.PipeReader
	stderr    *io.PipeReader
	serverIn  *io.PipeReader
	serverOut *io.PipeWriter
	serverErr *io.PipeWriter
	done      chan struct{}
	once      sync.Once
}

func newCommandCodexProcess(
	serve func(*bufio.Reader, io.Writer) error,
) *commandCodexProcess {
	serverIn, stdin := io.Pipe()
	stdout, serverOut := io.Pipe()
	stderr, serverErr := io.Pipe()
	process := &commandCodexProcess{
		stdin: stdin, stdout: stdout, stderr: stderr,
		serverIn: serverIn, serverOut: serverOut, serverErr: serverErr,
		done: make(chan struct{}),
	}
	go func() {
		_ = serve(bufio.NewReader(serverIn), serverOut)
		_ = serverOut.Close()
		_ = serverErr.Close()
		_ = serverIn.Close()
		close(process.done)
	}()
	return process
}

func (p *commandCodexProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *commandCodexProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *commandCodexProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *commandCodexProcess) Wait() error           { <-p.done; return nil }
func (p *commandCodexProcess) Kill() error {
	p.once.Do(func() {
		_ = p.serverIn.CloseWithError(context.Canceled)
		_ = p.serverOut.CloseWithError(context.Canceled)
		_ = p.serverErr.CloseWithError(context.Canceled)
	})
	return nil
}

type commandCodexStarter struct {
	t       *testing.T
	mu      sync.Mutex
	scripts []func(*bufio.Reader, io.Writer) error
	starts  atomic.Int64
}

func commandCodexTestAbsolutePath() string {
	if runtime.GOOS == "windows" {
		return `C:\attested\codex.exe`
	}
	return "/attested/codex"
}

func (s *commandCodexStarter) Start(
	_ context.Context, executable peoplesweep.CodexExecutable, _ []string, env []string, _ string,
) (peoplesweep.RPCProcess, error) {
	s.starts.Add(1)
	assert.Equal(s.t, commandCodexTestAbsolutePath(), executable.Path())
	assert.NotContains(s.t, env, "ARCHIVE_CREDENTIAL=must-not-forward")
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.scripts) == 0 {
		return nil, errors.New("unexpected Codex process")
	}
	script := s.scripts[0]
	s.scripts = s.scripts[1:]
	return newCommandCodexProcess(func(reader *bufio.Reader, writer io.Writer) error {
		return script(reader, writer)
	}), nil
}

type commandCodexGate struct{}

func (commandCodexGate) Verify(_ context.Context, _, boundary string) (peoplesweep.CodexAttestation, error) {
	return peoplesweep.CodexAttestation{
		ExecutablePath: commandCodexTestAbsolutePath(), Version: "codex-cli 0.149.0",
		ExecutableSHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ExecutionBoundary: boundary,
		LaunchArtifact:    peoplesweep.CodexLaunchArtifactNativeStandaloneV1,
	}, nil
}

func commandCodexAttestedPath() string {
	path, err := os.Executable()
	if err != nil {
		return filepath.Join(string(filepath.Separator), "attested", "codex")
	}
	return path
}

func (commandCodexGate) ReverifyForLaunch(peoplesweep.CodexAttestation) error { return nil }

func commandCodexConfig() peoplesweep.Config {
	config := personProviderTestConfig()
	config.Provider = peoplesweep.ProviderConfig{
		Kind: peoplesweep.ProviderCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", Executable: "codex",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1, RequestTimeout: time.Second,
	}
	config.ApplyDefaults()
	return config
}

func commandCodexScript(
	t *testing.T,
	methods *[]string,
	operation string,
	result map[string]any,
) func(*bufio.Reader, io.Writer) error {
	t.Helper()
	return func(reader *bufio.Reader, writer io.Writer) error {
		for id, want := range []string{"initialize", operation} {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read command Codex request: %w", err)
			}
			var request struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(line, &request); err != nil {
				return err
			}
			*methods = append(*methods, request.Method)
			if request.Method != want || request.ID != int64(id+1) {
				return errors.New("unexpected Codex command transcript")
			}
			if operation == "account/login/start" && id == 1 {
				assert.Equal(t, "chatgptDeviceCode", request.Params["type"])
			}
			response := map[string]any{"id": request.ID, "result": map[string]any{}}
			if id == 1 {
				response["result"] = result
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				return err
			}
			if _, err := writer.Write(append(encoded, '\n')); err != nil {
				return err
			}
			if operation == "account/login/start" && id == 1 {
				completed, err := json.Marshal(map[string]any{
					"method": "account/login/completed",
					"params": map[string]any{"success": true, "loginId": result["loginId"]},
				})
				if err != nil {
					return err
				}
				if _, err := writer.Write(append(completed, '\n')); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func codexCommandDeps(
	t *testing.T,
	starter *commandCodexStarter,
	opens *atomic.Int64,
) personProviderCommandDeps {
	t.Helper()
	config := commandCodexConfig()
	deps := localPersonProviderDeps(config, nil, nil)
	deps.openStore = func() (personProviderStore, func(), error) {
		opens.Add(1)
		return nil, func() {}, errors.New("archive store must not be opened")
	}
	deps.newCodexClient = func(config peoplesweep.Config) (personProviderCodexClient, error) {
		return peoplesweep.NewCodexAppServerTransport(config.Provider, starter, commandCodexGate{})
	}
	return deps
}

func TestPersonProviderLoginUsesDeviceCode(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var methods []string
	starter := &commandCodexStarter{t: t}
	starter.scripts = []func(*bufio.Reader, io.Writer) error{commandCodexScript(t, &methods, "account/login/start", map[string]any{
		"type": "chatgptDeviceCode", "loginId": "login-safe",
		"verificationUrl": "https://auth.example.test/device", "userCode": "ABCD-1234",
		"expiresAt": "2026-08-23T12:30:00Z",
	})}
	var opens atomic.Int64
	t.Setenv("ARCHIVE_CREDENTIAL", "must-not-forward")
	deps := codexCommandDeps(t, starter, &opens)

	output, err := executePersonProviderCommand(t, deps, "login")
	must.NoError(err)
	checks.Contains(output, "https://auth.example.test/device")
	checks.Contains(output, "ABCD-1234")
	checks.Contains(output, "2026-08-23T12:30:00Z")
	checks.NotContains(output, "login-safe")
	checks.Equal([]string{"initialize", "account/login/start"}, methods)
	checks.Zero(opens.Load())
}

func TestPersonProviderModelsListsSupportedEfforts(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var methods []string
	starter := &commandCodexStarter{t: t}
	starter.scripts = []func(*bufio.Reader, io.Writer) error{commandCodexScript(t, &methods, "model/list", map[string]any{
		"data": []any{map[string]any{
			"id": "gpt-test", "model": "gpt-test", "displayName": "Test Model",
			"defaultReasoningEffort": "medium",
			"supportedReasoningEfforts": []any{
				map[string]any{"reasoningEffort": "low", "description": "Fast"},
				map[string]any{"reasoningEffort": "medium", "description": "Balanced"},
			},
		}}, "nextCursor": nil,
	})}
	var opens atomic.Int64
	t.Setenv("ARCHIVE_CREDENTIAL", "must-not-forward")
	deps := codexCommandDeps(t, starter, &opens)

	output, err := executePersonProviderCommand(t, deps, "models")
	must.NoError(err)
	checks.Contains(output, "gpt-test")
	checks.Contains(output, "Test Model")
	checks.Contains(output, "medium")
	checks.Contains(output, "low, medium")
	checks.Equal([]string{"initialize", "model/list"}, methods)
	checks.Zero(opens.Load())
}

func TestPersonProviderLoginAndModelsUseConfiguredTimeout(t *testing.T) {
	for _, operation := range []string{"login", "models"} {
		t.Run(operation, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			starter := &commandCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer) error{
				func(reader *bufio.Reader, _ io.Writer) error {
					if _, err := reader.ReadBytes('\n'); err != nil {
						return fmt.Errorf("read silent command Codex request: %w", err)
					}
					_, err := reader.ReadBytes('\n')
					if err != nil {
						return fmt.Errorf("wait for silent command Codex request: %w", err)
					}
					return nil
				},
			}}
			var opens atomic.Int64
			deps := codexCommandDeps(t, starter, &opens)
			config := commandCodexConfig()
			config.Provider.RequestTimeout = 30 * time.Millisecond
			deps.config = func() peoplesweep.Config { return config }
			parentCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			startedAt := time.Now()
			_, err := executePersonProviderCommandContext(parentCtx, t, deps, operation)
			must.ErrorIs(err, context.DeadlineExceeded)
			checks.Less(time.Since(startedAt), 250*time.Millisecond)
			checks.Equal(int64(1), starter.starts.Load())
			checks.Zero(opens.Load())
		})
	}
}

var _ personProviderStore = (*store.Store)(nil)
