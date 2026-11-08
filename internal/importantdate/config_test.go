package importantdate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigRequiresPrivateDestinationAndExplicitEnable(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	assert.False(t, cfg.Enabled)
	assert.NoError(t, cfg.Validate())
	cfg.Enabled = true
	assert.Error(t, cfg.Validate())
	cfg.PrivateDestination = true
	cfg.Project = "private-example"
	cfg.Recipient = "owner/agent"
	assert.NoError(t, cfg.Validate())
	cfg.Timezone = "Local"
	assert.Error(t, cfg.Validate())
}
