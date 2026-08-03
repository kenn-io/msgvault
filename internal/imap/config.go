// Package imap provides an IMAP email client implementing gmail.API.
package imap

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// AuthMethod specifies how the IMAP client authenticates.
type AuthMethod string

const (
	// AuthPassword uses traditional LOGIN (username + password).
	AuthPassword AuthMethod = "password"
	// AuthXOAuth2 uses XOAUTH2 SASL mechanism (OAuth2 bearer token).
	AuthXOAuth2 AuthMethod = "xoauth2"
)

// Config holds connection settings for an IMAP server.
type Config struct {
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	TLS        bool       `json:"tls"`      // Implicit TLS (IMAPS, port 993)
	STARTTLS   bool       `json:"starttls"` // STARTTLS upgrade (port 143)
	Username   string     `json:"username"`
	AuthMethod AuthMethod `json:"auth_method,omitempty"`

	// Folders is a case-insensitive allow list. When non-empty,
	// only these mailboxes are synced. An empty list means "all
	// mailboxes". CLI --folder/--skip-folder override this config:
	// they are applied after config folders in
	// buildMessageListCache, so CLI values always take precedence.
	Folders []string `json:"folders,omitempty"`
}

// EffectiveAuthMethod returns the auth method, defaulting to password
// when the field is empty (backward compatibility with existing configs).
func (c *Config) EffectiveAuthMethod() AuthMethod {
	if c.AuthMethod == "" {
		return AuthPassword
	}
	return c.AuthMethod
}

// Addr returns the "host:port" string.
func (c *Config) Addr() string {
	port := c.Port
	if port == 0 {
		if c.TLS {
			port = 993
		} else {
			port = 143
		}
	}
	return net.JoinHostPort(normalizeHost(c.Host), strconv.Itoa(port))
}

// Identifier returns a canonical string like "imaps://user@host:port".
// The scheme encodes the transport mode: imaps (TLS), imap+starttls
// (STARTTLS), or imap (plaintext).
func (c *Config) Identifier() string {
	scheme := "imap"
	if c.TLS {
		scheme = "imaps"
	} else if c.STARTTLS {
		scheme = "imap+starttls"
	}
	port := c.Port
	if port == 0 {
		if c.TLS {
			port = 993
		} else {
			port = 143
		}
	}
	return fmt.Sprintf(
		"%s://%s@%s",
		scheme,
		url.PathEscape(c.Username),
		net.JoinHostPort(normalizeHost(c.Host), strconv.Itoa(port)),
	)
}

// normalizeHost strips surrounding IPv6 brackets so callers can pass either
// "::1" or "[::1]" and still get a valid host:port from net.JoinHostPort.
func normalizeHost(host string) string {
	return strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
}

// ToJSON serializes the config to JSON.
func (c *Config) ToJSON() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ConfigFromJSON parses a config from JSON.
func ConfigFromJSON(s string) (*Config, error) {
	var c Config
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, fmt.Errorf("parse IMAP config: %w", err)
	}
	c.Folders = trimFolders(c.Folders)
	return &c, nil
}

// trimFolders trims leading and trailing whitespace from each folder name
// in the list. It returns an empty slice if the result is empty after trimming.
func trimFolders(folders []string) []string {
	trimmed := make([]string, 0, len(folders))
	for _, f := range folders {
		t := strings.TrimSpace(f)
		if t != "" {
			trimmed = append(trimmed, t)
		}
	}
	return trimmed
}

// ParseIdentifier parses a config from an identifier URL like "imaps://user@host:port".
func ParseIdentifier(identifier string) (*Config, error) {
	u, err := url.Parse(identifier)
	if err != nil {
		return nil, fmt.Errorf("parse IMAP identifier: %w", err)
	}

	cfg := &Config{}
	switch u.Scheme {
	case "imaps":
		cfg.TLS = true
	case "imap+starttls":
		cfg.STARTTLS = true
	case "imap":
		// plaintext
	default:
		return nil, fmt.Errorf("unsupported scheme %q (expected imap, imaps, or imap+starttls)", u.Scheme)
	}

	cfg.Host = u.Hostname()
	cfg.Username = u.User.Username()

	portStr := u.Port()
	if portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %w", portStr, err)
		}
		cfg.Port = port
	}

	return cfg, nil
}
