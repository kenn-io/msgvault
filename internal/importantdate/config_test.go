package importantdate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigRequiresPrivateDestinationAndExplicitEnable(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	cfg := Config{}
	cfg.ApplyDefaults()
	checks.False(cfg.Enabled)
	must.NoError(cfg.Validate())
	cfg.Enabled = true
	must.Error(cfg.Validate())
	cfg.PrivateDestination = true
	cfg.Project = "private-example"
	cfg.Recipient = "owner/agent"
	must.NoError(cfg.Validate())
	cfg.CustomTier = "E"
	must.Error(cfg.Validate())
	cfg.CustomTier = TierC
	cfg.UnassignedTier = TierD
	must.NoError(cfg.Validate())
	cfg.Timezone = "Local"
	must.Error(cfg.Validate())
}

func TestDestinationScopeBindsInstanceAndProjectIdentity(t *testing.T) {
	checks := assert.New(t)
	base := DestinationScope("https://private.example.test", "private", "project-uid-one")
	checks.NotEmpty(base)
	checks.NotEqual(base, DestinationScope("https://other.example.test", "private", "project-uid-one"))
	checks.NotEqual(base, DestinationScope("https://private.example.test", "private", "project-uid-two"))
	checks.Empty(DestinationScope("", "private", "project-uid-one"))
}
