package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func remoteIngressConfigFixture(t *testing.T) RemoteIngressConfig {
	t.Helper()
	home := t.TempDir()
	apiFile := filepath.Join(home, "client-api-key")
	secretFile := filepath.Join(home, "signing-secret")
	require.NoError(t, os.WriteFile(apiFile, []byte(strings.Repeat("a", 40)), 0o600))
	require.NoError(t, os.WriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 64)))), 0o600))
	return RemoteIngressConfig{Enabled: true, Listen: "127.0.0.1:8081", ExternalURL: "https://archive.example.test/msgvault", ReplayStateFile: filepath.Join(home, "replay.json"), Clients: []RemoteIngressClient{{ClientID: "fixture-reader", APIKeyFile: apiFile, Keys: []RequestSigningKey{{KeyID: "reader-1", SecretFile: secretFile}}}}}
}

func TestRemoteIngressConfigRejectsUnsafePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RemoteIngressConfig)
	}{
		{"plaintext external", func(c *RemoteIngressConfig) { c.ExternalURL = "http://archive.example.test" }},
		{"external query", func(c *RemoteIngressConfig) { c.ExternalURL += "?x=y" }},
		{"wildcard bind", func(c *RemoteIngressConfig) { c.Listen = ":8081" }},
		{"public bind", func(c *RemoteIngressConfig) { c.Listen = "192.0.2.1:8081" }},
		{"missing clients", func(c *RemoteIngressConfig) { c.Clients = nil }},
		{"missing replay state", func(c *RemoteIngressConfig) { c.ReplayStateFile = "" }},
		{"unknown grant", func(c *RemoteIngressConfig) { c.Clients[0].Grants = []string{"admin"} }},
		{"missing signing keys", func(c *RemoteIngressConfig) { c.Clients[0].Keys = nil }},
		{"unbounded rotation", func(c *RemoteIngressConfig) {
			c.Clients[0].Keys = append(c.Clients[0].Keys, RequestSigningKey{KeyID: "reader-2", SecretFile: "second-secret"})
		}},
		{"long overlap", func(c *RemoteIngressConfig) {
			now := time.Now().Unix()
			c.Clients[0].Keys[0].NotAfter = now + 86401
			c.Clients[0].Keys = append(c.Clients[0].Keys, RequestSigningKey{KeyID: "reader-2", SecretFile: "second-secret", NotBefore: now})
		}},
		{"body too large", func(c *RemoteIngressConfig) { c.MaxRequestBytes = 65 << 20 }},
		{"excess concurrency", func(c *RemoteIngressConfig) { c.MaxConcurrent = 33 }},
		{"proxy parse", func(c *RemoteIngressConfig) { c.TrustedProxies = []string{"invalid"} }},
	} {
		t.Run(tc.name, func(t *testing.T) { c := remoteIngressConfigFixture(t); tc.change(&c); require.Error(t, c.Validate()) })
	}
	c := remoteIngressConfigFixture(t)
	assert.NoError(t, c.Validate())
	c.Clients[0].Grants = []string{"collections-write"}
	assert.NoError(t, c.Validate())
}

func TestRemoteSigningConfigurationIsAllOrNothing(t *testing.T) {
	for _, c := range []RemoteConfig{
		{URL: "https://archive.example.test", SigningKeyID: "reader-1"},
		{URL: "https://archive.example.test", SigningSecretFile: "secret"},
		{URL: "http://archive.example.test", AllowInsecure: true, SigningKeyID: "reader-1", SigningSecretFile: "secret"},
		{URL: "https://archive.example.test", APIKey: "key", APIKeyFile: "api-key"},
	} {
		require.Error(t, c.Validate())
	}
	assert.NoError(t, (&RemoteConfig{URL: "https://archive.example.test", APIKey: "private-key"}).Validate())
}

func TestRemoteSigningPathsRoundTripAndResolveFromConfig(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	content := `[remote]
url = "https://archive.example.test/msgvault"
api_key_file = "credentials/api-key"
signing_key_id = "reader-1"
signing_secret_file = "credentials/signing-key"
[server.remote_ingress]
enabled = true
external_url = "https://archive.example.test/msgvault"
replay_state_file = "replay.json"
[[server.remote_ingress.clients]]
client_id = "reader"
api_key_file = "credentials/api-key"
[[server.remote_ingress.clients.keys]]
key_id = "reader-1"
secret_file = "credentials/signing-key"
`
	requirements.NoError(os.WriteFile(path, []byte(content), 0o600))
	cfg, err := Load(path, "")
	requirements.NoError(err)
	assertions.Equal(filepath.Join(home, "credentials", "api-key"), cfg.Remote.APIKeyFile)
	assertions.Equal(filepath.Join(home, "credentials", "signing-key"), cfg.Remote.SigningSecretFile)
	assertions.Equal(filepath.Join(home, "replay.json"), cfg.Server.RemoteIngress.ReplayStateFile)
	assertions.Equal(filepath.Join(home, "credentials", "api-key"), cfg.Server.RemoteIngress.Clients[0].APIKeyFile)
	saved := filepath.Join(home, "saved.toml")
	cfg.configPath = saved
	requirements.NoError(cfg.Save())
	roundtrip, err := Load(saved, "")
	requirements.NoError(err)
	assertions.Equal(cfg.Remote, roundtrip.Remote)
	assertions.Equal(cfg.Server.RemoteIngress, roundtrip.Server.RemoteIngress)
}
