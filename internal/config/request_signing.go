package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"go.kenn.io/msgvault/internal/requestsign"
)

type RequestSigningKey struct {
	KeyID      string `toml:"key_id"`
	SecretFile string `toml:"secret_file"`
	NotBefore  int64  `toml:"not_before,omitzero"`
	NotAfter   int64  `toml:"not_after,omitzero"`
}
type RemoteIngressClient struct {
	ClientID   string              `toml:"client_id"`
	APIKeyFile string              `toml:"api_key_file"`
	Grants     []string            `toml:"grants"`
	Keys       []RequestSigningKey `toml:"keys"`
}
type RemoteIngressConfig struct {
	Enabled         bool                  `toml:"enabled"`
	Listen          string                `toml:"listen,omitzero"`
	ExternalURL     string                `toml:"external_url"`
	TrustedProxies  []string              `toml:"trusted_proxies"`
	ReplayStateFile string                `toml:"replay_state_file"`
	MaxRequestBytes int64                 `toml:"max_request_bytes,omitzero"`
	MaxConcurrent   int                   `toml:"max_concurrent,omitzero"`
	Clients         []RemoteIngressClient `toml:"clients"`
}

func (c *RemoteIngressConfig) ListenAddr() string {
	if c.Listen == "" {
		return "127.0.0.1:8081"
	}
	return c.Listen
}
func (c *RemoteIngressConfig) BodyLimit() int64 {
	if c.MaxRequestBytes == 0 {
		return requestsign.DefaultMaxRequestBytes
	}
	return c.MaxRequestBytes
}
func (c *RemoteIngressConfig) ConcurrentLimit() int {
	if c.MaxConcurrent == 0 {
		return 4
	}
	return c.MaxConcurrent
}
func (c *RemoteIngressConfig) ProxyAllowlist() []string {
	if len(c.TrustedProxies) == 0 {
		return []string{"127.0.0.0/8", "::1"}
	}
	return c.TrustedProxies
}
func (c *RemoteIngressConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if _, err := requestsign.ValidateBase(c.ExternalURL); err != nil {
		return fmt.Errorf("remote ingress external_url: %w", err)
	}
	host, port, err := net.SplitHostPort(c.ListenAddr())
	if err != nil {
		return errors.New("remote ingress listen must be a private IP and port")
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) || err != nil || n < 0 || n > 65535 {
		return errors.New("remote ingress listener must bind a literal private or loopback IP")
	}
	if c.ReplayStateFile == "" || len(c.Clients) < 1 || len(c.Clients) > 32 {
		return errors.New("remote ingress requires replay_state_file and one to 32 clients")
	}
	if c.BodyLimit() < 1 || c.BodyLimit() > requestsign.MaxRequestBytes || c.ConcurrentLimit() < 1 || c.ConcurrentLimit() > 32 {
		return errors.New("remote ingress request or concurrency limit is invalid")
	}
	for _, proxy := range c.ProxyAllowlist() {
		if net.ParseIP(proxy) == nil {
			if _, _, err := net.ParseCIDR(proxy); err != nil {
				return errors.New("remote ingress trusted_proxies must contain IPs or CIDRs")
			}
		}
	}
	clients := make(map[string]bool)
	keys := make(map[string]bool)
	for _, client := range c.Clients {
		if !requestsign.ValidID(client.ClientID) || clients[client.ClientID] || client.APIKeyFile == "" || len(client.Keys) < 1 || len(client.Keys) > 2 {
			return errors.New("remote ingress clients require unique IDs, API credential files and one or two signing keys")
		}
		clients[client.ClientID] = true
		seenGrant := false
		for _, grant := range client.Grants {
			if grant != "collections-write" || seenGrant {
				return errors.New("remote ingress accepts only a single collections-write grant")
			}
			seenGrant = true
		}
		for _, key := range client.Keys {
			if !requestsign.ValidID(key.KeyID) || keys[key.KeyID] || key.SecretFile == "" || key.NotBefore < 0 || key.NotAfter < 0 || (key.NotAfter != 0 && key.NotAfter <= key.NotBefore) {
				return errors.New("remote ingress signing keys require unique IDs, secret files and valid dates")
			}
			keys[key.KeyID] = true
		}
		if len(client.Keys) == 2 {
			old, next := client.Keys[0], client.Keys[1]
			if old.NotAfter == 0 || next.NotBefore == 0 || next.NotBefore < old.NotBefore || old.NotAfter-next.NotBefore > 86400 {
				return errors.New("key rotation requires an outgoing expiry and incoming activation with at most 24 hours overlap")
			}
		}
	}
	return nil
}

func (c *RemoteConfig) SigningEnabled() bool {
	return c.SigningKeyID != "" || c.SigningSecretFile != ""
}
func (c *RemoteConfig) Validate() error {
	if c.APIKey != "" && c.APIKeyFile != "" {
		return errors.New("remote api_key and api_key_file are mutually exclusive")
	}
	if c.MaxRequestBytes < 0 || c.MaxRequestBytes > requestsign.MaxRequestBytes {
		return errors.New("remote max_request_bytes must be at most 64 MiB")
	}
	if !c.SigningEnabled() {
		return nil
	}
	if !requestsign.ValidID(c.SigningKeyID) || c.SigningSecretFile == "" {
		return errors.New("remote signing requires signing_key_id and signing_secret_file")
	}
	if _, err := requestsign.ValidateBase(c.URL); err != nil {
		return fmt.Errorf("remote signing URL: %w", err)
	}
	if c.APIKey == "" && c.APIKeyFile == "" {
		return errors.New("remote signing requires a native API credential")
	}
	return nil
}

func (c *Config) resolveSigningPaths(explicit bool) {
	base := c.HomeDir
	if explicit {
		base = filepath.Dir(c.configPath)
	}
	resolve := func(path string) string {
		if path == "" {
			return ""
		}
		return resolveRelative(expandPath(path), base)
	}
	c.Remote.APIKeyFile = resolve(c.Remote.APIKeyFile)
	c.Remote.SigningSecretFile = resolve(c.Remote.SigningSecretFile)
	c.Server.RemoteIngress.ReplayStateFile = resolve(c.Server.RemoteIngress.ReplayStateFile)
	for i := range c.Server.RemoteIngress.Clients {
		client := &c.Server.RemoteIngress.Clients[i]
		client.APIKeyFile = resolve(client.APIKeyFile)
		for j := range client.Keys {
			client.Keys[j].SecretFile = resolve(client.Keys[j].SecretFile)
		}
	}
}
