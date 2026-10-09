package config

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

type MCPConfig struct {
	Events MCPEventsConfig `toml:"events"`
}

// MCPEventsConfig opts the daemon into scoped webhook delivery.
type MCPEventsConfig struct {
	Enabled          bool                 `toml:"enabled"`
	Retention        string               `toml:"retention"`
	Sources          []string             `toml:"sources"`
	TrustedCallbacks []MCPTrustedCallback `toml:"trusted_callbacks"`
}

type MCPTrustedCallback struct {
	Origin    string   `toml:"origin"`
	Addresses []string `toml:"addresses"`
}

func (c *MCPEventsConfig) ApplyDefaults() {
	if c.Retention == "" {
		c.Retention = "168h"
	}
	if c.Sources == nil {
		c.Sources = []string{"gmail", "imap", "gcal"}
	}
}

func (c *MCPEventsConfig) RetentionDuration() (time.Duration, error) {
	c.ApplyDefaults()
	duration, err := time.ParseDuration(c.Retention)
	if err != nil || duration <= 0 || duration > 7*24*time.Hour {
		return 0, errors.New("invalid [mcp.events] retention: want a positive duration at most 168h")
	}
	return duration, nil
}

func (c *MCPEventsConfig) Validate() error {
	c.ApplyDefaults()
	if _, err := c.RetentionDuration(); err != nil {
		return err
	}
	for _, source := range c.Sources {
		if source == "" || source != strings.TrimSpace(source) {
			return errors.New("invalid [mcp.events] sources: want nonempty source types")
		}
	}
	origins := make(map[string]bool)
	for _, entry := range c.TrustedCallbacks {
		origin, err := url.Parse(entry.Origin)
		if err != nil || (origin.Port() != "" && origin.Port() != "443" && origin.Port() != "8443") {
			return errors.New("invalid [mcp.events] trusted_callbacks: want HTTPS origins on ports 443 or 8443")
		}
		pins := make([]netip.Addr, 0, len(entry.Addresses))
		for _, raw := range entry.Addresses {
			address, err := netip.ParseAddr(raw)
			if err != nil {
				return errors.New("invalid [mcp.events] trusted_callbacks: invalid address pin")
			}
			pins = append(pins, address)
		}
		validated, _, err := netguard.ValidateTrustedDestination(origin, pins)
		if err != nil {
			return errors.New("invalid [mcp.events] trusted_callbacks: invalid origin or private address pins")
		}
		normalized := strings.ToLower(validated.String())
		normalized = strings.TrimSuffix(normalized, ":443")
		if origins[normalized] {
			return errors.New("invalid [mcp.events] trusted_callbacks: duplicate origin")
		}
		origins[normalized] = true
	}
	return nil
}
