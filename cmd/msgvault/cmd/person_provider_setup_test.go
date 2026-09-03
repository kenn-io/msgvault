package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/testutil"
)

const providerSetupSecretCanary = "provider-setup-secret-canary"

type providerCredentialChunkReader struct {
	chunks [][]byte
}

type countingProviderReader struct {
	reads int
	data  *bytes.Reader
}

type observedProviderReader struct {
	data           *bytes.Reader
	maxDestination int
}

func (r *countingProviderReader) Read(destination []byte) (int, error) {
	r.reads++
	return r.data.Read(destination) //nolint:wrapcheck // Test reader transparently delegates to bytes.Reader.
}

func (r *observedProviderReader) Read(destination []byte) (int, error) {
	if len(destination) > r.maxDestination {
		r.maxDestination = len(destination)
	}
	return r.data.Read(destination) //nolint:wrapcheck // Test reader transparently delegates to bytes.Reader.
}

func (r *providerCredentialChunkReader) Read(destination []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	read := copy(destination, chunk)
	if read == len(chunk) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = chunk[read:]
	}
	return read, nil
}

func TestReadProviderCredentialLineRejectsTrailingChunk(t *testing.T) {
	reader := &providerCredentialChunkReader{chunks: [][]byte{
		[]byte(providerSetupSecretCanary + "\n"),
		[]byte("attacker-controlled-trailing-data"),
	}}

	credential, err := readProviderCredentialLine(reader)
	require.Error(t, err)
	assert.Empty(t, credential)
	assert.NotContains(t, err.Error(), providerSetupSecretCanary)
}

func TestReadBoundedMaskedCredentialInputRejectsBeforeGrowingPastLimit(t *testing.T) {
	reader := &observedProviderReader{data: bytes.NewReader([]byte("123456789-discarded\n"))}

	credential, err := readBoundedMaskedCredentialInput(reader, 8)
	require.ErrorContains(t, err, "too large")
	assert.Empty(t, credential)
	assert.Equal(t, 1, reader.maxDestination)
}

func TestReadBoundedMaskedCredentialInputHandlesEditingAndCancel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	credential, err := readBoundedMaskedCredentialInput(bytes.NewBuffer([]byte{'a', 'b', 0x7f, 'c', '\r'}), 8)
	require.NoError(err)
	assert.Equal([]byte("ac"), credential)

	credential, err = readBoundedMaskedCredentialInput(bytes.NewBuffer([]byte{'a', 0x03}), 8)
	require.ErrorContains(err, "canceled")
	assert.Empty(credential)
}

func TestReadBoundedMaskedCredentialRestoresTerminalOnEveryReadResult(t *testing.T) {
	for _, test := range []struct {
		name       string
		input      []byte
		restoreErr error
	}{
		{name: "success", input: []byte("safe\n")},
		{name: "cancel", input: []byte{'x', 0x03}},
		{name: "too large", input: []byte("123456789\n")},
		{name: "restore error", input: []byte("safe\n"), restoreErr: errors.New("restore failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			newAssert := assert.New
			assert := assert.New(t)
			require := require.New(t)
			state := &term.State{}
			makeCalls := 0
			restoreCalls := 0

			credential, err := readBoundedMaskedCredentialWithTerminal(
				bytes.NewReader(test.input), 17, 8,
				func(fd uintptr) (*term.State, error) {
					assert := newAssert(t)
					makeCalls++
					assert.Equal(uintptr(17), fd)
					return state, nil
				},
				func(fd uintptr, restored *term.State) error {
					assert := newAssert(t)
					restoreCalls++
					assert.Equal(uintptr(17), fd)
					assert.Same(state, restored)
					return test.restoreErr
				},
			)
			assert.Equal(1, makeCalls)
			assert.Equal(1, restoreCalls)
			if test.restoreErr != nil {
				require.ErrorContains(err, "restore credential terminal")
				assert.Empty(credential)
			}
		})
	}
}

// TestPersonProviderAddRejectsCodexProtocolBeforeCatalogOrState pins that
// generic onboarding never accepts codex_app_server: the generic path demands
// an HTTP endpoint that Codex validation forbids, and capability
// negotiation deliberately excludes the Codex process transport. Codex
// profiles are configured manually per the documented path instead.
func TestPersonProviderAddRejectsCodexProtocolBeforeCatalogOrState(t *testing.T) {
	for _, flags := range [][]string{
		{"--custom"},
		{"--endpoint", "https://codex.example.test/v1"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			assertAnError := assert.AnError
			assert := assert.New(t)
			require := require.New(t)
			calls := 0
			deps := personProviderCommandDeps{
				config: func() peoplesweep.Config {
					calls++
					return personProviderTestConfig()
				},
				readConfigFile: func() (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assertAnError
				},
				editConfigTables: func(string, []config.TableEdit) (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assertAnError
				},
				restoreConfigFile: func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assertAnError
				},
				setup: personProviderSetupDeps{
					catalog: func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
						calls++
						return nil, nil
					},
					lookupEnv: func(string) (string, bool) {
						calls++
						return providerSetupSecretCanary, true
					},
					negotiate: func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
						calls++
						return peoplesweep.NegotiatedCapabilities{}, nil
					},
				},
			}
			args := []string{
				"add", "codex-profile", "--protocol", "codex_app_server",
				"--model", "codex-model", "--auth", "none",
				"--retention-posture", "local_only", "--training-posture", "local_only",
				"--source", "conversation_text", "--source-since", "2025-01-01", "--yes",
			}
			args = append(args, flags...)

			_, err := executePersonProviderCommand(t, deps, args...)
			require.Error(err)
			assert.Contains(err.Error(), "codex_app_server")
			assert.Contains(err.Error(), "config.toml")
			assert.Contains(err.Error(), `auth = "none"`)
			assert.Contains(err.Error(), `credential = "none"`)
			assert.Contains(err.Error(), "no endpoint")
			assert.Contains(err.Error(), "person provider login")
			assert.Zero(calls)
		})
	}
}

func TestPersonProviderAddValidatesPolicyBeforeReadingCredentialOrNegotiating(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, nil)
	var lookups, negotiations int
	deps.setup.lookupEnv = func(string) (string, bool) {
		lookups++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		negotiations++
		return peoplesweep.NegotiatedCapabilities{}, nil
	}

	output, err := executePersonProviderCommand(t, deps,
		"add", "unsafe-provider", "--custom", "--protocol", "openai_chat",
		"--endpoint", "http://provider.example.test/v1", "--model", "unsafe-model",
		"--auth", "bearer", "--credential-env", "EXACT_UNSAFE_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
	require.Error(err)
	assert.Contains(err.Error(), "HTTPS")
	assert.Zero(lookups)
	assert.Zero(negotiations)
	assert.NotContains(output, providerSetupSecretCanary)
}

func TestPersonProviderAddRejectsLocalOptionConflictsBeforeCatalogOrState(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
	}{
		{name: "custom catalog prices", flags: []string{"--custom", "--accept-catalog-prices", "--yes"}},
		{name: "missing confirmation", flags: []string{"--custom"}},
		{name: "mixed credential inputs", flags: []string{"--custom", "--api-key-stdin", "--credential-env", "EXACT_KEY", "--yes"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			deps := personProviderCommandDeps{
				config:         func() peoplesweep.Config { calls++; return personProviderTestConfig() },
				readConfigFile: func() (config.ConfigFile, error) { calls++; return config.ConfigFile{}, assert.AnError },
				editConfigTables: func(string, []config.TableEdit) (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assert.AnError
				},
				restoreConfigFile: func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error) {
					calls++
					return config.ConfigFile{}, assert.AnError
				},
				setup: personProviderSetupDeps{
					catalog:   func(context.Context) ([]peoplesweep.ProviderSuggestion, error) { calls++; return nil, nil },
					lookupEnv: func(string) (string, bool) { calls++; return providerSetupSecretCanary, true },
					negotiate: func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
						calls++
						return peoplesweep.NegotiatedCapabilities{}, nil
					},
				},
			}
			args := []string{
				"add", "local-options", "--protocol", "openai_chat",
				"--endpoint", "https://options.example.test/v1", "--model", "options-model",
				"--auth", "bearer", "--retention-posture", "zero_retention",
				"--training-posture", "no_training", "--source", "conversation_text",
				"--source-since", "2025-01-01",
			}
			args = append(args, test.flags...)

			_, err := executePersonProviderCommand(t, deps, args...)
			require.Error(t, err)
			assert.Zero(t, calls)
		})
	}
}

// TestPersonProviderAddRejectsRemoteDaemonBeforeAnyLocalWrite pins the remote
// trust boundary: with a configured remote daemon, local config edits and
// credential publication are invisible to the daemon that must validate the
// profile, so setup must refuse before touching catalog, config, or secret
// state instead of writing locally and failing at the proxied check.
func TestPersonProviderAddRejectsRemoteDaemonBeforeAnyLocalWrite(t *testing.T) {
	assertAnError := assert.AnError
	assert := assert.New(t)
	require := require.New(t)
	calls := 0
	deps := personProviderCommandDeps{
		remoteConfigured: func() bool { return true },
		config:           func() peoplesweep.Config { calls++; return personProviderTestConfig() },
		readConfigFile: func() (config.ConfigFile, error) {
			calls++
			return config.ConfigFile{}, assertAnError
		},
		editConfigTables: func(string, []config.TableEdit) (config.ConfigFile, error) {
			calls++
			return config.ConfigFile{}, assertAnError
		},
		restoreConfigFile: func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error) {
			calls++
			return config.ConfigFile{}, assertAnError
		},
		providerStoreOwnedByDaemon: func(context.Context) (bool, error) {
			calls++
			return true, nil
		},
		proxy: func(*cobra.Command, []string, map[string]string) error {
			calls++
			return assertAnError
		},
		setup: personProviderSetupDeps{
			catalog: func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
				calls++
				return nil, nil
			},
			lookupEnv: func(string) (string, bool) {
				calls++
				return providerSetupSecretCanary, true
			},
			negotiate: func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
				calls++
				return peoplesweep.NegotiatedCapabilities{}, nil
			},
			credentials: countingCredentialStore{calls: &calls},
		},
	}

	_, err := executePersonProviderCommand(t, deps,
		"add", "remote-provider", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://remote.example.test/v1", "--model", "remote-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.Error(err)
	assert.NotContains(err.Error(), providerSetupSecretCanary)
	assert.Contains(err.Error(), "remote daemon")
	assert.Contains(err.Error(), "--local")
	assert.Zero(calls, "remote setup must not read config, catalog, credentials, or proxy anything")
}

type countingCredentialStore struct {
	calls *int
}

func (s countingCredentialStore) Save(string, peoplesweep.Credential) error {
	*s.calls++
	return nil
}

func (s countingCredentialStore) Load(string) (peoplesweep.Credential, error) {
	*s.calls++
	return peoplesweep.Credential{}, nil
}

func (s countingCredentialStore) PreflightDelete(string) (peoplesweep.CredentialDeleteGuard, error) {
	*s.calls++
	return nil, assert.AnError
}

func (s countingCredentialStore) Delete(string, peoplesweep.CredentialDeleteGuard) error {
	*s.calls++
	return nil
}

func TestPersonProviderAddCatalogResolvesUnambiguousTransportBeforeCredentialOrState(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "catalog-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	catalogCalls, credentialReads, negotiations, writes := 0, 0, 0, 0
	deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
		catalogCalls++
		return []peoplesweep.ProviderSuggestion{{
			ID: "catalog", Name: "Catalog", Endpoint: "https://api.openai.com/v1",
			Models:             []peoplesweep.ModelSuggestion{{ID: "catalog-model", Name: "Catalog Model"}},
			ProtocolCandidates: []peoplesweep.Protocol{peoplesweep.ProtocolOpenAIChat},
		}}, nil
	}
	deps.setup.lookupEnv = func(name string) (string, bool) {
		assert := newAssert(t)
		credentialReads++
		assert.Equal("CATALOG_KEY", name)
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		_ context.Context, candidate peoplesweep.ProviderConfig, credential peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		assert := newAssert(t)
		negotiations++
		assert.Equal(peoplesweep.ProtocolOpenAIChat, candidate.Protocol)
		assert.Equal("https://api.openai.com/v1", candidate.Endpoint)
		assert.Equal("catalog-model", candidate.Model)
		assert.Equal(peoplesweep.AuthBearer, candidate.Auth)
		assert.Equal(peoplesweep.AuthBearer, credential.Scheme)
		return peoplesweep.NegotiatedCapabilities{
			OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
			DriverVersion: peoplesweep.OpenAIChatProviderVersion,
		}, nil
	}
	nativeEdit := deps.editConfigTables
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		writes++
		return nativeEdit(etag, edits)
	}

	output, err := executePersonProviderCommand(t, deps,
		"add", "catalog-provider", "--credential-env", "CATALOG_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
	require.NoError(err)
	assert.Equal(1, catalogCalls)
	assert.Equal(1, credentialReads)
	assert.Equal(1, negotiations)
	assert.Equal(1, writes)
	assert.Contains(output, "Suggestion: Catalog (catalog) endpoint=https://api.openai.com/v1")
	assert.Contains(output,
		`Final provider "catalog-provider": endpoint=https://api.openai.com/v1 protocol=openai_chat model=catalog-model auth=bearer`)
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(content), `[people.sweep.providers.catalog-provider]`)
}

// TestPersonProviderAddNeverSendsCredentialToCatalogEndpoint catches
// onboarding reading a credential or negotiating capabilities against an
// endpoint chosen by the models.dev catalog: a compromised catalog must not
// be able to redirect a credential to itself. Onboarding may transmit a
// credential only to an endpoint the operator explicitly supplied or to a
// first-party API host compiled into the binary.
func TestPersonProviderAddNeverSendsCredentialToCatalogEndpoint(t *testing.T) {
	for _, mode := range []string{"credential-env", "api-key-stdin"} {
		t.Run(mode, func(t *testing.T) {
			newAssert := assert.New
			assert := assert.New(t)
			require := require.New(t)
			path, loaded := providerSetupConfigFile(t)
			deps := providerSetupCommandDeps(t, path, loaded, nil)
			deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
				return []peoplesweep.ProviderSuggestion{{
					ID: "catalog", Name: "Catalog", Endpoint: "https://catalog.example.test/v1",
					Models:             []peoplesweep.ModelSuggestion{{ID: "catalog-model", Name: "Catalog Model"}},
					ProtocolCandidates: []peoplesweep.Protocol{peoplesweep.ProtocolOpenAIChat},
				}}, nil
			}
			credentialReads := 0
			deps.setup.lookupEnv = func(string) (string, bool) {
				credentialReads++
				return providerSetupSecretCanary, true
			}
			negotiations := 0
			deps.setup.negotiate = func(
				_ context.Context, candidate peoplesweep.ProviderConfig, credential peoplesweep.Credential,
			) (peoplesweep.NegotiatedCapabilities, error) {
				assert := newAssert(t)
				negotiations++
				assert.Equal("https://catalog.example.test/v1", candidate.Endpoint)
				assert.Equal(providerSetupSecretCanary, credential.Value())
				return peoplesweep.NegotiatedCapabilities{}, nil
			}
			writes := 0
			nativeEdit := deps.editConfigTables
			deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
				writes++
				return nativeEdit(etag, edits)
			}

			args := []string{"add", "catalog-provider", "--retention-posture", "zero_retention",
				"--training-posture", "no_training", "--source", "conversation_text",
				"--source-since", "2025-01-01", "--yes"}
			var output string
			var err error
			if mode == "credential-env" {
				output, err = executePersonProviderCommand(t, deps,
					append(args, "--credential-env", "CATALOG_KEY")...)
			} else {
				input := &bytes.Buffer{}
				input.WriteString(providerSetupSecretCanary + "\n")
				output, err = executePersonProviderCommandWithInput(t, deps, input,
					append(args, "--api-key-stdin")...)
			}
			require.Error(err)
			assert.Contains(err.Error(), "--endpoint")
			assert.NotContains(err.Error(), providerSetupSecretCanary)
			assert.NotContains(output, providerSetupSecretCanary)
			assert.Zero(credentialReads, "catalog-selected endpoint must not receive any credential")
			assert.Zero(negotiations, "catalog-selected endpoint must not be contacted")
			assert.Zero(writes)
			content, readErr := os.ReadFile(path)
			require.NoError(readErr)
			assert.NotContains(string(content), "catalog-provider")
		})
	}
}

// TestPersonProviderAddExplicitEndpointMayReceiveCredential proves the gate
// is endpoint provenance, not a host blocklist: the same catalog-listed
// endpoint becomes eligible for a credential when the operator supplies it
// explicitly with --endpoint.
func TestPersonProviderAddExplicitEndpointMayReceiveCredential(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "catalog-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
		return []peoplesweep.ProviderSuggestion{{
			ID: "catalog", Name: "Catalog", Endpoint: "https://catalog.example.test/v1",
			Models:             []peoplesweep.ModelSuggestion{{ID: "catalog-model", Name: "Catalog Model"}},
			ProtocolCandidates: []peoplesweep.Protocol{peoplesweep.ProtocolOpenAIChat},
		}}, nil
	}
	credentialReads, negotiations := 0, 0
	deps.setup.lookupEnv = func(string) (string, bool) {
		credentialReads++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		_ context.Context, candidate peoplesweep.ProviderConfig, credential peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		assert := newAssert(t)
		negotiations++
		assert.Equal("https://catalog.example.test/v1", candidate.Endpoint)
		assert.Equal(providerSetupSecretCanary, credential.Value())
		return peoplesweep.NegotiatedCapabilities{
			OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
			DriverVersion: peoplesweep.OpenAIChatProviderVersion,
		}, nil
	}

	_, err := executePersonProviderCommand(t, deps,
		"add", "explicit-provider", "--endpoint", "https://catalog.example.test/v1",
		"--credential-env", "CATALOG_KEY", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.NoError(err)
	assert.Equal(1, credentialReads)
	assert.Equal(1, negotiations)
	content, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Contains(string(content), `[people.sweep.providers.explicit-provider]`)
}

func TestPersonProviderAcceptedCatalogPriceMustResolveBeforeSecretOrProviderCall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, nil)
	catalogCalls, lookups, negotiations := 0, 0, 0
	deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
		catalogCalls++
		return []peoplesweep.ProviderSuggestion{{
			ID: "incomplete", Name: "Incomplete", Endpoint: "https://prices.example.test/v1",
			Models: []peoplesweep.ModelSuggestion{{ID: "prices-model", Name: "Prices Model"}},
		}}, nil
	}
	deps.setup.lookupEnv = func(string) (string, bool) {
		lookups++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		negotiations++
		return peoplesweep.NegotiatedCapabilities{}, nil
	}

	_, err := executePersonProviderCommand(t, deps,
		"add", "prices", "--protocol", "openai_chat",
		"--endpoint", "https://prices.example.test/v1", "--model", "prices-model",
		"--auth", "bearer", "--credential-env", "EXACT_PRICES_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01",
		"--accept-catalog-prices", "--yes")
	require.ErrorContains(err, "catalog price")
	assert.Equal(1, catalogCalls)
	assert.Zero(lookups)
	assert.Zero(negotiations)
}

func TestPersonProviderAcceptedCatalogPricesValidateProposedBudgetBeforeSecretOrProvider(t *testing.T) {
	for _, test := range []struct {
		name      string
		input     int64
		output    int64
		wantError string
	}{
		{name: "zero prices with cost cap", input: 0, output: 0, wantError: "prices are required"},
		{name: "overflowing reservation", input: math.MaxInt64, output: 1, wantError: "overflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path, _ := providerSetupConfigFile(t)
			snapshot, err := config.ReadConfigFile(path)
			require.NoError(err)
			withCap, err := config.EditConfigFile(path, snapshot.ETag, []config.Edit{{
				Key: "people.sweep.budgets.max_estimated_cost_microusd_per_run", Value: int64(10_000),
			}})
			require.NoError(err)
			loaded, err := config.LoadConfigFile(withCap, "")
			require.NoError(err)
			checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
				Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
				ModelVersion: "prices-model-v1",
			}}
			deps := providerSetupCommandDeps(t, path, loaded, checker)
			catalogCalls, credentialReads, negotiations, writes := 0, 0, 0, 0
			deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
				catalogCalls++
				return []peoplesweep.ProviderSuggestion{{
					ID: "prices", Name: "Prices", Endpoint: "https://prices.example.test/v1",
					Models: []peoplesweep.ModelSuggestion{{
						ID: "prices-model", Name: "Prices Model",
						InputCostMicroUSDPerMillionTokens:  &test.input,
						OutputCostMicroUSDPerMillionTokens: &test.output,
					}},
				}}, nil
			}
			deps.setup.lookupEnv = func(string) (string, bool) {
				credentialReads++
				return providerSetupSecretCanary, true
			}
			deps.setup.negotiate = func(
				context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
			) (peoplesweep.NegotiatedCapabilities, error) {
				negotiations++
				return peoplesweep.NegotiatedCapabilities{}, nil
			}
			nativeEdit := deps.editConfigTables
			deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
				writes++
				return nativeEdit(etag, edits)
			}

			_, err = executePersonProviderCommand(t, deps,
				"add", "prices", "--protocol", "openai_chat",
				"--endpoint", "https://prices.example.test/v1", "--model", "prices-model",
				"--auth", "bearer", "--credential-env", "EXACT_PRICES_KEY",
				"--retention-posture", "zero_retention", "--training-posture", "no_training",
				"--source", "conversation_text", "--source-since", "2025-01-01",
				"--accept-catalog-prices", "--yes")
			require.ErrorContains(err, test.wantError)
			assert.Equal(1, catalogCalls)
			assert.Zero(credentialReads)
			assert.Zero(negotiations)
			assert.Zero(writes)
		})
	}
}

func TestPersonProviderAddRejectsInvalidSnapshotCapsBeforeSecretOrProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, &fixedPersonProviderChecker{})
	// The file changes underneath the command after startup config loaded.
	content, err := os.ReadFile(path)
	require.NoError(err)
	content = bytes.Replace(content,
		[]byte("output_cost_microusd_per_million_tokens = 222\n"),
		[]byte("output_cost_microusd_per_million_tokens = 222\nmax_input_tokens_per_run = -1\n"), 1)
	require.NoError(os.WriteFile(path, content, 0o640))
	catalogCalls, credentialReads, negotiations, writes := 0, 0, 0, 0
	deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
		catalogCalls++
		return nil, nil
	}
	deps.setup.lookupEnv = func(string) (string, bool) {
		credentialReads++
		return providerSetupSecretCanary, true
	}
	deps.setup.negotiate = func(
		context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		negotiations++
		return peoplesweep.NegotiatedCapabilities{}, nil
	}
	nativeEdit := deps.editConfigTables
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		writes++
		return nativeEdit(etag, edits)
	}

	_, err = executePersonProviderCommand(t, deps,
		"add", "invalid-caps", "--protocol", "openai_chat",
		"--endpoint", "https://prices.example.test/v1", "--model", "prices-model",
		"--auth", "bearer", "--credential-env", "EXACT_PRICES_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
	require.ErrorContains(err, "max_input_tokens_per_run")
	assert.Zero(catalogCalls, "explicit transport fields need no catalog fetch")
	assert.Zero(credentialReads)
	assert.Zero(negotiations)
	assert.Zero(writes)
}

func TestPersonProviderSetUpdatesExistingProfile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)

	output, err := executePersonProviderCommand(t, deps,
		"set", "default", "--model", "updated-model", "--yes")
	require.NoError(err)
	assert.Contains(output, `Updated and checked people provider profile "default"`)

	reloaded, err := config.Load(path, "")
	require.NoError(err)
	provider := reloaded.People.Sweep.Providers["default"]
	assert.Equal("updated-model", provider.Model)
	assert.Equal("https://default.example.test/v1", provider.Endpoint)
	assert.Equal(peoplesweep.AuthBearer, provider.Auth)
	assert.Equal(peoplesweep.CredentialEnv, provider.Credential)
	assert.Equal("DEFAULT_KEY", provider.CredentialEnv)
}

func TestPersonProviderSetClosesStoreBeforeChecking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	st := testutil.NewSQLiteTestStore(t)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	deps.isDaemonSubprocess = func() bool { return false }
	deps.providerStoreOwnedByDaemon = func(context.Context) (bool, error) { return false, nil }
	deps.daemonAliveForRestartNotice = func(context.Context) (bool, error) { return false, nil }
	active := false
	opens := 0
	deps.openStore = func() (personProviderStore, func(), error) {
		opens++
		if active {
			return nil, nil, errors.New("people provider store lock is held")
		}
		active = true
		return st, func() { active = false }, nil
	}
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)

	_, err := executePersonProviderCommand(t, deps,
		"set", "default", "--model", "local-model", "--yes")
	require.NoError(err)
	assert.Equal(3, opens)
	assert.False(active)
}

func TestPersonProviderSetDoesNotRollbackAnUnverifiedConflictSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)
	before, err := config.ReadConfigFile(path)
	require.NoError(err)
	raced := before
	raced.Content = []byte("operator change")
	raced.ETag = "operator-change"
	reads := 0
	deps.readConfigFile = func() (config.ConfigFile, error) {
		reads++
		if reads == 1 {
			return before, nil
		}
		return raced, nil
	}
	deps.editConfigTables = func(string, []config.TableEdit) (config.ConfigFile, error) {
		return config.ConfigFile{}, config.ErrConfigConflict
	}
	restored := false
	deps.restoreConfigFile = func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error) {
		restored = true
		return config.ConfigFile{}, nil
	}

	_, err = executePersonProviderCommand(t, deps,
		"set", "default", "--model", "conflict-model", "--yes")
	require.ErrorIs(err, config.ErrConfigConflict)
	assert.Equal(1, reads)
	assert.False(restored)
}

func TestPersonProviderSetClearsOptionalPolicyFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	provider := loaded.People.Sweep.Providers["default"]
	provider.SourceUntil = "2025-12-31"
	provider.ReasoningEffort = "high"
	provider.ReasoningMode = "enabled"
	_, err = config.EditConfigTables(path, snapshot.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "default"},
		Values: personProviderTableValues(provider),
	}})
	require.NoError(err)
	loaded, err = config.Load(path, "")
	require.NoError(err)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)

	_, err = executePersonProviderCommand(t, deps, "set", "default",
		"--source-until", "", "--reasoning-effort", "", "--reasoning-mode", "", "--yes")
	require.NoError(err)
	reloaded, err := config.Load(path, "")
	require.NoError(err)
	updated := reloaded.People.Sweep.Providers["default"]
	assert.Empty(updated.SourceUntil)
	assert.Empty(updated.ReasoningEffort)
	assert.Empty(updated.ReasoningMode)
}

func TestPersonProviderSetRechecksAndRevokesOldConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	checker := newCheckedPersonProviderChecker()
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)

	oldConfig := loaded.People.Sweep
	oldConfig.Enabled = true
	oldProfile, err := oldConfig.Profile()
	require.NoError(err)
	st, cleanup, err := deps.openStore()
	require.NoError(err)
	t.Cleanup(cleanup)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), oldProfile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(
		t.Context(), oldProfile.Fingerprint, personProviderConsentActor)
	require.NoError(err)

	_, err = executePersonProviderCommand(t, deps,
		"set", "default", "--model", "rechecked-model", "--yes")
	require.NoError(err)
	reloaded, err := config.Load(path, "")
	require.NoError(err)
	newConfig := reloaded.People.Sweep
	newConfig.Enabled = true
	newProfile, err := newConfig.Profile()
	require.NoError(err)
	assert.NotEqual(oldProfile.Fingerprint, newProfile.Fingerprint)
	oldActive, err := st.HasActivePersonInferenceConsent(
		t.Context(), oldProfile.Fingerprint)
	require.NoError(err)
	assert.False(oldActive)
	newChecked, err := st.HasSuccessfulPersonInferenceCheck(
		t.Context(), newProfile.Fingerprint)
	require.NoError(err)
	assert.True(newChecked)
	newActive, err := st.HasActivePersonInferenceConsent(
		t.Context(), newProfile.Fingerprint)
	require.NoError(err)
	assert.False(newActive)
	assert.Equal(int64(1), checker.calls.Load())
}

func TestPersonProviderSetPreservesUnselectedProfileAndConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	sibling := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://sibling.example.test/v1",
		Model: "sibling-model", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "SIBLING_KEY",
		OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
		TokenLimitParameter: "max_completion_tokens", RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", AllowedSources: []peoplesweep.SourceClass{
			peoplesweep.SourceConversationText,
		}, SourceSince: "2025-01-01",
	}
	_, err = config.EditConfigTables(path, snapshot.ETag, []config.TableEdit{
		{Path: []string{"people", "sweep", "providers", "sibling"}, Values: personProviderTableValues(sibling)},
	})
	require.NoError(err)
	loaded, err = config.Load(path, "")
	require.NoError(err)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)
	t.Setenv("SIBLING_KEY", "sibling-secret")

	_, err = executePersonProviderCommand(t, deps,
		"set", "default", "--model", "preserved-model", "--yes")
	require.NoError(err)
	content, err := os.ReadFile(path)
	require.NoError(err)
	text := string(content)
	assert.Contains(text, "# retained operator comment")
	assert.Contains(text, "[future.operator_extension]")
	assert.Contains(text, "answer = 42")
	assert.Contains(text, "input_cost_microusd_per_million_tokens = 111 # operator price")
	assert.Contains(text, `provider = "default" # selector formatting must survive rollback`)
	assert.Contains(text, "credential_env = \"SIBLING_KEY\"")
	assert.Contains(text, "model = \"sibling-model\"")

	reloaded, err := config.Load(path, "")
	require.NoError(err)
	assert.Equal("default", reloaded.People.Sweep.Provider.Name)
	assert.False(reloaded.People.Sweep.Enabled)
	assert.Equal("preserved-model", reloaded.People.Sweep.Providers["default"].Model)
	assert.Equal("SIBLING_KEY", reloaded.People.Sweep.Providers["sibling"].CredentialEnv)
}

func TestPersonProviderSetRemote(t *testing.T) {
	assertAnError := assert.AnError
	assert := assert.New(t)
	require := require.New(t)
	calls := 0
	deps := personProviderCommandDeps{
		remoteConfigured: func() bool { return true },
		readConfigFile: func() (config.ConfigFile, error) {
			calls++
			return config.ConfigFile{}, assertAnError
		},
		editConfigTables: func(string, []config.TableEdit) (config.ConfigFile, error) {
			calls++
			return config.ConfigFile{}, assertAnError
		},
		restoreConfigFile: func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error) {
			calls++
			return config.ConfigFile{}, assertAnError
		},
		setup: personProviderSetupDeps{
			credentials: countingCredentialStore{calls: &calls},
			lookupEnv: func(string) (string, bool) {
				calls++
				return providerSetupSecretCanary, true
			},
		},
	}

	_, err := executePersonProviderCommand(t, deps,
		"set", "default", "--model", "remote-model", "--yes")
	require.Error(err)
	assert.Contains(err.Error(), "remote daemon")
	assert.Zero(calls)
}

func TestPersonProviderSetDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := providerSetupConfigFile(t)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	deps.isDaemonSubprocess = func() bool { return false }
	deps.providerStoreOwnedByDaemon = func(context.Context) (bool, error) { return true, nil }
	var proxied []string
	var checkFingerprint string
	deps.proxy = func(command *cobra.Command, _ []string, _ map[string]string) error {
		proxied = append(proxied, command.Use)
		if command.Use == "check" {
			checkFingerprint, _ = command.Flags().GetString(personProviderIfFingerprintFlag)
		}
		return nil
	}
	t.Setenv("DEFAULT_KEY", providerSetupSecretCanary)

	output, err := executePersonProviderCommand(t, deps,
		"set", "default", "--model", "daemon-model", "--yes")
	require.NoError(err)
	assert.Contains(output, "msgvault daemon restart")
	assert.Contains(output, "Updated and checked people provider profile")
	assert.Equal([]string{"revoke", "revoke", "check"}, proxied)
	assert.NotEmpty(checkFingerprint)
}

func providerSetupConfigFile(t *testing.T) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`# retained operator comment
[data]
data_dir = "`+filepath.ToSlash(filepath.Join(dir, "data"))+`"

[people.sweep]
enabled = false
provider = "default" # selector formatting must survive rollback

[people.sweep.providers.default]
protocol = "openai_chat"
endpoint = "https://default.example.test/v1"
model = "default-model"
auth = "bearer"
credential = "env"
credential_env = "DEFAULT_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"

[people.sweep.budgets]
input_cost_microusd_per_million_tokens = 111 # operator price
output_cost_microusd_per_million_tokens = 222

[future.operator_extension]
answer = 42
`), 0o640))
	loaded, err := config.Load(path, "")
	require.NoError(t, err)
	return path, loaded
}

// personProviderAddSelectionConfigFile writes an operator-owned config whose
// people sweep state is controlled by two dimensions: enabled controls
// scheduling, selected controls whether the file carries an explicit
// provider selector and named profile. An unselected config can only be
// valid while disabled, so selected=false forces enabled=false.
func personProviderAddSelectionConfigFile(
	t *testing.T,
	enabled bool,
	selected bool,
) (string, *config.Config) {
	t.Helper()
	if !selected {
		require.False(t, enabled, "an unselected people sweep config must stay disabled")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	var builder strings.Builder
	builder.WriteString("# retained operator comment\n[data]\ndata_dir = \"" +
		filepath.ToSlash(filepath.Join(dir, "data")) + "\"\n")
	if selected {
		fmt.Fprintf(&builder, "\n[people.sweep]\nenabled = %t\nprovider = \"default\"\n", enabled)
		builder.WriteString(`
[people.sweep.providers.default]
protocol = "openai_chat"
endpoint = "https://default.example.test/v1"
model = "default-model"
auth = "bearer"
credential = "env"
credential_env = "DEFAULT_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`)
	}
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o640))
	loaded, err := config.Load(path, "")
	require.NoError(t, err)
	return path, loaded
}

func addPersonProviderSelectionArgs(name string) []string {
	return []string{
		"add", name, "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://" + name + ".example.test/v1", "--model", name + "-model",
		"--auth", "bearer", "--credential-env", "EXACT_PROVIDER_KEY",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes",
	}
}

func newCheckedPersonProviderChecker() *fixedPersonProviderChecker {
	return &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "checked-model-v1",
	}}
}

func providerSetupCommandDeps(
	t *testing.T,
	path string,
	loaded *config.Config,
	checker personProviderChecker,
) personProviderCommandDeps {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(loaded.People.Sweep, st, checker)
	deps.config = func() peoplesweep.Config { return loaded.People.Sweep }
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup = personProviderSetupDeps{
		catalog: func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
			return nil, errors.New("catalog unavailable")
		},
		negotiate: func(_ context.Context, candidate peoplesweep.ProviderConfig, credential peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
			if credential.Scheme != peoplesweep.AuthNone {
				assert.Equal(t, providerSetupSecretCanary, credential.Value())
			}
			return peoplesweep.NegotiatedCapabilities{
				OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
				DriverVersion: peoplesweep.OpenAIChatProviderVersion,
			}, nil
		},
		credentials: peoplesweep.NewFileCredentialStore(loaded.TokensDir()),
		lookupEnv:   os.LookupEnv,
	}
	return deps
}

// TestPersonProviderDefaultDependenciesResolveCredentialsAfterConfigLoad
// reproduces the real command lifecycle: the provider command and its default
// dependencies are constructed during init, before PersistentPreRunE loads the
// selected config. Stored credentials must therefore resolve from the live
// config at execution time rather than from the init-time nil cfg.
func TestPersonProviderDefaultDependenciesResolveCredentialsAfterConfigLoad(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	previousConfig := cfg
	previousConfigFile := cfgFile
	previousHomeDir := homeDir
	previousLogger := logger
	previousLogResult := logResult
	t.Cleanup(func() {
		if logResult != nil && logResult != previousLogResult {
			logResult.Close()
		}
		cfg = previousConfig
		cfgFile = previousConfigFile
		homeDir = previousHomeDir
		logger = previousLogger
		logResult = previousLogResult
	})
	path, loaded := providerSetupConfigFile(t)
	cfg = nil
	cfgFile = path
	homeDir = ""
	logResult = nil
	deps := defaultPersonProviderCommandDeps()

	st := testutil.NewSQLiteTestStore(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "live-model-v1",
	}}
	deps.openStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }
	deps.openReadStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }
	deps.newChecker = func(peoplesweep.Config, personProviderStore) (personProviderChecker, error) {
		return checker, nil
	}
	deps.isDaemonSubprocess = func() bool { return true }
	deps.setup.catalog = nil
	deps.setup.negotiate = func(
		_ context.Context,
		_ peoplesweep.ProviderConfig,
		credential peoplesweep.Credential,
	) (peoplesweep.NegotiatedCapabilities, error) {
		assert := newAssert(t)
		assert.Equal(providerSetupSecretCanary, credential.Value())
		return peoplesweep.NegotiatedCapabilities{
			OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
			DriverVersion: peoplesweep.OpenAIChatProviderVersion,
		}, nil
	}

	root := &cobra.Command{Use: "msgvault", PersistentPreRunE: rootCmd.PersistentPreRunE}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonProviderCommand(deps))
	root.AddCommand(person)
	root.SetIn(strings.NewReader(providerSetupSecretCanary + "\n"))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		"person", "provider", "add", "live-stored", "--custom",
		"--protocol", "openai_chat", "--endpoint", "https://live.example.test/v1",
		"--model", "live-model", "--auth", "bearer", "--api-key-stdin",
		"--retention-posture", "zero_retention", "--training-posture", "no_training",
		"--source", "conversation_text", "--source-since", "2025-01-01", "--yes",
	})
	require.NoError(root.ExecuteContext(t.Context()))
	assert.NotContains(output.String(), providerSetupSecretCanary)

	credentialPath := filepath.Join(loaded.TokensDir(), "people-providers", "live-stored.json")
	credentialData, err := os.ReadFile(credentialPath)
	require.NoError(err)
	assert.Contains(string(credentialData), providerSetupSecretCanary)
	credentialInfo, err := os.Stat(credentialPath)
	require.NoError(err)
	assert.Equal(os.FileMode(0o600), credentialInfo.Mode().Perm())
	for _, directory := range []string{loaded.TokensDir(), filepath.Dir(credentialPath)} {
		info, statErr := os.Stat(directory)
		require.NoError(statErr)
		assert.Equal(os.FileMode(0o700), info.Mode().Perm())
	}
	configData, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(string(configData), providerSetupSecretCanary)
	assert.Contains(string(configData), `credential = "stored"`)

	removeRoot := &cobra.Command{Use: "msgvault", PersistentPreRunE: rootCmd.PersistentPreRunE}
	removePerson := &cobra.Command{Use: "person"}
	removePerson.AddCommand(newPersonProviderCommand(deps))
	removeRoot.AddCommand(removePerson)
	removeRoot.SetOut(&output)
	removeRoot.SetErr(&output)
	removeRoot.SetArgs([]string{"person", "provider", "remove", "live-stored"})
	require.NoError(removeRoot.ExecuteContext(t.Context()))
	assert.Contains(output.String(), `Removed people provider profile "live-stored"`)
	_, err = peoplesweep.NewFileCredentialStore(loaded.TokensDir()).Load("live-stored")
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
	tombstoneInfo, err := os.Stat(credentialPath)
	require.NoError(err)
	assert.Zero(tombstoneInfo.Size())
	assert.Equal(os.FileMode(0o600), tombstoneInfo.Mode().Perm())
}

// TestPersonProviderAddCustomStdinKeepsSecretLocal catches an add operation
// serializing a key into Cobra arguments, output, TOML, or config recovery
// artifacts instead of publishing it only to the private credential store.
func TestPersonProviderAddCustomStdinKeepsSecretLocal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "new-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)

	root := &bytes.Buffer{}
	root.WriteString(providerSetupSecretCanary + "\n")
	output, err := executePersonProviderCommandWithInput(t, deps, root,
		"add", "new-provider", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://new.example.test/v1", "--model", "new-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.NoError(err)
	assert.NotContains(output, providerSetupSecretCanary)
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(string(content), providerSetupSecretCanary)
	assert.Contains(string(content), `[people.sweep.providers.new-provider]`)
	// add must not switch the active selection: the operator's default
	// selector and its formatting survive untouched, and `person provider
	// use` owns selection and enablement.
	assert.Contains(string(content), `provider = "default" # selector formatting must survive rollback`)
	credential, err := peoplesweep.NewFileCredentialStore(loaded.TokensDir()).Load("new-provider")
	require.NoError(err)
	assert.Equal(providerSetupSecretCanary, credential.Value())
	recoveries, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".msgvault-config-recovery-*"))
	require.NoError(err)
	for _, recovery := range recoveries {
		recovered, readErr := os.ReadFile(recovery)
		require.NoError(readErr)
		assert.NotContains(string(recovered), providerSetupSecretCanary)
	}

	_, err = executePersonProviderCommand(t, deps, "add", "bad", "--api-key", providerSetupSecretCanary)
	require.Error(err)
	assert.NotContains(err.Error(), providerSetupSecretCanary)
}

// TestPersonProviderAddKeepsConsentedActiveProviderSelected pins the add
// boundary: publishing a new profile must not repoint an enabled scheduled
// sweep at a profile nobody consented to. The consented profile stays the
// active selection, stays enabled, and add prints no daemon restart advice
// because a running daemon's sweeps observe no selection change.
func TestPersonProviderAddKeepsConsentedActiveProviderSelected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := personProviderAddSelectionConfigFile(t, true, true)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)
	consented, err := loaded.People.Sweep.Profile()
	require.NoError(err)
	st, cleanup, err := deps.openStore()
	require.NoError(err)
	t.Cleanup(cleanup)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), consented)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(
		t.Context(), consented.Fingerprint, personProviderConsentActor)
	require.NoError(err)

	output, err := executePersonProviderCommand(t, deps,
		addPersonProviderSelectionArgs("new-provider")...)
	require.NoError(err)
	assert.Contains(output, "person provider use")
	assert.NotContains(output, "daemon restart")

	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(content), "[people.sweep.providers.new-provider]")
	assert.Contains(string(content), `provider = "default"`)
	assert.Contains(string(content), "enabled = true")
	assert.NotContains(string(content), `provider = "new-provider"`)

	reloaded, err := config.Load(path, "")
	require.NoError(err)
	assert.Equal("default", reloaded.People.Sweep.Provider.Name)
	assert.True(reloaded.People.Sweep.Enabled)
	assert.Contains(reloaded.People.Sweep.Providers, "new-provider")
	active, err := reloaded.People.Sweep.Profile()
	require.NoError(err)
	assert.Equal(consented.Fingerprint, active.Fingerprint)
	stillConsented, err := st.HasActivePersonInferenceConsent(
		t.Context(), consented.Fingerprint)
	require.NoError(err)
	assert.True(stillConsented)
}

// TestPersonProviderAddLeavesDisabledConfigDisabled covers add into configs
// whose sweep is disabled: an existing provider selection stays untouched,
// and a config without any selection stays unselected. The sweep itself
// stays disabled either way.
func TestPersonProviderAddLeavesDisabledConfigDisabled(t *testing.T) {
	t.Run("existing selection preserved", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		path, loaded := personProviderAddSelectionConfigFile(t, false, true)
		deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
		t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)

		_, err := executePersonProviderCommand(t, deps,
			addPersonProviderSelectionArgs("new-provider")...)
		require.NoError(err)

		content, err := os.ReadFile(path)
		require.NoError(err)
		assert.Contains(string(content), `provider = "default"`)
		assert.NotContains(string(content), `provider = "new-provider"`)
		assert.NotContains(string(content), "enabled = true")

		reloaded, err := config.Load(path, "")
		require.NoError(err)
		assert.Equal("default", reloaded.People.Sweep.Provider.Name)
		assert.False(reloaded.People.Sweep.Enabled)
		assert.Contains(reloaded.People.Sweep.Providers, "new-provider")
	})

	t.Run("first profile publishes no selector", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		path, loaded := personProviderAddSelectionConfigFile(t, false, false)
		deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
		t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)

		_, err := executePersonProviderCommand(t, deps,
			addPersonProviderSelectionArgs("first-provider")...)
		require.NoError(err)

		content, err := os.ReadFile(path)
		require.NoError(err)
		assert.NotContains(string(content), `provider = "first-provider"`)
		assert.NotContains(string(content), "enabled")

		reloaded, err := config.Load(path, "")
		require.NoError(err)
		assert.Empty(reloaded.People.Sweep.Provider.Name)
		assert.False(reloaded.People.Sweep.Enabled)
		assert.Contains(reloaded.People.Sweep.Providers, "first-provider")
	})
}

// TestPersonProviderUseStillSelectsAndEnablesAfterAdd pins the consent
// ordering end to end: add publishes and checks a profile without enabling
// sweeps, and only provider use, backed by that recorded check, switches the
// selection and enables the sweep with its daemon restart notice.
func TestPersonProviderUseStillSelectsAndEnablesAfterAdd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := personProviderAddSelectionConfigFile(t, false, true)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)
	_, err := executePersonProviderCommand(t, deps,
		addPersonProviderSelectionArgs("new-provider")...)
	require.NoError(err)

	disabled, err := config.Load(path, "")
	require.NoError(err)
	require.False(disabled.People.Sweep.Enabled)

	output, err := executePersonProviderCommand(t, deps, "use", "new-provider")
	require.NoError(err)
	assert.Contains(output, `Selected people provider profile "new-provider"`)
	assert.Contains(output, "msgvault daemon restart")

	reloaded, err := config.Load(path, "")
	require.NoError(err)
	assert.Equal("new-provider", reloaded.People.Sweep.Provider.Name)
	assert.True(reloaded.People.Sweep.Enabled)
}

// TestPersonProviderLifecycleJSONOutput pins the machine-readable contract an
// agent drives the lifecycle with: add reports the checked fingerprint, status
// exposes the recorded check next to consent, and use and remove report the
// resulting selection and daemon restart requirement.
func TestPersonProviderLifecycleJSONOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, loaded := personProviderAddSelectionConfigFile(t, false, true)
	deps := providerSetupCommandDeps(t, path, loaded, newCheckedPersonProviderChecker())
	t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)

	addRaw, err := executePersonProviderCommand(t, deps,
		append(addPersonProviderSelectionArgs("json-provider"), "--json")...)
	require.NoError(err)
	var added personProviderAddOutput
	require.NoError(json.Unmarshal([]byte(addRaw), &added), addRaw)
	assert.Equal("json-provider", added.Name)
	assert.True(added.Checked)
	assert.Len(added.Fingerprint, 64)

	// Each real CLI invocation loads config.toml fresh; mirror that here.
	published, err := config.Load(path, "")
	require.NoError(err)
	deps.config = func() peoplesweep.Config { return published.People.Sweep }

	statusRaw, err := executePersonProviderCommand(t, deps, "status", "json-provider", "--json")
	require.NoError(err)
	var status personProviderStatusOutput
	require.NoError(json.Unmarshal([]byte(statusRaw), &status), statusRaw)
	assert.Equal(added.Fingerprint, status.Profile.Fingerprint)
	require.NotNil(status.Check, "status must expose the recorded check")
	assert.Equal(added.Fingerprint, status.Check.ProfileFingerprint)
	assert.False(status.Consent.Active)

	useRaw, err := executePersonProviderCommand(t, deps, "use", "json-provider", "--json")
	require.NoError(err)
	var used personProviderUseOutput
	require.NoError(json.Unmarshal([]byte(useRaw), &used), useRaw)
	assert.Equal(personProviderUseOutput{
		Name: "json-provider", Fingerprint: added.Fingerprint, Enabled: true,
		DaemonRestartRequired: true,
	}, used)

	removeRaw, err := executePersonProviderCommand(t, deps, "remove", "default", "--json")
	require.NoError(err)
	var removed personProviderRemoveOutput
	require.NoError(json.Unmarshal([]byte(removeRaw), &removed), removeRaw)
	assert.Equal(personProviderRemoveOutput{
		Name: "default", Removed: true, DaemonRestartRequired: true,
	}, removed)
}

func executePersonProviderCommandWithInput(
	t *testing.T,
	deps personProviderCommandDeps,
	input io.Reader,
	args ...string,
) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonProviderCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "provider"}, args...))
	root.SetIn(input)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err := root.ExecuteContext(t.Context())
	return output.String(), err
}

// TestPersonProviderAddReadsOnlyExactEnvironmentOrMaskedTerminal catches setup
// falling back to a broader environment variable or echoing masked input.
func TestPersonProviderAddReadsOnlyExactEnvironmentOrMaskedTerminal(t *testing.T) {
	requireStoredCredentialStorePlatform(t)
	t.Run("exact environment", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		path, loaded := providerSetupConfigFile(t)
		checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
			Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
			ModelVersion: "env-model-v1",
		}}
		deps := providerSetupCommandDeps(t, path, loaded, checker)
		t.Setenv("EXACT_PROVIDER_KEY", providerSetupSecretCanary)
		t.Setenv("OPENAI_API_KEY", "broader-secret-must-not-be-read")
		var lookedUp []string
		deps.setup.lookupEnv = func(name string) (string, bool) {
			lookedUp = append(lookedUp, name)
			return os.LookupEnv(name)
		}

		output, err := executePersonProviderCommand(t, deps,
			"add", "env-provider", "--custom", "--protocol", "openai_chat",
			"--endpoint", "https://env.example.test/v1", "--model", "env-model",
			"--auth", "bearer", "--credential-env", "EXACT_PROVIDER_KEY",
			"--retention-posture", "zero_retention", "--training-posture", "no_training",
			"--source", "conversation_text", "--source-since", "2025-01-01", "--yes")
		require.NoError(err)
		assert.Equal([]string{"EXACT_PROVIDER_KEY"}, lookedUp)
		assert.NotContains(output, providerSetupSecretCanary)
		content, err := os.ReadFile(path)
		require.NoError(err)
		assert.Contains(string(content), `credential_env = "EXACT_PROVIDER_KEY"`)
		assert.NotContains(string(content), providerSetupSecretCanary)
		_, err = deps.setup.credentials.Load("env-provider")
		require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
	})

	t.Run("masked terminal", func(t *testing.T) {
		newAssert := assert.New
		assert := assert.New(t)
		require := require.New(t)
		path, loaded := providerSetupConfigFile(t)
		checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
			Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
			ModelVersion: "masked-model-v1",
		}}
		deps := providerSetupCommandDeps(t, path, loaded, checker)
		input, err := os.Open(os.DevNull)
		require.NoError(err)
		defer func() { _ = input.Close() }()
		deps.setup.isTerminal = func(uintptr) bool { return true }
		deps.setup.readMasked = func(file *os.File, limit int) ([]byte, error) {
			assert := newAssert(t)
			assert.Same(input, file)
			assert.Equal(maxProviderCredentialBytes, limit)
			return []byte(providerSetupSecretCanary), nil
		}

		output, err := executePersonProviderCommandWithInput(t, deps, input,
			"add", "masked-provider", "--custom", "--protocol", "openai_chat",
			"--endpoint", "https://masked.example.test/v1", "--model", "masked-model",
			"--auth", "x_api_key", "--retention-posture", "zero_retention",
			"--training-posture", "no_training", "--source", "conversation_text",
			"--source-since", "2025-01-01", "--yes")
		require.NoError(err)
		assert.NotContains(output, providerSetupSecretCanary)
		credential, err := deps.setup.credentials.Load("masked-provider")
		require.NoError(err)
		assert.Equal(peoplesweep.AuthXAPIKey, credential.Scheme)
		assert.Equal(providerSetupSecretCanary, credential.Value())
	})
}

// TestPersonProviderCatalogPricesRequireExplicitAcceptance catches mutable
// catalog data silently refreshing configured budget prices.
func TestPersonProviderCatalogPricesRequireExplicitAcceptance(t *testing.T) {
	for _, test := range []struct {
		name       string
		accept     bool
		wantInput  string
		wantOutput string
	}{
		{name: "rejected", wantInput: "111", wantOutput: "222"},
		{name: "accepted", accept: true, wantInput: "700000", wantOutput: "900000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path, loaded := providerSetupConfigFile(t)
			checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
				Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
				ModelVersion: "catalog-model-v1",
			}}
			deps := providerSetupCommandDeps(t, path, loaded, checker)
			inputPrice, outputPrice := int64(700000), int64(900000)
			deps.setup.catalog = func(context.Context) ([]peoplesweep.ProviderSuggestion, error) {
				return []peoplesweep.ProviderSuggestion{{
					ID: "hint-provider", Name: "Hint Provider", Endpoint: "https://catalog.example.test/v1",
					Models: []peoplesweep.ModelSuggestion{{
						ID: "catalog-model", Name: "Catalog Model",
						InputCostMicroUSDPerMillionTokens:  &inputPrice,
						OutputCostMicroUSDPerMillionTokens: &outputPrice,
					}},
				}}, nil
			}
			args := []string{
				"add", "catalog-provider", "--protocol", "openai_chat",
				"--endpoint", "https://catalog.example.test/v1", "--model", "catalog-model",
				"--auth", "none", "--retention-posture", "local_only",
				"--training-posture", "local_only", "--source", "conversation_text",
				"--source-since", "2025-01-01", "--yes",
			}
			if test.accept {
				args = append(args, "--accept-catalog-prices")
			}
			_, err := executePersonProviderCommand(t, deps, args...)
			require.ErrorContains(err, "loopback")
			// Use authenticated HTTPS after proving catalog labels never imply
			// anonymous policy or alter the explicit values.
			args = append(args, "--credential-env", "CATALOG_KEY")
			t.Setenv("CATALOG_KEY", providerSetupSecretCanary)
			args[9] = "bearer"
			_, err = executePersonProviderCommand(t, deps, args...)
			require.NoError(err)
			content, err := os.ReadFile(path)
			require.NoError(err)
			assert.Contains(string(content),
				"input_cost_microusd_per_million_tokens = "+test.wantInput+" # operator price")
			assert.Contains(string(content),
				"output_cost_microusd_per_million_tokens = "+test.wantOutput)
		})
	}
}

// TestPersonProviderRemoveRevokesAndDeletesOnlyExactCredential catches profile
// removal deleting audit history, a sibling credential, or the active enabled
// selector.
func TestPersonProviderRemoveRevokesAndDeletesOnlyExactCredential(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	oldProvider := configuredPersonProvider(loaded.People.Sweep)
	oldProvider.Endpoint = "https://old.example.test/v1"
	oldProvider.Model = "old-model"
	oldProvider.Credential = peoplesweep.CredentialStored
	oldProvider.CredentialEnv = ""
	after, err := config.EditConfigTables(path, snapshot.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(oldProvider),
	}})
	require.NoError(err)
	loaded, err = config.LoadConfigFile(after, "")
	require.NoError(err)
	credentialStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	require.NoError(credentialStore.Save("old", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary)))
	require.NoError(credentialStore.Save("sibling", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, "sibling-secret")))
	st := testutil.NewSQLiteTestStore(t)
	selected := loaded.People.Sweep
	selected.Enabled = true
	selected.Provider = peoplesweep.ProviderSelection{Name: "old"}
	profile, err := selected.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	deps := localPersonProviderDeps(loaded.People.Sweep, st, nil)
	deps.config = func() peoplesweep.Config {
		require := newRequire(t)
		current, loadErr := config.Load(path, "")
		require.NoError(loadErr)
		return current.People.Sweep
	}
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup.credentials = credentialStore

	output, err := executePersonProviderCommand(t, deps, "remove", "old")
	require.NoError(err)
	assert.Contains(output, "old")
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(string(content), "[people.sweep.providers.old]")
	_, err = credentialStore.Load("old")
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
	sibling, err := credentialStore.Load("sibling")
	require.NoError(err)
	assert.Equal("sibling-secret", sibling.Value())
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(active)
	profiles, err := st.ListPersonInferenceProfiles(t.Context())
	require.NoError(err)
	assert.Len(profiles, 1, "immutable audit profile must remain")

	activePath, activeLoaded := providerSetupConfigFile(t)
	activeSnapshot, err := config.ReadConfigFile(activePath)
	require.NoError(err)
	_, err = config.EditConfigFile(activePath, activeSnapshot.ETag, []config.Edit{{
		Key: "people.sweep.enabled", Value: true,
	}})
	require.NoError(err)
	activeDeps := localPersonProviderDeps(activeLoaded.People.Sweep, st, nil)
	activeDeps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(activePath) }
	activeDeps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(activePath, etag, edits)
	}
	activeDeps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(activePath, published, before)
	}
	_, err = executePersonProviderCommand(t, activeDeps, "remove", "default")
	require.ErrorContains(err, "active")
}

func TestPersonProviderRemoveUsesOneFreshConfigSnapshotForAllSideEffects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	initial, err := config.ReadConfigFile(path)
	require.NoError(err)
	old := configuredPersonProvider(loaded.People.Sweep)
	old.Endpoint = "https://old-snapshot.example.test/v1"
	old.Model = "startup-old-model"
	old.Credential = peoplesweep.CredentialStored
	old.CredentialEnv = ""
	withOld, err := config.EditConfigTables(path, initial.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(old), InsertOnly: true,
	}})
	require.NoError(err)
	startupLoaded, err := config.LoadConfigFile(withOld, "")
	require.NoError(err)
	startup := startupLoaded.People.Sweep
	credentialStore := peoplesweep.NewFileCredentialStore(startupLoaded.TokensDir())
	require.NoError(credentialStore.Save("old", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary)))
	staleSelection := startup
	staleSelection.Enabled = true
	staleSelection.Provider = peoplesweep.ProviderSelection{Name: "old"}
	staleProfile, err := staleSelection.Profile()
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), staleProfile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), staleProfile.Fingerprint, "cli")
	require.NoError(err)

	current := old
	current.Model = "operator-current-model"
	current.Credential = peoplesweep.CredentialEnv
	current.CredentialEnv = "OPERATOR_CURRENT_KEY"
	currentSnapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	_, err = config.EditConfigTables(path, currentSnapshot.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(current),
	}})
	require.NoError(err)

	deps := localPersonProviderDeps(startup, st, nil)
	deps.config = func() peoplesweep.Config { return startup }
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup.credentials = credentialStore

	_, err = executePersonProviderCommand(t, deps, "remove", "old")
	require.NoError(err)
	stillActive, err := st.HasActivePersonInferenceConsent(t.Context(), staleProfile.Fingerprint)
	require.NoError(err)
	assert.True(stillActive, "stale startup fingerprint must not be revoked")
	credential, err := credentialStore.Load("old")
	require.NoError(err)
	assert.Equal(providerSetupSecretCanary, credential.Value())
	finalConfig, err := config.Load(path, "")
	require.NoError(err)
	_, exists := finalConfig.People.Sweep.Providers["old"]
	assert.False(exists)
}

func TestPersonProviderRemoveConfigConflictHasNoConsentOrCredentialSideEffects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	initial, err := config.ReadConfigFile(path)
	require.NoError(err)
	old := configuredPersonProvider(loaded.People.Sweep)
	old.Model = "conflict-old-model"
	old.Credential = peoplesweep.CredentialStored
	old.CredentialEnv = ""
	withOld, err := config.EditConfigTables(path, initial.ETag, []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Values: personProviderTableValues(old), InsertOnly: true,
	}})
	require.NoError(err)
	current, err := config.LoadConfigFile(withOld, "")
	require.NoError(err)
	credentialStore := peoplesweep.NewFileCredentialStore(current.TokensDir())
	require.NoError(credentialStore.Save("old", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary)))
	selected := current.People.Sweep
	selected.Enabled = true
	selected.Provider = peoplesweep.ProviderSelection{Name: "old"}
	profile, err := selected.Profile()
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)

	deps := localPersonProviderDeps(current.People.Sweep, st, nil)
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		operator, readErr := config.ReadConfigFile(path)
		if readErr != nil {
			return config.ConfigFile{}, readErr
		}
		if _, editErr := config.EditConfigFile(path, operator.ETag, []config.Edit{{
			Key: "web.theme", Value: "dark",
		}}); editErr != nil {
			return config.ConfigFile{}, editErr
		}
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup.credentials = credentialStore

	_, err = executePersonProviderCommand(t, deps, "remove", "old")
	require.ErrorIs(err, config.ErrConfigConflict)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(active)
	credential, err := credentialStore.Load("old")
	require.NoError(err)
	assert.Equal(providerSetupSecretCanary, credential.Value())
	finalConfig, err := config.Load(path, "")
	require.NoError(err)
	assert.Equal("conflict-old-model", finalConfig.People.Sweep.Providers["old"].Model)
	assert.Equal("dark", finalConfig.Web.Theme)
}

// TestPersonProviderAddRollsBackExactConfigAndNewCredential catches a failed
// final saved-profile check leaving a selector/profile or deleting an existing
// sibling credential during rollback.
func TestPersonProviderAddRollsBackExactConfigAndNewCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	before, err := os.ReadFile(path)
	require.NoError(err)
	credentialStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	require.NoError(credentialStore.Save("sibling", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, "sibling-secret")))
	checker := &fixedPersonProviderChecker{err: errors.New("synthetic check failed")}
	deps := providerSetupCommandDeps(t, path, loaded, checker)

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "will-rollback", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://rollback.example.test/v1", "--model", "rollback-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(err, "synthetic check failed")
	after, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(before, after)
	_, err = credentialStore.Load("will-rollback")
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
	sibling, err := credentialStore.Load("sibling")
	require.NoError(err)
	assert.Equal("sibling-secret", sibling.Value())
	info, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(fs.FileMode(0o640), info.Mode().Perm())
}

type replacingSaveNewCredentialStore struct {
	*peoplesweep.FileCredentialStore

	afterSaveNew func()
}

func (s *replacingSaveNewCredentialStore) SaveNew(
	profileName string,
	credential peoplesweep.Credential,
) (peoplesweep.CredentialCleanupGuard, bool, error) {
	guard, created, err := s.FileCredentialStore.SaveNew(profileName, credential)
	if err == nil && created && s.afterSaveNew != nil {
		s.afterSaveNew()
	}
	return guard, created, err
}

type callbackPersonProviderChecker func(context.Context) (peoplesweep.StructuredResponse, error)

func (check callbackPersonProviderChecker) Check(
	ctx context.Context,
) (peoplesweep.StructuredResponse, error) {
	return check(ctx)
}

func replaceNewPersonProviderCredential(
	t *testing.T,
	store *peoplesweep.FileCredentialStore,
	tokensDir, profileName string,
) (string, string, []byte, []byte) {
	t.Helper()
	credentialPath := filepath.Join(tokensDir, "people-providers", profileName+".json")
	retainedPath := credentialPath + ".retained"
	require.NoError(t, os.Rename(credentialPath, retainedPath))
	require.NoError(t, store.Save(profileName, peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary+"-replacement",
	)))
	original, err := os.ReadFile(retainedPath)
	require.NoError(t, err)
	replacement, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	return retainedPath, credentialPath, original, replacement
}

func assertNewPersonProviderCredentialReplacementUntouched(
	t *testing.T,
	retainedPath, credentialPath string,
	wantOriginal, wantReplacement []byte,
) {
	t.Helper()
	gotOriginal, err := os.ReadFile(retainedPath)
	require.NoError(t, err)
	gotReplacement, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	assert.Equal(t, wantOriginal, gotOriginal)
	assert.Equal(t, wantReplacement, gotReplacement)
}

func TestPersonProviderAddConfigFailureRejectsReplacementCredentialCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	before, err := os.ReadFile(path)
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	baseStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	var retainedPath, credentialPath string
	var original, replacement []byte
	credentialStore := &replacingSaveNewCredentialStore{FileCredentialStore: baseStore}
	credentialStore.afterSaveNew = func() {
		retainedPath, credentialPath, original, replacement = replaceNewPersonProviderCredential(
			t, baseStore, loaded.TokensDir(), "config-race",
		)
	}
	deps := providerSetupCommandDeps(t, path, loaded, &fixedPersonProviderChecker{})
	deps.setup.credentials = credentialStore
	deps.openStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }
	deps.editConfigTables = func(string, []config.TableEdit) (config.ConfigFile, error) {
		return config.ConfigFile{}, errors.New("injected config publication failure")
	}

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "config-race", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://config-race.example.test/v1", "--model", "config-race-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(err, "injected config publication failure")
	require.ErrorContains(err, "credential cleanup conflict")
	after, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(before, after)
	assertNewPersonProviderCredentialReplacementUntouched(
		t, retainedPath, credentialPath, original, replacement,
	)
	credential, loadErr := baseStore.Load("config-race")
	require.NoError(loadErr)
	assert.Equal(providerSetupSecretCanary+"-replacement", credential.Value())
	var consentCount int
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_inference_consents`).Scan(&consentCount))
	assert.Zero(consentCount)
}

func TestPersonProviderAddFinalCheckFailureRejectsReplacementCredentialCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	before, err := os.ReadFile(path)
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	credentialStore := peoplesweep.NewFileCredentialStore(loaded.TokensDir())
	var retainedPath, credentialPath string
	var original, replacement []byte
	checker := callbackPersonProviderChecker(func(context.Context) (peoplesweep.StructuredResponse, error) {
		retainedPath, credentialPath, original, replacement = replaceNewPersonProviderCredential(
			t, credentialStore, loaded.TokensDir(), "check-race",
		)
		return peoplesweep.StructuredResponse{}, errors.New("synthetic final check failed")
	})
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	deps.setup.credentials = credentialStore
	deps.openStore = func() (personProviderStore, func(), error) { return st, func() {}, nil }

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "check-race", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://check-race.example.test/v1", "--model", "check-race-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(err, "synthetic final check failed")
	require.ErrorContains(err, "credential cleanup conflict")
	after, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(before, after)
	assertNewPersonProviderCredentialReplacementUntouched(
		t, retainedPath, credentialPath, original, replacement,
	)
	credential, loadErr := credentialStore.Load("check-race")
	require.NoError(loadErr)
	assert.Equal(providerSetupSecretCanary+"-replacement", credential.Value())
	var consentCount int
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_inference_consents`).Scan(&consentCount))
	assert.Zero(consentCount)
}

func TestPersonProviderAddConfigConflictKeepsConcurrentEditAndDeletesOnlyNewCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "conflict-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	editTables := deps.editConfigTables
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		current, err := config.ReadConfigFile(path)
		if err != nil {
			return config.ConfigFile{}, err
		}
		if _, err := config.EditConfigFile(path, current.ETag, []config.Edit{{
			Key: "web.theme", Value: "dark",
		}}); err != nil {
			return config.ConfigFile{}, err
		}
		return editTables(etag, edits)
	}

	output, err := executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "conflicted", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://conflict.example.test/v1", "--model", "conflict-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorIs(err, config.ErrConfigConflict)
	assert.NotContains(err.Error(), providerSetupSecretCanary)
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(content), `theme = "dark"`)
	assert.NotContains(string(content), "providers.conflicted")
	assert.NotContains(string(content), providerSetupSecretCanary)
	assert.NotContains(output, providerSetupSecretCanary)
	_, err = deps.setup.credentials.Load("conflicted")
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}

func TestPersonProviderAddRollsBackExactUncertainPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	beforeBytes, err := os.ReadFile(path)
	require.NoError(err)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "uncertain-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		after, editErr := config.EditConfigTables(path, etag, edits)
		if editErr != nil {
			return after, editErr
		}
		return after, errors.Join(config.ErrConfigChanged, errors.New("injected cleanup uncertainty"))
	}

	_, err = executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "uncertain", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://uncertain.example.test/v1", "--model", "uncertain-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorIs(err, config.ErrConfigChanged)
	afterBytes, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(beforeBytes, afterBytes)
	_, err = deps.setup.credentials.Load("uncertain")
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}

func TestPersonProviderAddFailedCheckRestoresOriginallyMissingConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	startup := config.NewDefaultConfig()
	startup.HomeDir = dir
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(startup.People.Sweep, st,
		&fixedPersonProviderChecker{err: errors.New("synthetic final check failed")})
	deps.config = func() peoplesweep.Config { return startup.People.Sweep }
	deps.configHomeDir = func() string { return dir }
	deps.readConfigFile = func() (config.ConfigFile, error) { return config.ReadConfigFile(path) }
	deps.editConfigTables = func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		return config.EditConfigTables(path, etag, edits)
	}
	deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
		return config.RestoreConfigFile(path, published, before)
	}
	deps.setup = personProviderSetupDeps{
		negotiate: func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error) {
			return peoplesweep.NegotiatedCapabilities{
				OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
				DriverVersion: peoplesweep.OpenAIChatProviderVersion,
			}, nil
		},
		credentials: peoplesweep.NewFileCredentialStore(filepath.Join(dir, "tokens")),
	}

	_, err := executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "first", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://first.example.test/v1", "--model", "first-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(err, "synthetic final check failed")
	_, statErr := os.Stat(path)
	require.ErrorIs(statErr, fs.ErrNotExist)
	_, credentialErr := deps.setup.credentials.Load("first")
	require.ErrorIs(credentialErr, peoplesweep.ErrCredentialNotFound)
	recoveries, globErr := filepath.Glob(filepath.Join(dir, ".config-retired-*"))
	require.NoError(globErr)
	require.Len(recoveries, 1)
	recovered, readErr := os.ReadFile(recoveries[0])
	require.NoError(readErr)
	assert.NotContains(string(recovered), providerSetupSecretCanary)
}

func TestPersonProviderConcurrentExactAddNeverReadsSecretOrOverwrites(t *testing.T) {
	requireStoredCredentialStorePlatform(t)
	tests := []struct {
		name       string
		auth       string
		endpoint   string
		credential []string
	}{
		{name: "stored", auth: "bearer", endpoint: "https://candidate.example.test/v1",
			credential: []string{"--api-key-stdin"}},
		{name: "environment", auth: "bearer", endpoint: "https://candidate.example.test/v1",
			credential: []string{"--credential-env", "EXACT_RACE_KEY"}},
		{name: "anonymous", auth: "none", endpoint: "http://127.0.0.1:11434/v1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path, loaded := providerSetupConfigFile(t)
			deps := providerSetupCommandDeps(t, path, loaded, &fixedPersonProviderChecker{})
			startup := loaded.People.Sweep
			deps.config = func() peoplesweep.Config { return startup }
			concurrent := configuredPersonProvider(startup)
			concurrent.Model = "operator-raced-model"
			concurrent.Credential = peoplesweep.CredentialEnv
			concurrent.CredentialEnv = "OPERATOR_RACED_KEY"
			installed := false
			deps.readConfigFile = func() (config.ConfigFile, error) {
				if !installed {
					installed = true
					before, err := config.ReadConfigFile(path)
					if err != nil {
						return config.ConfigFile{}, err
					}
					if _, err := config.EditConfigTables(path, before.ETag, []config.TableEdit{{
						Path:   []string{"people", "sweep", "providers", "raced"},
						Values: personProviderTableValues(concurrent), InsertOnly: true,
					}}); err != nil {
						return config.ConfigFile{}, err
					}
				}
				return config.ReadConfigFile(path)
			}
			lookups := 0
			deps.setup.lookupEnv = func(string) (string, bool) {
				lookups++
				return providerSetupSecretCanary, true
			}
			negotiations := 0
			deps.setup.negotiate = func(
				context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential,
			) (peoplesweep.NegotiatedCapabilities, error) {
				negotiations++
				return peoplesweep.NegotiatedCapabilities{
					OutputMode: peoplesweep.OutputModeJSONObject, TokenLimitParameter: "max_tokens",
					DriverVersion: peoplesweep.OpenAIChatProviderVersion,
				}, nil
			}
			input := &countingProviderReader{data: bytes.NewReader([]byte(providerSetupSecretCanary + "\n"))}
			args := []string{
				"add", "raced", "--custom", "--protocol", "openai_chat", "--endpoint", test.endpoint,
				"--model", "candidate-model", "--auth", test.auth,
				"--retention-posture", "local_only", "--training-posture", "local_only",
				"--source", "conversation_text", "--source-since", "2025-01-01", "--yes",
			}
			args = append(args, test.credential...)

			_, err := executePersonProviderCommandWithInput(t, deps, input, args...)
			require.ErrorContains(err, "already exists")
			assert.Zero(input.reads)
			assert.Zero(lookups)
			assert.Zero(negotiations)
			current, loadErr := config.Load(path, "")
			require.NoError(loadErr)
			assert.Equal("operator-raced-model", current.People.Sweep.Providers["raced"].Model)
			_, credentialErr := deps.setup.credentials.Load("raced")
			require.ErrorIs(credentialErr, peoplesweep.ErrCredentialNotFound)
		})
	}
}

func TestPersonProviderAddRefusesToOverwriteExactCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	path, loaded := providerSetupConfigFile(t)
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAIChatProviderVersion,
		ModelVersion: "occupied-model-v1",
	}}
	deps := providerSetupCommandDeps(t, path, loaded, checker)
	require.NoError(deps.setup.credentials.Save("occupied", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, "preexisting-credential")))

	output, err := executePersonProviderCommandWithInput(t, deps,
		bytes.NewBufferString(providerSetupSecretCanary+"\n"),
		"add", "occupied", "--custom", "--protocol", "openai_chat",
		"--endpoint", "https://occupied.example.test/v1", "--model", "occupied-model",
		"--auth", "bearer", "--api-key-stdin", "--retention-posture", "zero_retention",
		"--training-posture", "no_training", "--source", "conversation_text",
		"--source-since", "2025-01-01", "--yes")
	require.ErrorContains(err, "already exists")
	assert.NotContains(err.Error(), providerSetupSecretCanary)
	credential, err := deps.setup.credentials.Load("occupied")
	require.NoError(err)
	assert.Equal("preexisting-credential", credential.Value())
	assert.NotContains(output, providerSetupSecretCanary)
}
