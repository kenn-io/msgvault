package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

const setupProvidersTestKey = "setup-providers-test-key"

// setupProvidersFixture is one operator machine: a real config file, a real
// archive store, a fixed environment, and a fixed filesystem view for the
// probe manifests.
type setupProvidersFixture struct {
	dir     string
	path    string
	store   *store.Store
	env     map[string]string
	files   map[string]bool
	ollama  ollamaProbeResult
	checker *fixedPersonProviderChecker
	input   io.Reader
	tty     bool
}

func newSetupProvidersFixture(t *testing.T, content string) *setupProvidersFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if content != "" {
		content = strings.ReplaceAll(content, "{{DIR}}", filepath.ToSlash(dir))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return &setupProvidersFixture{
		dir:   dir,
		path:  path,
		store: testutil.NewSQLiteTestStore(t),
		env:   map[string]string{},
		files: map[string]bool{},
		checker: &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
			Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
			ModelVersion: "setup-model-v1",
		}},
	}
}

func (f *setupProvidersFixture) load(t *testing.T) *config.Config {
	t.Helper()
	snapshot, err := config.ReadConfigFile(f.path)
	require.NoError(t, err)
	loaded, err := loadSetupConfig(snapshot, f.dir)
	require.NoError(t, err)
	return loaded
}

func (f *setupProvidersFixture) lookupEnv(name string) (string, bool) {
	value, ok := f.env[name]
	return value, ok
}

func (f *setupProvidersFixture) personProviderDeps(t *testing.T) personProviderCommandDeps {
	t.Helper()
	loaded := f.load(t)
	deps := localPersonProviderDeps(loaded.People.Sweep, f.store, f.checker)
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(f.path) }
	deps.configHomeDir = func() string { return f.dir }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(f.path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(f.path, published, before)
	}
	deps.setup = personProviderSetupDeps{
		lookupEnv: f.lookupEnv,
		negotiate: func(_ context.Context, candidate peoplesweep.ProviderConfig, credential peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
			if credential.Scheme != peoplesweep.AuthNone {
				assert.Equal(t, setupProvidersTestKey, credential.Value())
			}
			return peoplesweep.NegotiatedCapabilities{
				OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
				ReasoningEffort: candidate.ReasoningEffort, DriverVersion: peoplesweep.OpenAIChatProviderVersion,
			}, nil
		},
	}
	return deps
}

func (f *setupProvidersFixture) deps(t *testing.T) setupProvidersDeps {
	t.Helper()
	return setupProvidersDeps{
		lookupEnv:  f.lookupEnv,
		fileExists: func(path string) bool { return f.files[path] },
		readConfigFile: func() (config.ConfigFile, error) {
			return config.ReadConfigFile(f.path)
		},
		editConfigTables: func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
			return config.EditConfigTables(f.path, etag, edits)
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			return config.RestoreConfigFile(f.path, published, before)
		},
		loadConfig: func(snapshot config.ConfigFile) (*config.Config, error) {
			return loadSetupConfig(snapshot, f.dir)
		},
		remoteConfigured: func() bool { return false },
		isTerminal:       func(*cobra.Command) bool { return f.tty },
		probeOllama:      func(context.Context, string) ollamaProbeResult { return f.ollama },
		consentState: func(ctx context.Context, loaded *config.Config) *setupConsentState {
			return setupConsentFromStore(ctx, loaded, f.store)
		},
		daemonAlive:    func(context.Context, *config.Config) bool { return false },
		personProvider: func() personProviderCommandDeps { return f.personProviderDeps(t) },
		now:            func() time.Time { return time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC) },
	}
}

func (f *setupProvidersFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	setup := &cobra.Command{Use: "setup"}
	setup.AddCommand(newSetupProvidersCommand(f.deps(t)))
	setup.AddCommand(newSetupStatusCommand(setupStatusDeps{
		config: func() *config.Config { return f.load(t) },
		environment: func(command *cobra.Command, loaded *config.Config) setupEnvironment {
			return setupEnvironment{
				lookupEnv:  f.lookupEnv,
				fileExists: func(path string) bool { return f.files[path] },
				consent:    setupConsentFromStore(command.Context(), loaded, f.store),
			}
		},
	}))
	root.AddCommand(setup)
	root.SetArgs(append([]string{"setup"}, args...))
	input := f.input
	if input == nil {
		input = strings.NewReader("")
	}
	root.SetIn(input)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err := root.ExecuteContext(t.Context())
	return output.String(), err
}

func (f *setupProvidersFixture) readConfig(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(content)
}

const setupProvidersMinimalConfig = `# operator comment survives setup
[data]
data_dir = "{{DIR}}/data"
`

func TestSetupProvidersPostgresRequiresCompiledBackend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+`database_url = "postgres://localhost/setup_test"`)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	fixture.files[filepath.Join(fixture.dir, setupVoyageManifestName)] = true
	output, err := fixture.run(t, "providers", "--yes", "--json")
	require.NoError(err, output)
	loaded := fixture.load(t)
	assert.Equal(pgvector.Available(), loaded.Vector.Enabled)
	assert.Equal(pgvector.Available(), loaded.Vector.Multimodal.Enabled)
	assert.Equal(pgvector.Available(), loaded.Vector.People.Enabled)
	previous := cfg
	cfg = loaded
	t.Cleanup(func() { cfg = previous })
	require.NoError(precheckVectorFeatures(loaded.DatabaseDSN()))
	if !pgvector.Available() {
		var result setupProvidersOutput
		require.NoError(json.Unmarshal([]byte(output), &result))
		assert.False(result.Applied)
		assert.Contains(output, "rebuild")
	}
}

func TestSetupProvidersRequiresSensitiveOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey
	output, err := fixture.run(t, "providers", "--yes", "--json")
	require.NoError(err, output)
	assert.False(fixture.load(t).People.Sweep.Enabled)
	assert.NotContains(fixture.load(t).People.Sweep.Providers, setupInferenceProfile)
	assert.Zero(fixture.checker.calls.Load())
	assert.Contains(output, "--allow-sensitive")

	output, err = fixture.run(t, "providers", "--yes", "--allow-sensitive", "--json")
	require.NoError(err, output)
	assert.Contains(output, "sensitive archive excerpts")
	assert.EqualValues(1, fixture.checker.calls.Load())
	profile, err := fixture.load(t).People.Sweep.Profile()
	require.NoError(err)
	consented, err := fixture.store.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(consented)
	assert.True(profile.AllowSensitive)
}

func TestSetupStatusReportsMissingHostedCredentials(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	for _, key := range []string{setupVoyageKeyEnv, "MISTRAL_API_KEY", setupOpenAIKeyEnv} {
		fixture.env[key] = setupProvidersTestKey
	}
	fixture.files[filepath.Join(fixture.dir, setupVoyageManifestName)] = true
	_, err := fixture.run(t, "providers", "--yes", "--allow-sensitive")
	require.NoError(err)
	for lane, key := range map[string]string{
		laneVisualSearch: setupVoyageKeyEnv, laneDocuments: "MISTRAL_API_KEY", lanePeopleInference: setupOpenAIKeyEnv,
	} {
		fixture.env[key] = "  "
		output, err := fixture.run(t, "status", "--json")
		require.NoError(err, output)
		var report laneReport
		require.NoError(json.Unmarshal([]byte(output), &report))
		status := findLane(t, report, lane)
		assert.Equal(laneStatePending, status.State)
		assert.Contains(status.Reason, key)
		assert.Contains(status.Reason, "not set")
		fixture.env[key] = setupProvidersTestKey
		output, err = fixture.run(t, "status", "--json")
		require.NoError(err, output)
		require.NoError(json.Unmarshal([]byte(output), &report))
		assert.Equal(laneStateOn, findLane(t, report, lane).State)
	}
}

func TestSetupProvidersVoyageWritesRecommendedDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey

	output, err := fixture.run(t, "providers", "--yes")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.True(loaded.Vector.Enabled)
	assert.Equal("sqlite-vec", loaded.Vector.Backend)
	assert.Equal(vector.APIFormatVoyageContextual, loaded.Vector.Embeddings.EffectiveAPIFormat())
	assert.Equal(setupVoyageTextModel, loaded.Vector.Embeddings.Model)
	assert.Equal(setupVoyageTextDim, loaded.Vector.Embeddings.Dimension)
	assert.Equal(setupVoyageEndpoint, loaded.Vector.Embeddings.Endpoint)
	assert.Equal(setupVoyageKeyEnv, loaded.Vector.Embeddings.APIKeyEnv)
	assert.True(loaded.Vector.Embed.Schedule.RunAfterSync)
	assert.Equal(setupEmbedCron, loaded.Vector.Embed.Schedule.Cron)
	assert.True(loaded.Vector.People.Enabled)
	assert.Equal(setupPostureDeclared, loaded.Vector.People.RetentionPosture)
	assert.Equal(setupPostureDeclared, loaded.Vector.People.TrainingPosture)
	// The visual lane stays off until the probe manifest exists: enabling it
	// without one makes the daemon refuse every vector lane.
	assert.False(loaded.Vector.Multimodal.Enabled)
	assert.True(loaded.Vector.Multimodal.Schedule.RunAfterSync)
	assert.False(loaded.Attachments.Documents.Enabled)
	assert.False(loaded.People.Sweep.Enabled)

	assert.Contains(fixture.readConfig(t), "# operator comment survives setup")
	assert.Contains(output, "Configuration written to")
	assert.Contains(output, "multimodal probe --seeds <private-seed-dir> --out "+setupVoyageManifestPath(loaded))
	assert.Contains(output, "msgvault embeddings build --yes")
	assert.Contains(output, "person provider consent --semantic-embeddings --yes")
	assert.NotContains(output, setupProvidersTestKey)

	// Re-running leaves a configured archive alone.
	before := fixture.readConfig(t)
	output, err = fixture.run(t, "providers", "--yes")
	require.NoError(err, output)
	assert.Equal(before, fixture.readConfig(t))
	assert.Contains(output, "already configured")
}

func TestSetupProvidersCreatesMissingConfigFile(t *testing.T) {
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, "")
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey

	output, err := fixture.run(t, "providers", "--yes")
	require.NoError(err, output)
	loaded := fixture.load(t)
	require.True(loaded.Vector.Enabled)
	require.Equal(setupVoyageTextModel, loaded.Vector.Embeddings.Model)
}

func TestSetupProvidersEnablesVisualLaneWhenManifestExists(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	manifest := filepath.Join(fixture.dir, setupVoyageManifestName)
	fixture.files[manifest] = true

	output, err := fixture.run(t, "providers", "--yes")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.True(loaded.Vector.Multimodal.Enabled)
	assert.Equal(manifest, loaded.Vector.Multimodal.CapabilitiesFile)
	assert.True(loaded.Vector.Multimodal.Schedule.RunAfterSync)
	assert.Equal(setupEmbedCron, loaded.Vector.Multimodal.Schedule.Cron)
	assert.Contains(output, "msgvault multimodal build --yes")
}

func TestSetupProvidersMistralEnablesDocumentsAndVectors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	fixture.env["MISTRAL_API_KEY"] = setupProvidersTestKey

	output, err := fixture.run(t, "providers", "--yes",
		"--document-retention", documentindex.RetentionZDR, "--document-training", documentindex.TrainingOptedOut)
	require.NoError(err, output)

	loaded := fixture.load(t)
	documents := loaded.Attachments.Documents
	assert.True(documents.Enabled)
	assert.Equal(documentindex.RetentionZDR, documents.RetentionPosture)
	assert.Equal(documentindex.TrainingOptedOut, documents.TrainingPosture)
	assert.Equal(documentindex.ModelMistralOCR, documents.Model)
	assert.True(documents.Index.Embeddings.Enabled)
	manifest := setupMistralManifestPath(loaded)
	assert.Contains(output, "documents probe-mistral --fixtures <private-fixture-dir> > "+manifest)
	assert.Contains(output, "documents consent-mistral --capabilities "+manifest+" --yes")
	assert.Contains(output, "msgvault documents vectors consent --yes")
	assert.Contains(output, "retention=zdr, training=opted-out")
}

func TestSetupProvidersRejectsUnknownDocumentPostures(t *testing.T) {
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env["MISTRAL_API_KEY"] = setupProvidersTestKey

	_, err := fixture.run(t, "providers", "--yes", "--document-retention", "unknown")
	require.ErrorContains(t, err, "--document-retention")
	assert.Empty(t, strings.TrimSpace(strings.TrimPrefix(fixture.readConfig(t), strings.ReplaceAll(setupProvidersMinimalConfig, "{{DIR}}", filepath.ToSlash(fixture.dir)))))
}

func TestSetupProvidersOpenAIFallbackOnboardsInference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey

	output, err := fixture.run(t, "providers", "--yes", "--allow-sensitive")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.True(loaded.Vector.Enabled)
	assert.Equal(vector.APIFormatOpenAI, loaded.Vector.Embeddings.EffectiveAPIFormat())
	assert.Equal(setupOpenAITextModel, loaded.Vector.Embeddings.Model)
	assert.Equal(setupOpenAITextDim, loaded.Vector.Embeddings.Dimension)
	assert.Equal(setupOpenAIKeyEnv, loaded.Vector.Embeddings.APIKeyEnv)
	assert.False(loaded.Vector.Multimodal.Enabled)

	sweep := loaded.People.Sweep
	require.True(sweep.Enabled)
	assert.Equal(setupInferenceProfile, sweep.Provider.Name)
	profile := sweep.Providers[setupInferenceProfile]
	assert.Equal(peoplesweep.ProtocolOpenAIChat, profile.Protocol)
	assert.Equal(setupOpenAIEndpoint, profile.Endpoint)
	assert.Equal(setupInferenceModel, profile.Model)
	assert.Equal(setupInferenceReasoning, profile.ReasoningEffort)
	assert.Equal(peoplesweep.CredentialEnv, profile.Credential)
	assert.Equal(setupOpenAIKeyEnv, profile.CredentialEnv)
	assert.Equal("2025-01-01", profile.SourceSince)
	assert.True(profile.AllowSensitive)
	assert.ElementsMatch([]peoplesweep.SourceClass{peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText}, profile.AllowedSources)

	// The onboarding went through the real add, check, consent, and use gates.
	fingerprint, err := sweep.Profile()
	require.NoError(err)
	checked, err := fixture.store.HasSuccessfulPersonInferenceCheck(t.Context(), fingerprint.Fingerprint)
	require.NoError(err)
	assert.True(checked)
	consented, err := fixture.store.HasActivePersonInferenceConsent(t.Context(), fingerprint.Fingerprint)
	require.NoError(err)
	assert.True(consented)
	assert.EqualValues(1, fixture.checker.calls.Load())

	assert.Contains(output, "no conversation-window context")
	assert.Contains(output, "msgvault person track <person-id>")
	assert.NotContains(output, setupProvidersTestKey)

	// Status now reports the sweep on with an active consent.
	status, err := fixture.run(t, "status", "--json")
	require.NoError(err, status)
	var report laneReport
	require.NoError(json.Unmarshal([]byte(status), &report))
	inference := findLane(t, report, lanePeopleInference)
	assert.Equal(laneStateOn, inference.State)
	assert.Equal(consentActive, inference.Consent)
	assert.Equal(setupInferenceModel, inference.Model)
}

func TestSetupProvidersLocalOllamaFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.ollama = ollamaProbeResult{Reachable: true, Models: []string{"nomic-embed-text:latest", "gpt-oss-128k:latest"}}

	output, err := fixture.run(t, "providers", "--allow-sensitive")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.True(loaded.Vector.Enabled)
	assert.Equal("http://localhost:11434/v1", loaded.Vector.Embeddings.Endpoint)
	assert.Equal(setupOllamaTextModel, loaded.Vector.Embeddings.Model)
	assert.Equal(setupOllamaTextDim, loaded.Vector.Embeddings.Dimension)
	assert.Equal(setupOllamaDocPrefix, loaded.Vector.Embeddings.DocumentPrefix)
	assert.Equal(setupOllamaQueryPrefix, loaded.Vector.Embeddings.QueryPrefix)
	assert.Equal(setupOllamaMaxInput, loaded.Vector.Embeddings.MaxInputChars)
	assert.Empty(loaded.Vector.Embeddings.APIKeyEnv)

	sweep := loaded.People.Sweep
	require.True(sweep.Enabled)
	assert.Equal(setupOllamaProfile, sweep.Provider.Name)
	profile := sweep.Providers[setupOllamaProfile]
	assert.Equal("gpt-oss-128k", profile.Model)
	assert.Equal(peoplesweep.AuthNone, profile.Auth)
	assert.Equal(peoplesweep.CredentialNone, profile.Credential)
	assert.Equal("http://localhost:11434/v1", profile.Endpoint)
	assert.Contains(output, "stays on this machine")
}

func TestSetupProvidersSkipsRemoteOllama(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+`
[chat]
server = "http://ollama.internal.example:11434"
`)
	fixture.ollama = ollamaProbeResult{Reachable: true, Models: []string{"nomic-embed-text:latest", "gpt-oss-128k:latest"}}
	before := fixture.readConfig(t)

	output, err := fixture.run(t, "providers", "--json")
	require.NoError(err, output)
	// A reachable server off this machine is never selected: it would receive
	// message text without a credential or a disclosure.
	assert.Equal(before, fixture.readConfig(t))
	var result setupProvidersOutput
	require.NoError(json.Unmarshal([]byte(output), &result))
	for _, lane := range result.Plan {
		assert.Equal(planActionSkip, lane.Action, lane.Lane)
		if lane.Lane == laneTextSearch || lane.Lane == lanePeopleInference {
			assert.Contains(lane.Reason, "not loopback")
		}
	}
}

func TestSetupProvidersOllamaWithoutChatModelSkipsInference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.ollama = ollamaProbeResult{Reachable: true, Models: []string{"nomic-embed-text:latest"}}

	output, err := fixture.run(t, "providers")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.True(loaded.Vector.Enabled)
	assert.Equal(setupOllamaTextModel, loaded.Vector.Embeddings.Model)
	// The embedding-only model is never promoted to the sweep's chat model.
	assert.False(loaded.People.Sweep.Enabled)
	assert.NotContains(loaded.People.Sweep.Providers, setupOllamaProfile)
	assert.Contains(output, "ollama pull gpt-oss-128k")
}

func TestSetupProvidersDisclosureListsInferenceSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey
	fixture.env["MISTRAL_API_KEY"] = setupProvidersTestKey

	output, err := fixture.run(t, "providers", "--dry-run", "--allow-sensitive")
	require.NoError(err, output)
	assert.Contains(output, "bounded evidence packets of conversation_text, meeting_text, document_text for tracked people")
	assert.Contains(output, "--allow-sensitive authorizes sending sensitive archive excerpts to OpenAI")
}

func TestSetupProvidersDeclinedDocumentsUpdateDependentLanes(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint("local=", local), func(t *testing.T) {
			assert := assert.New(t)
			fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
			fixture.env["MISTRAL_API_KEY"] = setupProvidersTestKey
			fixture.tty = true
			profileName := setupInferenceProfile
			if local {
				fixture.ollama = ollamaProbeResult{Reachable: true, Models: []string{"nomic-embed-text:latest", "gpt-oss-128k:latest"}}
				fixture.input = strings.NewReader("n\n")
				profileName = setupOllamaProfile
			} else {
				fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey
				fixture.input = strings.NewReader("n\ny\n")
			}

			output, err := fixture.run(t, "providers", "--allow-sensitive")
			require.NoError(t, err, output)
			loaded := fixture.load(t)
			assert.False(loaded.Attachments.Documents.Enabled)
			assert.False(loaded.Attachments.Documents.Index.Embeddings.Enabled)
			assert.ElementsMatch([]peoplesweep.SourceClass{peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText},
				loaded.People.Sweep.Providers[profileName].AllowedSources)
			assert.NotContains(output, "bounded evidence packets of conversation_text, meeting_text, document_text")
			assert.NotContains(output, "msgvault documents vectors consent --yes")
		})
	}
}

func TestSetupProvidersFailureRestoresConfig(t *testing.T) {
	for _, stage := range []string{"negotiate", "check", "consent", "selection"} {
		for _, content := range []string{"", setupProvidersMinimalConfig} {
			t.Run(fmt.Sprintf("%s/missing=%t", stage, content == ""), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				fixture := newSetupProvidersFixture(t, content)
				fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey
				fixture.env["MISTRAL_API_KEY"] = setupProvidersTestKey
				before := fixture.readConfig(t)
				failure := errors.New("provider setup failed")
				deps := fixture.deps(t)
				deps.personProvider = func() personProviderCommandDeps {
					provider := fixture.personProviderDeps(t)
					switch stage {
					case "negotiate":
						provider.setup.negotiate = func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
							return peoplesweep.NegotiatedCapabilities{}, failure
						}
					case "check":
						fixture.checker.err = failure
					case "consent":
						openStore := provider.openStore
						provider.openStore = func() (personProviderStore, func(), error) {
							if fixture.checker.calls.Load() > 0 {
								return nil, nil, failure
							}
							return openStore()
						}
					case "selection":
						provider.openReadStore = func() (personProviderStore, func(), error) {
							return nil, nil, failure
						}
					}
					return provider
				}
				command := newSetupProvidersCommand(deps)
				command.SetArgs([]string{"--yes", "--allow-sensitive"})
				command.SetOut(io.Discard)
				command.SetErr(io.Discard)

				err := command.ExecuteContext(t.Context())
				require.ErrorIs(err, failure)
				assert.Equal(before, fixture.readConfig(t))
				if content == "" {
					_, err := os.Stat(fixture.path)
					require.ErrorIs(err, os.ErrNotExist)
				}
				fixture.checker.err = nil
				output, err := fixture.run(t, "providers", "--yes", "--allow-sensitive")
				require.NoError(err, output)
				assert.True(fixture.load(t).People.Sweep.Enabled)
			})
		}
	}
}

func TestSetupProvidersRollbackPreservesConcurrentConfig(t *testing.T) {
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey
	deps := fixture.deps(t)
	failure := errors.New("provider check failed")
	var concurrent config.ConfigFile
	deps.personProvider = func() personProviderCommandDeps {
		provider := fixture.personProviderDeps(t)
		provider.newChecker = func(peoplesweep.Config, personProviderStore) (personProviderChecker, error) {
			return callbackPersonProviderChecker(func(context.Context) (peoplesweep.StructuredResponse, error) {
				before, err := config.ReadConfigFile(fixture.path)
				require.NoError(t, err)
				concurrent, err = config.EditConfigTables(fixture.path, before.ETag, []config.TableEdit{{
					Path: []string{"activity"}, Values: map[string]any{"schedule": "0 * * * *"},
				}})
				require.NoError(t, err)
				return peoplesweep.StructuredResponse{}, failure
			}), nil
		}
		return provider
	}
	command := newSetupProvidersCommand(deps)
	command.SetArgs([]string{"--yes", "--allow-sensitive"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)

	err := command.ExecuteContext(t.Context())
	require.ErrorIs(t, err, failure)
	require.ErrorIs(t, err, config.ErrConfigConflict)
	assert.Equal(t, string(concurrent.Content), fixture.readConfig(t))
}

func TestSetupProvidersWithoutProvidersReportsEveryLaneOff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	before := fixture.readConfig(t)

	output, err := fixture.run(t, "providers", "--json")
	require.NoError(err, output)
	assert.Equal(before, fixture.readConfig(t))

	var result setupProvidersOutput
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.False(result.Applied)
	for _, lane := range result.Plan {
		assert.Equal(planActionSkip, lane.Action, lane.Lane)
	}
	text := findLane(t, result.Report, laneTextSearch)
	assert.Equal(laneStateOff, text.State)
	assert.Contains(text.Reason, setupVoyageKeyEnv)
}

func TestSetupProvidersDryRunWritesNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	before := fixture.readConfig(t)

	output, err := fixture.run(t, "providers", "--dry-run")
	require.NoError(err, output)
	assert.Equal(before, fixture.readConfig(t))
	assert.Contains(output, "Dry run: nothing written.")
	assert.Contains(output, "Voyage AI ("+setupVoyageEndpoint+") receives:")
	assert.Contains(output, "message, chat, and meeting text")
}

func TestSetupProvidersRequiresConsentWithoutTerminal(t *testing.T) {
	assert := assert.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	before := fixture.readConfig(t)

	_, err := fixture.run(t, "providers")
	require.ErrorContains(t, err, "--yes")
	require.ErrorContains(t, err, gateVoyage)
	assert.Equal(before, fixture.readConfig(t))
}

func TestSetupProvidersDeclinedProviderWritesNothingForIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	fixture.env["MISTRAL_API_KEY"] = setupProvidersTestKey
	fixture.tty = true
	// Decline Voyage, accept Mistral.
	fixture.input = strings.NewReader("n\ny\n")

	output, err := fixture.run(t, "providers")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.False(loaded.Vector.Enabled)
	assert.False(loaded.Vector.People.Enabled)
	assert.True(loaded.Attachments.Documents.Enabled)
	// Document vectors need the declined text lane, so they were not enabled
	// even though the Mistral gate was accepted.
	assert.False(loaded.Attachments.Documents.Index.Embeddings.Enabled)
	assert.Contains(output, "Enable the voyage lanes? [y/N]:")
	assert.Contains(output, "Enable the mistral lanes? [y/N]:")
}

func TestSetupProvidersKeepsConfiguredTextLane(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+`
[vector]
enabled = true

[vector.embeddings]
endpoint = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"
model = "text-embedding-3-large"
dimension = 3072
`)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	fixture.env[setupOpenAIKeyEnv] = setupProvidersTestKey

	output, err := fixture.run(t, "providers", "--yes", "--allow-sensitive")
	require.NoError(err, output)

	loaded := fixture.load(t)
	assert.Equal("text-embedding-3-large", loaded.Vector.Embeddings.Model)
	assert.Equal(3072, loaded.Vector.Embeddings.Dimension)
	assert.Equal(vector.APIFormatOpenAI, loaded.Vector.Embeddings.EffectiveAPIFormat())
	// The unset lanes still gained defaults.
	assert.True(loaded.Vector.People.Enabled)
	assert.True(loaded.People.Sweep.Enabled)
	assert.Contains(output, "embeddings build --full-rebuild")
}

func TestSetupProvidersRefusesConfiguredRemote(t *testing.T) {
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey
	deps := fixture.deps(t)
	deps.remoteConfigured = func() bool { return true }
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newSetupProvidersCommand(deps))
	root.SetArgs([]string{"providers", "--yes"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "remote daemon")
	assert.Equal(t, strings.ReplaceAll(setupProvidersMinimalConfig, "{{DIR}}", filepath.ToSlash(fixture.dir)), fixture.readConfig(t))
}

func TestSetupStatusReportsPendingLanesForPresentKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig)
	fixture.env[setupVoyageKeyEnv] = setupProvidersTestKey

	output, err := fixture.run(t, "status", "--json")
	require.NoError(err, output)
	var report laneReport
	require.NoError(json.Unmarshal([]byte(output), &report))

	text := findLane(t, report, laneTextSearch)
	assert.Equal(laneStatePending, text.State)
	assert.Equal([]string{"msgvault setup providers"}, text.Next)
	visual := findLane(t, report, laneVisualSearch)
	assert.Equal(laneStatePending, visual.State)
	documents := findLane(t, report, laneDocuments)
	assert.Equal(laneStateOff, documents.State)
	assert.Contains(documents.Reason, "MISTRAL_API_KEY")
	activity := findLane(t, report, laneActivity)
	assert.Equal(laneStateOn, activity.State)
	assert.Equal("cron 17 * * * *", activity.Schedule)
	media := findLane(t, report, laneMediaPolicy)
	assert.Contains(media.Reason, "beeper scope all")
	assert.Equal([]string{"search_people", "get_person_notes", "get_person_relationship", "search_person_files"}, report.MCPTools)

	human, err := fixture.run(t, "status")
	require.NoError(err, human)
	assert.Contains(human, "LANE")
	assert.Contains(human, "Text search (messages, chats, meetings)")
	assert.Contains(human, "MCP tools live with this configuration")
}

func findLane(t *testing.T, report laneReport, lane string) laneStatus {
	t.Helper()
	for _, item := range report.Lanes {
		if item.Lane == lane {
			return item
		}
	}
	require.Failf(t, "lane missing", "lane %q not in report", lane)
	return laneStatus{}
}
