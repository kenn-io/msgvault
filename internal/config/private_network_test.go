package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestKataPrivateNetworkConfigProperty(t *testing.T) {
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint64(0), ^uint64(0)))
		endpoint := fmt.Sprintf("http://service-%d.example.test:7777/api", n)
		mode := hegel.Draw(ht, hegel.Integers(0, 2))
		docbankTrust := hegel.Draw(ht, hegel.Booleans())
		trust := ""
		if mode != 0 {
			trust = fmt.Sprintf("trust_private_network = %t\n", mode == 2)
		}
		content := fmt.Sprintf("[integrations.kata]\nenabled = true\nendpoint = %q\n%s[integrations.docbank]\ntrust_private_network = %t\n", endpoint, trust, docbankTrust)
		require.NoError(ht, os.WriteFile(path, []byte(content), 0o600))
		cfg, err := Load(path, "")
		if mode != 2 {
			require.Error(ht, err)
			return
		}
		require.NoError(ht, err)
		assert.True(ht, cfg.Integrations.Kata.TrustPrivateNetwork)
		assert.Equal(ht, docbankTrust, cfg.Integrations.Docbank.TrustPrivateNetwork)
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}

func TestPrivateNetworkConfigStrictness(t *testing.T) {
	assertions := assert.New(t)
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	credentialURL := (&url.URL{Scheme: "http", Host: "kata", User: url.UserPassword("user", "pass")}).String()
	for _, tc := range []struct {
		name, section, endpoint, trust string
		wantError                      bool
	}{
		{"default false", "kata", "http://kata:7777", "", true},
		{"shrunk DNS regression", "kata", "http://service-0.example.test:7777/api", "trust_private_network = true", false},
		{"explicit false", "kata", "http://kata:7777", "trust_private_network = false", true},
		{"private accepted", "kata", "http://10.2.3.4:7777", "trust_private_network = true", false},
		{"public rejected", "kata", "http://8.8.8.8", "trust_private_network = true", true},
		{"userinfo rejected", "kata", credentialURL, "trust_private_network = true", true},
		{"query rejected", "kata", "http://kata?key=value", "trust_private_network = true", true},
		{"fragment rejected", "kata", "http://kata#fragment", "trust_private_network = true", true},
		{"generic tasks stay strict", "tasks", "http://tasks:7777", "trust_private_network = true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDaemonSectionConfig(t, fmt.Sprintf("[integrations.%s]\nendpoint = %q\n%s\n", tc.section, tc.endpoint, tc.trust))
			_, err := Load(path, "")
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	path := writeDaemonSectionConfig(t, "[integrations.docbank]\ntrust_private_network = true\nurl = 'http://docbank:8485'\n")
	cfg, err := Load(path, "")
	require.NoError(t, err)
	assertions.Equal("http://docbank:8485", cfg.Integrations.Docbank.URL)
	assertions.True(cfg.Integrations.Docbank.TrustPrivateNetwork)
	assertions.False(cfg.Integrations.Kata.TrustPrivateNetwork)
}

func TestPrivateNetworkTrustSettingIsKataOnly(t *testing.T) {
	path := writeDaemonSectionConfig(t, "[integrations.tasks]\nendpoint = \"http://localhost:7777\"\ntrust_private_network = true\n")
	_, err := Load(path, "")
	require.ErrorContains(t, err, "trust_private_network is only supported in [integrations.kata]")
}
