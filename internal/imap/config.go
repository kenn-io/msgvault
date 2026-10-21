// Package imap provides an IMAP email client implementing gmail.API.
package imap

import "go.kenn.io/msgvault/internal/imapconfig"

// AuthMethod specifies how the IMAP client authenticates.
type AuthMethod = imapconfig.AuthMethod

const (
	AuthPassword = imapconfig.AuthPassword
	AuthXOAuth2  = imapconfig.AuthXOAuth2
)

// Config holds connection settings for an IMAP server.
type Config = imapconfig.Config

// ConfigFromJSON parses the canonical native IMAP configuration.
func ConfigFromJSON(s string) (*Config, error) { return imapconfig.ConfigFromJSON(s) }

// ParseIdentifier parses a canonical IMAP source identifier.
func ParseIdentifier(s string) (*Config, error) { return imapconfig.ParseIdentifier(s) }

func normalizeHost(s string) string         { return imapconfig.NormalizeHost(s) }
func trimFolders(folders []string) []string { return imapconfig.TrimFolders(folders) }
