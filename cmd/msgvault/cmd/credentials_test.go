package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func runCredentialsTestCommand(t *testing.T, cfg *config.Config, input string, args ...string) (string, error) {
	t.Helper()
	command := newCredentialsCommand()
	command.SetContext(withStoreResolverConfig(t, cfg))
	command.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		return output.String(), fmt.Errorf("execute credentials test command: %w", err)
	}
	return output.String(), nil
}

type credentialsTestErrorWriter struct{ err error }

func (w credentialsTestErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func runCredentialsTestCommandWithOutput(t *testing.T, cfg *config.Config, input string, output io.Writer, args ...string) error {
	t.Helper()
	command := newCredentialsCommand()
	command.SetContext(withStoreResolverConfig(t, cfg))
	command.SetIn(strings.NewReader(input))
	command.SetOut(output)
	command.SetErr(io.Discard)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		return fmt.Errorf("execute credentials test command with output: %w", err)
	}
	return nil
}

func TestCredentialsCommandsWrapOutputErrors(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		args        []string
		prepare     func(*testing.T, *config.Config)
		wantContext string
	}{
		{
			name:        "set status",
			input:       "provider-key",
			args:        []string{"set", providercredentials.VectorEmbeddingsID, "--stdin"},
			wantContext: "write saved credential status",
		},
		{
			name: "list text",
			args: []string{"list"},
			prepare: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				snapshot, err := providercredentials.Read(cfg.TokensDir())
				require.NoError(t, err)
				_, err = providercredentials.Put(cfg.TokensDir(), snapshot.ETag, providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, "provider-key")
				require.NoError(t, err)
			},
			wantContext: "write credential listing",
		},
		{
			name: "list JSON",
			args: []string{"list", "--json"},
			prepare: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				snapshot, err := providercredentials.Read(cfg.TokensDir())
				require.NoError(t, err)
				_, err = providercredentials.Put(cfg.TokensDir(), snapshot.ETag, providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, "provider-key")
				require.NoError(t, err)
			},
			wantContext: "write credential metadata JSON",
		},
		{
			name: "import summary",
			args: []string{"import-env"},
			prepare: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				cfg.Vector.Embeddings.APIKeyEnv = "MSGVAULT_TEST_CREDENTIAL_OUTPUT_KEY"
				t.Setenv(cfg.Vector.Embeddings.APIKeyEnv, "provider-key")
			},
			wantContext: "write credential import summary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := credentialsTestConfig(t)
			if tc.prepare != nil {
				tc.prepare(t, cfg)
			}
			writeErr := errors.New("output is closed")
			err := runCredentialsTestCommandWithOutput(t, cfg, tc.input, credentialsTestErrorWriter{err: writeErr}, tc.args...)
			require.ErrorIs(t, err, writeErr)
			assert.Contains(t, err.Error(), tc.wantContext)
		})
	}
}

func TestCredentialsNamedProviderAndMalformedStore(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	cfg := credentialsTestConfig(t)
	cfg.People.Enrichment.Providers = []personenrichment.ProviderConfig{{Name: "research", Kind: personenrichment.ProviderExa, Endpoint: "https://research.example.test/search"}}
	_, err := runCredentialsTestCommand(t, cfg, "named-provider-key", "set", providercredentials.PersonEnrichmentID("research"), "--stdin")
	require.NoError(err)
	key, configured, err := personEnrichmentProviderCredentialLookup(cfg)(personenrichment.ProviderProfile{Name: "research", Kind: personenrichment.ProviderExa, Endpoint: "https://research.example.test/search"})
	require.NoError(err)
	assert.True(configured)
	assert.Equal("named-provider-key", key)
	output, err := runCredentialsTestCommand(t, cfg, "", "list", "--json")
	require.NoError(err)
	assert.JSONEq(`[{"id":"people.enrichment/research","origin":"https://research.example.test"}]`, output)
	assert.NotContains(output, "named-provider-key")
	require.NoError(fileutil.SecureWriteFile(filepath.Join(cfg.TokensDir(), providercredentials.Filename), []byte("invalid store"), 0o600))
	_, err = runCredentialsTestCommand(t, cfg, "replacement", "set", providercredentials.VectorEmbeddingsID, "--stdin")
	require.Error(err)
	contents, err := os.ReadFile(filepath.Join(cfg.TokensDir(), providercredentials.Filename))
	require.NoError(err)
	assert.Equal("invalid store", string(contents))
}

func credentialsTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Vector.Embeddings.Endpoint = "https://embed.example.test/v1"
	cfg.Vector.Embeddings.APIKeyEnv = "MSGVAULT_TEST_UNSET_EMBEDDINGS_KEY"
	cfg.Vector.Multimodal.APIKeyEnv = "MSGVAULT_TEST_UNSET_MULTIMODAL_KEY"
	return cfg
}

func TestCredentialsSetListAndRuntimePrecedence(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	cfg := credentialsTestConfig(t)
	file := filepath.Join(t.TempDir(), "key")
	require.NoError(fileutil.SecureWriteFile(file, []byte("stored-provider-key\n"), 0o600))
	output, err := runCredentialsTestCommand(t, cfg, "", "set", providercredentials.VectorEmbeddingsID, "--from-file", file)
	require.NoError(err)
	assert.NotContains(output, "stored-provider-key")
	output, err = runCredentialsTestCommand(t, cfg, "", "list")
	require.NoError(err)
	assert.Contains(output, providercredentials.VectorEmbeddingsID)
	assert.Contains(output, "https://embed.example.test")
	assert.NotContains(output, "stored-provider-key")
	snapshot, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	key, state, err := snapshot.Resolve(providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, "LOWER_PRIORITY_ENV", func(string) (string, bool) { return "lower-priority-key", true })
	require.NoError(err)
	assert.Equal("stored-provider-key", key)
	assert.Equal(providercredentials.SourceStored, state.Source)
	_, _, err = snapshot.Resolve(providercredentials.VectorEmbeddingsID, "https://other.example.test/v1", "LOWER_PRIORITY_ENV", func(string) (string, bool) { return "lower-priority-key", true })
	require.ErrorIs(err, providercredentials.ErrOriginMismatch)
	_, err = os.Stat(cfg.ConfigFilePath())
	assert.ErrorIs(err, os.ErrNotExist)
}

func TestCredentialsStdinSuppressionUsesStoredRuntimeValue(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	cfg := credentialsTestConfig(t)
	cfg.People.Enrichment.SuppressionKeyEnv = "MSGVAULT_TEST_SUPPRESSION_KEY"
	t.Setenv(cfg.People.Enrichment.SuppressionKeyEnv, "old-environment-value")
	output, err := runCredentialsTestCommand(t, cfg, "stored-suppression-key-32bytes-123\n", "set", providercredentials.PersonEnrichmentSuppressionID, "--stdin")
	require.NoError(err)
	assert.NotContains(output, "stored-suppression-key")
	key, ok := personEnrichmentEnvironmentLookup(cfg)(cfg.People.Enrichment.SuppressionKeyEnv)
	require.True(ok)
	assert.Equal("stored-suppression-key-32bytes-123", key)
}

func TestCredentialsValidateSuppressionBeforeSaving(t *testing.T) { //nolint:paralleltest // process environment
	for _, args := range [][]string{
		{"set", providercredentials.PersonEnrichmentSuppressionID, "--stdin"},
		{"import-env"},
	} {
		t.Run(args[0], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := credentialsTestConfig(t)
			cfg.People.Enrichment.SuppressionKeyEnv = "MSGVAULT_TEST_SUPPRESSION_KEY"
			shortKey := strings.Repeat("s", 31)
			t.Setenv(cfg.People.Enrichment.SuppressionKeyEnv, shortKey)
			_, err := runCredentialsTestCommand(t, cfg, shortKey, args...)
			require.ErrorContains(err, "suppression key must contain at least 32 bytes")
			snapshot, err := providercredentials.Read(cfg.TokensDir())
			require.NoError(err)
			assert.False(snapshot.Stored(providercredentials.PersonEnrichmentSuppressionID))

			validKey := strings.Repeat("s", 32)
			t.Setenv(cfg.People.Enrichment.SuppressionKeyEnv, validKey)
			_, err = runCredentialsTestCommand(t, cfg, validKey, args...)
			require.NoError(err)
			snapshot, err = providercredentials.Read(cfg.TokensDir())
			require.NoError(err)
			key, configured, err := snapshot.ResolveSuppression()
			require.NoError(err)
			assert.True(configured)
			assert.Equal(validKey, key)
		})
	}
}

func TestCredentialsImportEnvironmentPreservesStoredEntries(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	cfg := credentialsTestConfig(t)
	cfg.Vector.Embeddings.APIKeyEnv = "MSGVAULT_TEST_EMBED_KEY"
	cfg.People.Enrichment.SuppressionKeyEnv = "MSGVAULT_TEST_IMPORT_SUPPRESSION"
	t.Setenv(cfg.Vector.Embeddings.APIKeyEnv, "initial-environment-key")
	t.Setenv(cfg.People.Enrichment.SuppressionKeyEnv, "stable-suppression-key-32bytes-123")
	output, err := runCredentialsTestCommand(t, cfg, "", "import-env")
	require.NoError(err)
	assert.NotContains(output, "initial-environment-key")
	snapshot, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	firstETag := snapshot.ETag
	t.Setenv(cfg.Vector.Embeddings.APIKeyEnv, "rotated-environment-key")
	_, err = runCredentialsTestCommand(t, cfg, "", "import-env")
	require.NoError(err)
	snapshot, err = providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	assert.Equal(firstETag, snapshot.ETag)
	key, _, err := snapshot.Resolve(providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, cfg.Vector.Embeddings.APIKeyEnv, os.LookupEnv)
	require.NoError(err)
	assert.Equal("initial-environment-key", key)
}

func TestCredentialsRejectInvalidInputsAndPeopleProviderID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input string
		args        []string
	}{
		{"missing input", "", []string{"set", providercredentials.VectorEmbeddingsID}},
		{"empty input", " \n", []string{"set", providercredentials.VectorEmbeddingsID, "--stdin"}},
		{"oversized input", strings.Repeat("x", 65537), []string{"set", providercredentials.VectorEmbeddingsID, "--stdin"}},
		{"people provider key", "key", []string{"set", providercredentials.PeopleProviderID("remote"), "--stdin", "--endpoint", "https://provider.example.test"}},
		{"suppression endpoint", "key", []string{"set", providercredentials.PersonEnrichmentSuppressionID, "--stdin", "--endpoint", "https://provider.example.test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := credentialsTestConfig(t)
			_, err := runCredentialsTestCommand(t, cfg, tc.input, tc.args...)
			require.Error(t, err)
			snapshot, err := providercredentials.Read(cfg.TokensDir())
			require.NoError(t, err)
			assert.False(t, snapshot.Stored(providercredentials.VectorEmbeddingsID))
		})
	}
}
