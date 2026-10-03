package documentindex

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral"
)

func doclingTestConfig() DocumentsConfig {
	c := DefaultDocumentsConfig()
	c.Provider = "docling"
	c.Endpoint = "http://127.0.0.1:5001"
	c.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	return c
}

func TestDoclingEndpointRejectsAlternateDestinations(t *testing.T) {
	requirements := require.New(t)

	for _, endpoint := range []string{
		"", "ftp://127.0.0.1:5001", "http://", "http://user:secret@localhost:5001",
		"http://localhost:5001/path", "http://localhost:5001/", "http://localhost:5001?key=secret",
		"http://localhost:5001#fragment", "http://localhost:5001?", "http://localhost:5001#",
		" http://localhost:5001", "http://localhost:5001\n", "http://localhost:5001:bad", "http://localhost:65536", "http://localhost:5001:5002", "HTTP://localhost:5001",
	} {
		t.Run(endpoint, func(t *testing.T) {
			requirements := require.New(t)

			c := doclingTestConfig()
			c.Endpoint = endpoint
			requirements.ErrorContains(c.Validate(), "endpoint")
		})
	}
	for _, endpoint := range []string{"http://127.0.0.1:5001", "https://docling.example.com", "http://[::1]:5001"} {
		c := doclingTestConfig()
		c.Endpoint = endpoint
		requirements.NoError(c.Validate())
	}
}

func TestDoclingEndpointRequiresHTTPSOutsidePrivateNetworks(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		valid    bool
	}{
		{endpoint: "http://docling.example.com"},
		{endpoint: "http://192.0.2.10"},
		{endpoint: "http://8.8.8.8:5001"},
		{endpoint: "http://100.64.0.1:5001"},
		{endpoint: "http://[2001:db8::1]:5001"},
		{endpoint: "http://localhost.example"},
		{endpoint: "http://docling.local:5001"},
		{endpoint: "http://192.168.1.20:5001", valid: true},
		{endpoint: "http://10.0.0.5:5001", valid: true},
		{endpoint: "http://172.16.4.2:5001", valid: true},
		{endpoint: "http://169.254.10.1:5001", valid: true},
		{endpoint: "http://[fd00::5]:5001", valid: true},
		{endpoint: "http://[fe80::1]:5001", valid: true},
		{endpoint: "https://docling.example.com", valid: true},
		{endpoint: "http://localhost:5001", valid: true},
		{endpoint: "http://127.0.0.2:5001", valid: true},
		{endpoint: "http://[::1]:5001", valid: true},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			c := doclingTestConfig()
			c.Endpoint = test.endpoint
			err := c.Validate()
			if test.valid {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "HTTPS")
		})
	}
}

func TestDoclingAPIKeyRequiresHTTPSOffLoopback(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		valid    bool
	}{
		{endpoint: "http://192.168.1.20:5001"},
		{endpoint: "http://[fe80::1]:5001"},
		{endpoint: "http://127.0.0.1:5001", valid: true},
		{endpoint: "https://192.168.1.20:5001", valid: true},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			c := doclingTestConfig()
			c.Endpoint, c.APIKeyEnv = test.endpoint, "SYNTHETIC_DOCLING_KEY"
			err := c.Validate()
			if test.valid {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "api_key_env")
		})
	}
}

func TestDoclingPolicyBindsUploadAuthorityWithoutCredentials(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	c := doclingTestConfig()
	c.APIKeyEnv = "SYNTHETIC_DOCLING_KEY"
	t.Setenv(c.APIKeyEnv, "synthetic-secret-value")
	input, err := ResolveInputPolicy(&c, mistral.CapabilityManifest{})
	requirements.NoError(err)
	requirements.Contains(input.AllowedMediaTypes, "application/pdf")
	requirements.Contains(input.AllowedMediaTypes, "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	requirements.Contains(input.AllowedMediaTypes, "text/html")
	requirements.Contains(input.AllowedMediaTypes, "text/markdown")
	requirements.Len(input.AllowedMediaTypes, 8)
	fingerprint, err := c.ProfileFingerprint(mistral.CapabilityManifest{}, input.AllowedMediaTypes)
	requirements.NoError(err)
	encoded, err := c.ProfilePolicyJSON(mistral.CapabilityManifest{}, input.AllowedMediaTypes)
	requirements.NoError(err)
	assertions.NotContains(string(encoded), "synthetic-secret-value")
	assertions.Contains(string(encoded), c.Endpoint)
	assertions.Contains(string(encoded), c.APIKeyEnv)
	for _, test := range []struct {
		name   string
		mutate func(*DocumentsConfig)
	}{
		{"endpoint", func(c *DocumentsConfig) { c.Endpoint = "http://127.0.0.1:5002" }},
		{"credential binding", func(c *DocumentsConfig) { c.APIKeyEnv = "OTHER_DOCLING_KEY" }},
		{"inline scope", func(c *DocumentsConfig) { c.Scope.IncludeInline = true }},
		{"message scope", func(c *DocumentsConfig) { c.Scope.MessageTypes = []string{"email"} }},
		{"total timeout", func(c *DocumentsConfig) { c.TotalTimeout += time.Second }},
		{"poll interval", func(c *DocumentsConfig) { c.PollInterval += time.Second }},
		{"poll attempts", func(c *DocumentsConfig) { c.MaxPollAttempts++ }},
		{"file bound", func(c *DocumentsConfig) { c.MaxFileBytes-- }},
		{"output bound", func(c *DocumentsConfig) { c.MaxPagesPerDocument-- }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			changed := c
			test.mutate(&changed)
			got, err := changed.ProfileFingerprint(mistral.CapabilityManifest{}, input.AllowedMediaTypes)
			requirements.NoError(err)
			assertions.NotEqual(fingerprint, got)
		})
	}
	reversed := append([]string(nil), input.AllowedMediaTypes...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	got, err := c.ProfileFingerprint(mistral.CapabilityManifest{}, reversed)
	requirements.NoError(err)
	assertions.Equal(fingerprint, got)
}

func FuzzDoclingEndpointOrigin(f *testing.F) {
	for _, seed := range []string{"http://127.0.0.1:5001", "https://docling.example.com", "http://user:secret@localhost", "http://localhost/path", "http://localhost?", "http://localhost#"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, endpoint string) {
		c := doclingTestConfig()
		c.Endpoint = endpoint
		if c.Validate() != nil {
			return
		}
		u, err := url.Parse(endpoint)
		require.NoError(t, err)
		assert.Contains(t, []string{"http", "https"}, u.Scheme)
		assert.NotEmpty(t, u.Hostname())
		assert.Nil(t, u.User)
		assert.Empty(t, u.Path)
		assert.Empty(t, u.RawQuery)
		assert.Empty(t, u.Fragment)
		assert.False(t, u.ForceQuery)
		assert.Equal(t, strings.TrimSpace(endpoint), endpoint)
	})
}

func TestDoclingDescriptorBindsTheOperatorPolicy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	c := doclingTestConfig()
	descriptor, err := c.DoclingDescriptor()
	requirements.NoError(err)
	assertions.Equal("docling.serve-v1", descriptor.ID)
	assertions.Equal("operator_network", string(descriptor.TrustBoundary))
	assertions.True(descriptor.ReturnsMarkdown)
	assertions.True(descriptor.ReturnsStructured)
	assertions.Len(descriptor.SupportedFormats, 8)
	changed := c
	changed.Endpoint = "https://docling.example.com"
	other, err := changed.DoclingDescriptor()
	requirements.NoError(err)
	assertions.NotEqual(descriptor.Fingerprint, other.Fingerprint)
	assertions.NotEqual(descriptor.PolicyFingerprint, other.PolicyFingerprint)
}
