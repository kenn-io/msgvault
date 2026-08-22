package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
)

func TestCLIRunCommandAllowedPermitsExactPersonProviderCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "status", args: []string{"person", "provider", "status"}, want: true},
		{name: "status flags", args: []string{"person", "provider", "status", "--json"}, want: true},
		{name: "consent", args: []string{"person", "provider", "consent", "--yes"}, want: true},
		{name: "revoke", args: []string{"person", "provider", "revoke"}, want: true},
		{name: "check", args: []string{"person", "provider", "check", "--json"}, want: true},
		{name: "missing operation", args: []string{"person", "provider"}},
		{name: "unknown operation", args: []string{"person", "provider", "run"}},
		{name: "ordinary person mutation", args: []string{"person", "delete", "7"}},
		{name: "different nested group", args: []string{"person", "attributes", "list", "7"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, cliRunCommandAllowed(test.args))
		})
	}
}

func TestCLIRunEnvAllowedPermitsConfiguredPeopleProviderKeyOnly(t *testing.T) {
	srv := &Server{cfg: &config.Config{}}
	srv.cfg.People.Sweep.Provider.APIKeyEnv = "MSGVAULT_PEOPLE_PROVIDER_KEY"

	assert.True(t, srv.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
	assert.False(t, srv.cliRunEnvAllowed("OPENAI_API_KEY"))

	unconfigured := &Server{cfg: &config.Config{}}
	assert.False(t, unconfigured.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
}
