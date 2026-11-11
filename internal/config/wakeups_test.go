package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWakeupValidationAndDefaults(t *testing.T) {
	base := WakeupConfig{Name: "inbox", URL: "https://example.com/hook", SecretEnv: "HOOK_SECRET"}
	configs := []WakeupConfig{base}
	require.NoError(t, prepareWakeups(configs))
	assert.Equal(t, 10*time.Second, configs[0].Timeout)
	assert.Equal(t, 72*time.Hour, configs[0].MaxMessageAge)
	require.NotNil(t, configs[0].IncomingOnly)
	assert.True(t, *configs[0].IncomingOnly)
	for _, tc := range []struct {
		name   string
		mutate func(*WakeupConfig)
	}{
		{"name", func(c *WakeupConfig) { c.Name = "Bad Name" }},
		{"scheme", func(c *WakeupConfig) { c.URL = "http://example.com" }},
		{"port", func(c *WakeupConfig) { c.URL = "https://example.com:444/" }},
		{"userinfo", func(c *WakeupConfig) { c.URL = "https://user@example.com/" }},
		{"fragment", func(c *WakeupConfig) { c.URL = "https://example.com/#fragment" }},
		{"private", func(c *WakeupConfig) { c.URL = "https://127.0.0.1/" }},
		{"missing secret", func(c *WakeupConfig) { c.SecretEnv = "" }},
		{"both secrets", func(c *WakeupConfig) { c.SecretFile = "secret" }},
		{"timeout", func(c *WakeupConfig) { c.Timeout = 61 * time.Second }},
		{"age", func(c *WakeupConfig) { c.MaxMessageAge = -time.Second }},
		{"person", func(c *WakeupConfig) { c.PersonID = -1 }},
		{"source", func(c *WakeupConfig) { c.SourceIDs = []int64{0} }},
		{"conversation", func(c *WakeupConfig) { c.ConversationIDs = []int64{-1} }},
	} {
		t.Run(tc.name, func(t *testing.T) { c := base; tc.mutate(&c); assert.Error(t, prepareWakeups([]WakeupConfig{c})) })
	}
	assert.Error(t, prepareWakeups([]WakeupConfig{base, base}))
	assert.Error(t, prepareWakeups(make([]WakeupConfig, 33)))
}

func TestInlineWakeupSecretRejectedByConfigLoader(t *testing.T) {
	path := writeDaemonSectionConfig(t, "[[wakeups]]\nname='inbox'\nurl='https://example.com/hook'\nsecret_env='WAKEUP_KEY'\nsecret='not-allowed'\n")
	_, err := Load(path, "")
	require.ErrorContains(t, err, "unknown wakeups config key")
}
