package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/fileutil"
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
		t.Run(tc.name, func(t *testing.T) { c := base; tc.mutate(&c); Require.Error(t, prepareWakeups([]WakeupConfig{c})) })
	}
	require.Error(prepareWakeups([]WakeupConfig{base, base}))
	entries := make([]WakeupConfig, 33)
	for i := range entries {
		entries[i] = base
		entries[i].Name = fmt.Sprintf("inbox-%d", i)
	}
	require.NoError(prepareWakeups(entries[:32]))
	require.Error(prepareWakeups(entries))
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
	}{{"", true}, {"person_id=0\n", false}, {"person_id=42\n", true}} {
		path := writeDaemonSectionConfig(t, "[[wakeups]]\nname='inbox'\nurl='https://example.com/hook'\nsecret_env='WAKEUP_KEY'\n"+tc.selector)
		_, err := Load(path, "")
		if tc.valid {
			Assert.NoError(t, err)
		} else {
			Require.Error(t, err)
		}
	}
}

func TestWakeupSecretFileResolutionPreservesConfig(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	require.NoError(fileutil.SecureWriteFile(filepath.Join(dir, "secret"), []byte(secret), 0600))
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[[wakeups]]\nname='relative'\nurl='https://example.com'\nsecret_file='secret'\n[[wakeups]]\nname='home'\nurl='https://example.com'\nsecret_file='~/secret'\n"), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	for _, entry := range cfg.Wakeups {
		got, err := cfg.ResolveWakeupSecret(entry)
		require.NoError(err)
		assert.Equal(secret, got)
	}
	require.NoError(cfg.Save())
	saved, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(saved), "secret_file = \"secret\"")
	assert.Contains(string(saved), "secret_file = \"~/secret\"")
}

func TestWakeupDestinationRejectsPrivateNativeTargets(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/hook", "https://127.0.0.1/hook", "https://localhost/hook", "https://example.com:444/hook"} {
		assert.Error(t, ValidateWakeupDestination(endpoint, nil), endpoint)
	}
	assert.NoError(t, ValidateWakeupDestination("https://example.com/hook", nil))

}
