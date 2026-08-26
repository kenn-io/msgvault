package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonProviderFrontendRoutesExactCommandsAndCredential(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		keyValue    string
		wantArgs    []string
		wantEnv     map[string]string
		wantLookups int
	}{
		{name: "status", args: []string{"status", "--json"},
			wantArgs: []string{"person", "provider", "status", "--json"}},
		{name: "consent", args: []string{"consent", "--yes"},
			wantArgs: []string{"person", "provider", "consent", "--yes"}},
		{name: "revoke", args: []string{"revoke"},
			wantArgs: []string{"person", "provider", "revoke"}},
		{name: "login", args: []string{"login"},
			wantArgs: []string{"person", "provider", "login"}},
		{name: "models", args: []string{"models", "--json"},
			wantArgs: []string{"person", "provider", "models", "--json"}},
		{name: "semantic consent", args: []string{"consent", "--semantic-embeddings", "--yes"},
			wantArgs: []string{"person", "provider", "consent", "--semantic-embeddings", "--yes"}},
		{name: "check without key", args: []string{"check", "--json"},
			wantArgs: []string{"person", "provider", "check", "--json"}, wantLookups: 1},
		{name: "check with key", args: []string{"check"}, keyValue: "caller-key",
			wantArgs: []string{"person", "provider", "check"},
			wantEnv:  map[string]string{"TEST_PROVIDER_KEY": "caller-key"}, wantLookups: 1},
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
					return test.keyValue, test.keyValue != ""
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
			assert.Equal(test.wantEnv, gotEnv)
			assert.Equal(test.wantLookups, lookups)
		})
	}
}

func TestPersonProviderAnonymousCheckForwardsNoCredential(t *testing.T) {
	config := personProviderTestConfig()
	config.Provider.Endpoint = "http://127.0.0.1:11434/v1"
	config.Provider.APIKeyEnv = ""
	config.Provider.AllowAnonymous = true
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
