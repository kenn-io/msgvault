package api

import (
	"github.com/spf13/pflag"
	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"hegel.dev/go/hegel"
	"io"
	"testing"
)

func TestKeepInTouchCLIIsOwnerOnlyAndForwardsExactCredential(t *testing.T) {
	assert := Assert.New(t)

	args := []string{"person", "keep-in-touch", "check", "--json"}
	assert.True(cliRunCommandAllowed(args))
	assert.False(cliRunCommandAllowed([]string{"person", "keep-in-touch", "send"}))
	assert.False(delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit}}))
	srv := &Server{cfg: &config.Config{}}
	srv.cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "synthetic"}
	srv.cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{"synthetic": {Protocol: peoplesweep.ProtocolOpenAIChat, Credential: peoplesweep.CredentialEnv, CredentialEnv: "OUTREACH_KEY"}}
	assert.True(srv.cliRunEnvAllowedForCommand(args, "OUTREACH_KEY"))
	assert.False(srv.cliRunEnvAllowedForCommand(args, "OTHER_KEY"))
	assert.False(srv.cliRunEnvAllowedForCommand([]string{"person", "keep-in-touch", "check", "--dry-run"}, "OUTREACH_KEY"))
}

func TestKeepInTouchExplicitFalseIsActualInference(t *testing.T) {
	srv := &Server{cfg: &config.Config{}}
	srv.cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "synthetic"}
	srv.cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{"synthetic": {Protocol: peoplesweep.ProtocolOpenAIChat, Credential: peoplesweep.CredentialEnv, CredentialEnv: "OUTREACH_KEY"}}
	for _, flags := range [][]string{{"--dry-run=false"}, {"--dry-run", "--dry-run=false"}, {"--dry-run=1", "--dry-run=0"}} {
		args := append([]string{"person", "keep-in-touch", "check"}, flags...)
		Assert.True(t, srv.cliRunEnvAllowedForCommand(args, "OUTREACH_KEY"), "%v", args)
	}
}

func TestKeepInTouchPreviewMatchesPflag(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		forms := []string{"--dry-run", "--dry-run=true", "--dry-run=false", "--dry-run=1", "--dry-run=0", "--dry-run=T", "--dry-run=F"}
		count := hegel.Draw(ht, hegel.Integers(0, 12))
		args := []string{}
		for range count {
			args = append(args, forms[hegel.Draw(ht, hegel.Integers(0, len(forms)-1))])
		}
		flags := pflag.NewFlagSet("preview", pflag.ContinueOnError)
		flags.SetOutput(io.Discard)
		expected := flags.Bool("dry-run", false, "")
		require.NoError(ht, flags.Parse(args))
		Assert.Equal(ht, *expected, cliRunOutreachPreview(args))
	})
	Assert.True(t, cliRunOutreachPreview([]string{"--dry-run=invalid"}))
}
