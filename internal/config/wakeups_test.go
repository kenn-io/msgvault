package config

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
)

func TestWakeupValidationAndDefaults(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	base := WakeupConfig{Name: "inbox", URL: "https://example.com/hook", SecretEnv: "HOOK_SECRET"}
	configs := []WakeupConfig{base}
	require.NoError(prepareWakeups(configs))
	assert.Equal(10*time.Second, configs[0].Timeout)
	assert.Equal(72*time.Hour, configs[0].MaxMessageAge)
	require.NotNil(configs[0].IncomingOnly)
	assert.True(*configs[0].IncomingOnly)
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
		{"person", func(c *WakeupConfig) { c.PersonID = func() *int64 { v := int64(-1); return &v }() }},
		{"source", func(c *WakeupConfig) { c.SourceIDs = []int64{0} }},
		{"conversation", func(c *WakeupConfig) { c.ConversationIDs = []int64{-1} }},
	} {
		t.Run(tc.name, func(t *testing.T) { c := base; tc.mutate(&c); Assert.Error(t, prepareWakeups([]WakeupConfig{c})) })
	}
	assert.Error(prepareWakeups([]WakeupConfig{base, base}))
	assert.Error(prepareWakeups(make([]WakeupConfig, 33)))
}

func TestInlineWakeupSecretRejectedByConfigLoader(t *testing.T) {
	path := writeDaemonSectionConfig(t, "[[wakeups]]\nname='inbox'\nurl='https://example.com/hook'\nsecret_env='WAKEUP_KEY'\nsecret='not-allowed'\n")
	_, err := Load(path, "")
	Require.ErrorContains(t, err, "unknown wakeups config key")
}

func TestWakeupPersonSelectorPresence(t *testing.T) {
	for _, tc := range []struct {
		selector string
		valid    bool
	}{{"", true}, {"person_id=0\n", false}, {"person_id=-1\n", false}, {"person_id=42\n", true}} {
		path := writeDaemonSectionConfig(t, "[[wakeups]]\nname='inbox'\nurl='https://example.com/hook'\nsecret_env='WAKEUP_KEY'\n"+tc.selector)
		_, err := Load(path, "")
		if tc.valid {
			Assert.NoError(t, err)
		} else {
			Assert.Error(t, err)
		}
	}
}
