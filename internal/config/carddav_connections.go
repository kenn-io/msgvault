package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const DefaultCardDAVConnection = "default"

var cardDAVConnectionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var ErrDuplicateCardDAVAccount = errors.New("CardDAV account already belongs to connection")

// SameAccount compares configured identities. Google account emails are case
// insensitive and independent of the OAuth app used to authorize them.
func (c CardDAVConfig) SameAccount(other CardDAVConfig) bool {
	if c.Username == "" || other.Username == "" {
		return false
	}
	if c.Provider == "google" && other.Provider == "google" {
		return strings.EqualFold(strings.TrimSpace(c.Username), strings.TrimSpace(other.Username))
	}
	return c.BaseURL != "" && c.BaseURL == other.BaseURL && c.Username == other.Username
}

// ValidateCardDAVConnectionIdentity rejects an account saved under another name,
// including disabled connections, which may still have imported contacts.
func (c *Config) ValidateCardDAVConnectionIdentity(name string, next CardDAVConfig) error {
	if name != DefaultCardDAVConnection && next.SameAccount(c.CardDAV) {
		return fmt.Errorf("%w %q", ErrDuplicateCardDAVAccount, DefaultCardDAVConnection)
	}
	for existingName, existing := range c.CardDAVConnections {
		if existingName != name && next.SameAccount(existing) {
			return fmt.Errorf("%w %q", ErrDuplicateCardDAVAccount, existingName)
		}
	}
	return nil
}

// ValidateCardDAVConnectionName validates both named connections and the
// legacy default selector. The default name is reserved in the named map.
func ValidateCardDAVConnectionName(name string) error {
	if !cardDAVConnectionNamePattern.MatchString(name) {
		return errors.New("CardDAV connection name must start with a lowercase letter and contain at most 64 lowercase letters, digits, underscores or hyphens")
	}
	return nil
}

func (c *Config) validateCardDAVConnections() error {
	for name, connection := range c.CardDAVConnections {
		if err := ValidateCardDAVConnectionName(name); err != nil {
			return fmt.Errorf("carddav_connections: %w", err)
		}
		if name == DefaultCardDAVConnection {
			return errors.New("carddav_connections.default is reserved; configure the default connection in [carddav]")
		}
		if connection.Provider != "" && connection.Provider != "google" {
			return fmt.Errorf("carddav_connections.%s.provider must be empty or \"google\"", name)
		}
		if _, _, err := connection.TrustedDestination(); err != nil {
			return fmt.Errorf("carddav_connections.%s: %w", name, err)
		}
		if err := c.ValidateCardDAVConnectionIdentity(name, connection); err != nil {
			return fmt.Errorf("carddav_connections.%s: %w", name, err)
		}
	}
	return nil
}
