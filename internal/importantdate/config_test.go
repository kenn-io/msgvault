package importantdate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigRequiresPrivateDestinationAndExplicitEnable(t *testing.T) {
	checks := assert.New(t)
	cfg := Config{}
	cfg.ApplyDefaults()
	checks.False(cfg.Enabled)
	checks.NoError(cfg.Validate())
	cfg.Enabled = true
	checks.Error(cfg.Validate())
	cfg.PrivateDestination = true
	cfg.Project = "private-example"
	cfg.Recipient = "owner/agent"
	checks.NoError(cfg.Validate())
	cfg.CustomTier = "E"
	checks.Error(cfg.Validate())
	cfg.CustomTier = TierC
	cfg.UnassignedTier = TierD
	checks.NoError(cfg.Validate())
	cfg.Timezone = "Local"
	checks.Error(cfg.Validate())
}

func TestDestinationScopeBindsInstanceAndProjectIdentity(t *testing.T) {
	checks := assert.New(t)
	base := DestinationScope("https://private.example.test", "private", "project-uid-one")
	checks.NotEmpty(base)
	checks.NotEqual(base, DestinationScope("https://other.example.test", "private", "project-uid-one"))
	checks.NotEqual(base, DestinationScope("https://private.example.test", "private", "project-uid-two"))
	checks.Empty(DestinationScope("", "private", "project-uid-one"))
}
