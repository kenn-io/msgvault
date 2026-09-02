package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/testutil"
)

type deleteCountingCredentialStore struct {
	peoplesweep.CredentialStore

	deletes int
}

func (s *deleteCountingCredentialStore) Delete(
	profileName string,
	guard peoplesweep.CredentialDeleteGuard,
) error {
	s.deletes++
	return s.CredentialStore.Delete(profileName, guard)
}

type postPreflightRaceCredentialStore struct {
	peoplesweep.CredentialStore

	beforeDelete func() error
}

func (s *postPreflightRaceCredentialStore) Delete(
	profileName string,
	guard peoplesweep.CredentialDeleteGuard,
) error {
	if s.beforeDelete != nil {
		if err := s.beforeDelete(); err != nil {
			return err
		}
		s.beforeDelete = nil
	}
	return s.CredentialStore.Delete(profileName, guard)
}

func TestPersonProviderFrontendRoutesExactCommandsAndCredential(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgs []string
	}{
		{name: "status", args: []string{"status", "--json"},
			wantArgs: []string{"person", "provider", "status", "--json"}},
		{name: "consent", args: []string{"consent", "--yes"},
			wantArgs: []string{"person", "provider", "consent", "--yes"}},
		{name: "revoke", args: []string{"revoke"},
			wantArgs: []string{"person", "provider", "revoke"}},
		{name: "list", args: []string{"list", "--json"},
			wantArgs: []string{"person", "provider", "list", "--json"}},
		{name: "history", args: []string{"history", "default", "--json"},
			wantArgs: []string{"person", "provider", "history", "--json", "default"}},
		{name: "semantic consent", args: []string{"consent", "--semantic-embeddings", "--yes"},
			wantArgs: []string{"person", "provider", "consent", "--semantic-embeddings", "--yes"}},
		{name: "check", args: []string{"check", "--json"},
			wantArgs: []string{"person", "provider", "check", "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			var gotArgs []string
			var gotEnv map[string]string
			lookups := 0
			deps := personProviderCommandDeps{
				config:             personProviderTestConfig,
				isDaemonSubprocess: func() bool { return false },
				lookupEnv: func(name string) (string, bool) {
					lookups++
					assert.Equal("TEST_PROVIDER_KEY", name)
					return "caller-secret-canary", true
				},
				proxy: func(command *cobra.Command, args []string, env map[string]string) error {
					var err error
					gotArgs, err = daemonCLIArgsFromCobra(command, args)
					require.NoError(t, err)
					gotEnv = env
					return nil
				},
			}

			_, err := executePersonProviderCommand(t, deps, test.args...)
			require.NoError(t, err)
			assert.Equal(test.wantArgs, gotArgs)
			assert.Nil(gotEnv)
			assert.Zero(lookups)
		})
	}
}

func TestPersonProviderFrontendNamedCheckSendsOnlyProfileName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := personProviderTestConfig()
	secondary := configuredPersonProvider(config)
	secondary.Model = "secondary-model"
	secondary.CredentialEnv = "SECONDARY_PROVIDER_KEY"
	config.Providers["secondary"] = secondary

	var gotArgs []string
	var gotEnv map[string]string
	var lookups []string
	deps := personProviderCommandDeps{
		config:             func() peoplesweep.Config { return config },
		isDaemonSubprocess: func() bool { return false },
		lookupEnv: func(name string) (string, bool) {
			lookups = append(lookups, name)
			return map[string]string{
				"TEST_PROVIDER_KEY":      "active-key-canary",
				"SECONDARY_PROVIDER_KEY": "selected-key-canary",
			}[name], true
		},
		proxy: func(command *cobra.Command, args []string, env map[string]string) error {
			var err error
			gotArgs, err = daemonCLIArgsFromCobra(command, args)
			gotEnv = env
			return err
		},
	}

	_, err := executePersonProviderCommand(t, deps, "check", "secondary", "--json")
	require.NoError(err)
	assert.Equal([]string{"person", "provider", "check", "--json", "secondary"}, gotArgs)
	assert.Empty(lookups)
	assert.Nil(gotEnv)
}

func TestPersonProviderLoginAndModelsNeverProxy(t *testing.T) {
	for _, operation := range []string{"login", "models"} {
		t.Run(operation, func(t *testing.T) {
			proxied := false
			deps := personProviderCommandDeps{
				config:             personProviderTestConfig,
				isDaemonSubprocess: func() bool { return false },
				proxy: func(*cobra.Command, []string, map[string]string) error {
					proxied = true
					return nil
				},
				newCodexClient: func(peoplesweep.Config) (personProviderCodexClient, error) {
					return nil, assert.AnError
				},
			}

			_, err := executePersonProviderCommand(t, deps, operation)
			require.Error(t, err)
			assert.False(t, proxied)
		})
	}
}

func TestPersonProviderFrontendRemoveProxiesOnlyNamedRevoke(t *testing.T) {
	assertAnError := assert.AnError
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	configured := personProviderTestConfig()
	beta := configuredPersonProvider(configured)
	beta.Model = "beta-model"
	configured.Providers["beta"] = beta
	path, _ := retainedPersonProviderTestConfig(t, configured)
	selected := configured
	selected.Provider = peoplesweep.ProviderSelection{Name: "beta"}
	profile, err := selected.Profile()
	require.NoError(err)
	var gotArgs []string
	var events []string
	deps := personProviderCommandDeps{
		config:             func() peoplesweep.Config { return configured },
		isDaemonSubprocess: func() bool { return false },
		proxy: func(command *cobra.Command, args []string, _ map[string]string) error {
			events = append(events, "revoke")
			var err error
			gotArgs, err = daemonCLIArgsFromCobra(command, args)
			return err
		},
		openStore: func() (personProviderStore, func(), error) {
			require := newRequire(t)
			require.FailNow("frontend remove must revoke through the daemon owner")
			return nil, nil, assertAnError
		},
		readConfigFile: func() (config.ConfigFile, error) {
			return config.ReadConfigFile(path)
		},
		editConfigTables: func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
			events = append(events, "edit")
			return config.EditConfigTables(path, etag, edits)
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			return config.RestoreConfigFile(path, published, before)
		},
		configHomeDir: func() string { return filepath.Dir(path) },
	}

	_, err = executePersonProviderCommand(t, deps, "remove", "beta")
	require.NoError(err)
	assert.Equal([]string{
		"person", "provider", "revoke", "--if-fingerprint=" + profile.Fingerprint, "beta",
	}, gotArgs)
	assert.Equal([]string{"revoke", "edit"}, events)
}

func TestPersonProviderRemoveCompletesLocalPreflightBeforeRevoke(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured peoplesweep.Config
		mutateFile func(t *testing.T, path string)
		wantError  string
	}{
		{
			name:       "only selected profile",
			configured: func() peoplesweep.Config { c := personProviderTestConfig(); c.Enabled = false; return c }(),
			wantError:  "only configured",
		},
		{
			name: "descendant extension table",
			configured: func() peoplesweep.Config {
				c := personProviderTestConfig()
				c.Enabled = false
				beta := configuredPersonProvider(c)
				beta.Model = "beta-model"
				c.Providers["beta"] = beta
				c.Provider = peoplesweep.ProviderSelection{Name: "beta"}
				return c
			}(),
			mutateFile: func(t *testing.T, path string) {
				t.Helper()
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				require.NoError(t, err)
				_, writeErr := file.WriteString("\n[people.sweep.providers.beta.operator_extension]\nanswer = 42\n")
				require.NoError(t, errors.Join(writeErr, file.Close()))
			},
			wantError: "descendant content",
		},
		{
			name: "dotted descendant extension",
			configured: func() peoplesweep.Config {
				c := personProviderTestConfig()
				c.Enabled = false
				beta := configuredPersonProvider(c)
				beta.Model = "beta-model"
				c.Providers["beta"] = beta
				c.Provider = peoplesweep.ProviderSelection{Name: "beta"}
				return c
			}(),
			mutateFile: func(t *testing.T, path string) {
				t.Helper()
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				header := []byte("[people.sweep.providers.beta]\n")
				withExtension := append(append([]byte(nil), header...), []byte("operator_extension.answer = 42\n")...)
				content = bytes.Replace(content, header, withExtension, 1)
				require.NoError(t, os.WriteFile(path, content, 0o600))
			},
			wantError: "descendant content",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, _ := retainedPersonProviderTestConfig(t, test.configured)
			if test.mutateFile != nil {
				test.mutateFile(t, path)
			}
			revokes := 0
			edits := 0
			deps := personProviderCommandDeps{
				config:                     func() peoplesweep.Config { return test.configured },
				isDaemonSubprocess:         func() bool { return false },
				providerStoreOwnedByDaemon: func(context.Context) (bool, error) { return true, nil },
				proxy: func(*cobra.Command, []string, map[string]string) error {
					revokes++
					return nil
				},
				readConfigFile: func() (config.ConfigFile, error) { return config.ReadConfigFile(path) },
				editConfigTables: func(etag string, planned []config.TableEdit) (config.ConfigFile, error) {
					edits++
					return config.EditConfigTables(path, etag, planned)
				},
				restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
					return config.RestoreConfigFile(path, published, before)
				},
			}

			_, err := executePersonProviderCommand(t, deps, "remove", test.configured.Provider.Name)
			require.ErrorContains(t, err, test.wantError)
			assert.Zero(t, revokes)
			assert.Zero(t, edits)
		})
	}
}

func TestPersonProviderDaemonRemovePreflightsStoredCredentialBeforeRevoke(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	configured := personProviderTestConfig()
	beta := configuredPersonProvider(configured)
	beta.Model = "beta-model"
	beta.Credential = peoplesweep.CredentialStored
	beta.CredentialEnv = ""
	configured.Providers["beta"] = beta
	path, _ := retainedPersonProviderTestConfig(t, configured)

	tokensDir := t.TempDir()
	credentials := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(credentials.Save("beta", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary,
	)))
	credentialPath := filepath.Join(tokensDir, "people-providers", "beta.json")
	retainedPath := credentialPath + ".retained"
	require.NoError(os.Rename(credentialPath, retainedPath))
	externalPath := filepath.Join(t.TempDir(), "external-credential")
	require.NoError(os.WriteFile(externalPath, []byte("must-remain"), 0o600))
	require.NoError(os.Symlink(externalPath, credentialPath))

	revokes := 0
	edits := 0
	deps := personProviderCommandDeps{
		config:                     func() peoplesweep.Config { return configured },
		isDaemonSubprocess:         func() bool { return false },
		providerStoreOwnedByDaemon: func(context.Context) (bool, error) { return true, nil },
		proxy: func(*cobra.Command, []string, map[string]string) error {
			revokes++
			return nil
		},
		readConfigFile: func() (config.ConfigFile, error) { return config.ReadConfigFile(path) },
		editConfigTables: func(etag string, planned []config.TableEdit) (config.ConfigFile, error) {
			edits++
			return config.EditConfigTables(path, etag, planned)
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			return config.RestoreConfigFile(path, published, before)
		},
		setup: personProviderSetupDeps{credentials: credentials},
	}

	output, err := executePersonProviderCommand(t, deps, "remove", "beta")
	require.Error(err)
	assert.Zero(revokes)
	assert.Zero(edits)
	assert.NotContains(output, providerSetupSecretCanary)
	assert.NotContains(err.Error(), providerSetupSecretCanary)
	external, readErr := os.ReadFile(externalPath)
	require.NoError(readErr)
	assert.Equal("must-remain", string(external))
	retained, readErr := os.ReadFile(retainedPath)
	require.NoError(readErr)
	assert.Contains(string(retained), providerSetupSecretCanary)
	finalConfig, loadErr := config.Load(path, "")
	require.NoError(loadErr)
	assert.Contains(finalConfig.People.Sweep.Providers, "beta")
}

func TestPersonProviderDaemonRemoveMissingCredentialRootHasZeroSideEffects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	configured := personProviderTestConfig()
	beta := configuredPersonProvider(configured)
	beta.Model = "beta-model"
	beta.Credential = peoplesweep.CredentialStored
	beta.CredentialEnv = ""
	configured.Providers["beta"] = beta
	path, _ := retainedPersonProviderTestConfig(t, configured)

	tokensParent := t.TempDir()
	tokensDir := filepath.Join(tokensParent, "missing-tokens")
	credentials := &deleteCountingCredentialStore{
		CredentialStore: peoplesweep.NewFileCredentialStore(tokensDir),
	}
	beforeTokensParent, statErr := os.Stat(tokensParent)
	require.NoError(statErr)
	beforeEntries, readErr := os.ReadDir(tokensParent)
	require.NoError(readErr)

	revokes := 0
	edits := 0
	deps := personProviderCommandDeps{
		config:                     func() peoplesweep.Config { return configured },
		isDaemonSubprocess:         func() bool { return false },
		providerStoreOwnedByDaemon: func(context.Context) (bool, error) { return true, nil },
		proxy: func(*cobra.Command, []string, map[string]string) error {
			revokes++
			return nil
		},
		readConfigFile: func() (config.ConfigFile, error) { return config.ReadConfigFile(path) },
		editConfigTables: func(etag string, planned []config.TableEdit) (config.ConfigFile, error) {
			edits++
			return config.EditConfigTables(path, etag, planned)
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			return config.RestoreConfigFile(path, published, before)
		},
		setup: personProviderSetupDeps{credentials: credentials},
	}

	output, err := executePersonProviderCommand(t, deps, "remove", "beta")
	require.ErrorContains(err, "preflight stored people provider credential deletion")
	assert.Zero(revokes)
	assert.Zero(edits)
	assert.Zero(credentials.deletes)
	assert.NotContains(output, providerSetupSecretCanary)
	assert.NotContains(err.Error(), providerSetupSecretCanary)
	assert.NoDirExists(tokensDir)
	afterTokensParent, statErr := os.Stat(tokensParent)
	require.NoError(statErr)
	assert.True(os.SameFile(beforeTokensParent, afterTokensParent))
	assert.Equal(beforeTokensParent.Mode(), afterTokensParent.Mode())
	afterEntries, readErr := os.ReadDir(tokensParent)
	require.NoError(readErr)
	assert.Equal(beforeEntries, afterEntries)
	finalConfig, loadErr := config.Load(path, "")
	require.NoError(loadErr)
	assert.Contains(finalConfig.People.Sweep.Providers, "beta")
}

func TestPersonProviderDaemonRemoveValidReplacementRaceRollsBackExactConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requireStoredCredentialStorePlatform(t)
	configured := personProviderTestConfig()
	beta := configuredPersonProvider(configured)
	beta.Model = "beta-model"
	beta.Credential = peoplesweep.CredentialStored
	beta.CredentialEnv = ""
	configured.Providers["beta"] = beta
	path, configBefore := retainedPersonProviderTestConfig(t, configured)

	tokensDir := t.TempDir()
	fileCredentials := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(fileCredentials.Save("beta", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, providerSetupSecretCanary,
	)))
	credentialPath := filepath.Join(tokensDir, "people-providers", "beta.json")
	retainedPath := credentialPath + ".post-preflight"
	credentialBefore, statErr := os.Stat(credentialPath)
	require.NoError(statErr)
	contentsBefore, readErr := os.ReadFile(credentialPath)
	require.NoError(readErr)
	replacementContents := []byte(`{"scheme":"bearer","value":"valid-replacement-test-value"}`)
	credentials := &postPreflightRaceCredentialStore{
		CredentialStore: fileCredentials,
		beforeDelete: func() error {
			if err := os.Rename(credentialPath, retainedPath); err != nil {
				return err
			}
			return os.WriteFile(credentialPath, replacementContents, 0o600)
		},
	}

	revokes := 0
	edits := 0
	restores := 0
	deps := personProviderCommandDeps{
		config:                     func() peoplesweep.Config { return configured },
		isDaemonSubprocess:         func() bool { return false },
		providerStoreOwnedByDaemon: func(context.Context) (bool, error) { return true, nil },
		proxy: func(*cobra.Command, []string, map[string]string) error {
			revokes++
			return nil
		},
		readConfigFile: func() (config.ConfigFile, error) { return config.ReadConfigFile(path) },
		editConfigTables: func(etag string, planned []config.TableEdit) (config.ConfigFile, error) {
			edits++
			return config.EditConfigTables(path, etag, planned)
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			restores++
			return config.RestoreConfigFile(path, published, before)
		},
		setup: personProviderSetupDeps{credentials: credentials},
	}

	output, err := executePersonProviderCommand(t, deps, "remove", "beta")
	require.ErrorContains(err, "credential changed during guarded deletion")
	require.ErrorContains(err, "exact people provider consent remains revoked")
	assert.Equal(1, revokes)
	assert.Equal(1, edits)
	assert.Equal(1, restores)
	assert.NotContains(output, providerSetupSecretCanary)
	if err != nil {
		assert.NotContains(err.Error(), providerSetupSecretCanary)
	}
	replacementAfter, readErr := os.ReadFile(credentialPath)
	require.NoError(readErr)
	assert.Equal(replacementContents, replacementAfter)
	credentialAfter, statErr := os.Stat(retainedPath)
	require.NoError(statErr)
	assert.True(os.SameFile(credentialBefore, credentialAfter))
	assert.Equal(credentialBefore.Mode(), credentialAfter.Mode())
	assert.Equal(credentialBefore.Size(), credentialAfter.Size())
	contentsAfter, readErr := os.ReadFile(retainedPath)
	require.NoError(readErr)
	assert.True(bytes.Equal(contentsBefore, contentsAfter), "retained credential changed")
	finalSnapshot, loadErr := config.ReadConfigFile(path)
	require.NoError(loadErr)
	assert.Equal(configBefore.Content, finalSnapshot.Content)
	finalConfig, loadErr := config.Load(path, "")
	require.NoError(loadErr)
	assert.Contains(finalConfig.People.Sweep.Providers, "beta")

	loaded := make(chan struct {
		credential peoplesweep.Credential
		err        error
	}, 1)
	go func() {
		credential, loadCredentialErr := fileCredentials.Load("beta")
		loaded <- struct {
			credential peoplesweep.Credential
			err        error
		}{credential: credential, err: loadCredentialErr}
	}()
	select {
	case result := <-loaded:
		require.NoError(result.err)
		assert.Equal("valid-replacement-test-value", result.credential.Value())
	case <-time.After(time.Second):
		assert.Fail("provider removal did not close the credential deletion guard")
	}
}

func TestPersonProviderAnonymousCheckForwardsNoCredential(t *testing.T) {
	config := personProviderTestConfig()
	mutateConfiguredPersonProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.Endpoint = "http://127.0.0.1:11434/v1"
		provider.Auth = peoplesweep.AuthNone
		provider.Credential = peoplesweep.CredentialNone
		provider.CredentialEnv = ""
	})
	var gotEnv map[string]string
	deps := personProviderCommandDeps{
		config:             func() peoplesweep.Config { return config },
		isDaemonSubprocess: func() bool { return false },
		lookupEnv: func(string) (string, bool) {
			require.FailNow(t, "anonymous check must not resolve a credential")
			return "", false
		},
		proxy: func(_ *cobra.Command, _ []string, env map[string]string) error {
			gotEnv = env
			return nil
		},
	}

	_, err := executePersonProviderCommand(t, deps, "check")
	require.NoError(t, err)
	assert.Nil(t, gotEnv)
}

// TestPersonProviderFrontendCheckUsesDirectStoreWithoutDaemon catches the
// final add check auto-starting a daemon instead of using the available local
// writer when no daemon owns the database.
func TestPersonProviderFrontendCheckUsesDirectStoreWithoutDaemon(t *testing.T) {
	configured := personProviderTestConfig()
	checker := &fixedPersonProviderChecker{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"ok":true}`), ProviderVersion: peoplesweep.OpenAICompatibleProviderVersion,
		ModelVersion: "direct-model-v1",
	}}
	st := testutil.NewSQLiteTestStore(t)
	deps := localPersonProviderDeps(configured, st, checker)
	deps.isDaemonSubprocess = func() bool { return false }
	deps.providerStoreOwnedByDaemon = func(context.Context) (bool, error) { return false, nil }
	deps.proxy = func(*cobra.Command, []string, map[string]string) error {
		require.FailNow(t, "no-daemon check must not proxy or auto-start a daemon")
		return assert.AnError
	}

	output, err := executePersonProviderCommand(t, deps, "check", "default", "--json")
	require.NoError(t, err)
	assert.Contains(t, output, `"ok":true`)
}

func TestPersonProviderCommandsRejectUnsafeNamesBeforeRoutingOrState(t *testing.T) {
	tests := []struct {
		operation string
		name      string
	}{
		{operation: "add", name: "--json"},
		{operation: "check", name: "bad\nname"},
		{operation: "use", name: strings.Repeat("u", 65)},
		{operation: "remove", name: "--help"},
		{operation: "status", name: "bad\rname"},
		{operation: "consent", name: " leading"},
		{operation: "revoke", name: "trailing "},
		{operation: "history", name: "--json"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			stateCalls := 0
			proxyCalls := 0
			deps := personProviderCommandDeps{
				config: func() peoplesweep.Config {
					stateCalls++
					return personProviderTestConfig()
				},
				isDaemonSubprocess: func() bool { return false },
				proxy: func(*cobra.Command, []string, map[string]string) error {
					proxyCalls++
					return nil
				},
			}

			_, err := executePersonProviderCommand(t, deps, test.operation, "--", test.name)
			require.Error(err)
			assert.Contains(err.Error(), "invalid people provider profile name")
			assert.NotContains(err.Error(), test.name)
			assert.Zero(stateCalls)
			assert.Zero(proxyCalls)
		})
	}
}
