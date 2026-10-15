package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

const DefaultInlineMCPEndpoint = "https://mcp.inline.chat/mcp/v2"

// InlineConfig selects accounts, schedules sync, and controls attachment collection.
// Chat selection belongs to each account; an empty list archives all available chats.
type InlineConfig struct {
	Enabled              bool                          `toml:"enabled"`
	Schedule             string                        `toml:"schedule"`
	Accounts             []InlineAccount               `toml:"accounts"`
	Media                *bool                         `toml:"media"`
	MediaScope           string                        `toml:"media_scope"`
	MediaMaxParticipants int                           `toml:"media_max_participants"`
	MaxMediaMB           int                           `toml:"max_media_mb"`
	AccountsConfig       map[string]MediaAccountConfig `toml:"accounts_config"`
}

// InlineAccount binds an authenticated Inline principal to all available chats or an explicit filter.
// Credentials are stored separately on the daemon host; CLI authentication stays
// with the configured Inline executable.
type InlineAccount struct {
	Identifier string   `toml:"identifier"`
	Transport  string   `toml:"transport"`
	ChatIDs    []string `toml:"chat_ids"`
	Endpoint   string   `toml:"endpoint"`
	CLIPath    string   `toml:"cli_path"`
}

func (a InlineAccount) EffectiveTransport() string {
	if a.Transport == "" {
		return "mcp"
	}
	return a.Transport
}

func (a InlineAccount) EffectiveEndpoint() string {
	if a.Endpoint == "" {
		return DefaultInlineMCPEndpoint
	}
	return a.Endpoint
}

// Validate rejects identities and pagination IDs that would collide after parsing.
func (a InlineAccount) Validate() error {
	const prefix = "api.inline.chat:user:"
	if !strings.HasPrefix(a.Identifier, prefix) || !inlinePositiveID(strings.TrimPrefix(a.Identifier, prefix)) {
		return errors.New("[[inline.accounts]] identifier must be api.inline.chat:user:<positive decimal user ID>")
	}
	if a.EffectiveTransport() != "mcp" && a.EffectiveTransport() != "cli" {
		return fmt.Errorf("[[inline.accounts]] %q: transport must be mcp or cli", a.Identifier)
	}
	seen := make(map[string]bool, len(a.ChatIDs))
	for _, id := range a.ChatIDs {
		if !inlinePositiveID(id) {
			return fmt.Errorf("[[inline.accounts]] %q: chat ID %q must be a positive canonical decimal ID no larger than 9007199254740991", a.Identifier, id)
		}
		if seen[id] {
			return fmt.Errorf("[[inline.accounts]] %q: duplicate chat ID %q", a.Identifier, id)
		}
		seen[id] = true
	}
	if a.EffectiveEndpoint() != DefaultInlineMCPEndpoint {
		return fmt.Errorf("[[inline.accounts]] %q: endpoint must be %s", a.Identifier, DefaultInlineMCPEndpoint)
	}
	if strings.TrimSpace(a.CLIPath) != a.CLIPath || strings.ContainsAny(a.CLIPath, "\r\n\x00") {
		return fmt.Errorf("[[inline.accounts]] %q: cli_path must name an executable without surrounding whitespace", a.Identifier)
	}
	return nil
}

func inlinePositiveID(id string) bool {
	value, err := strconv.ParseInt(id, 10, 64)
	return err == nil && value > 0 && value <= 1<<53-1 && strconv.FormatInt(value, 10) == id
}

func (c *Config) validateInlineAccounts() error {
	seen := make(map[string]bool, len(c.Inline.Accounts))
	for _, account := range c.Inline.Accounts {
		if err := account.Validate(); err != nil {
			return err
		}
		if seen[account.Identifier] {
			return fmt.Errorf("[[inline.accounts]] duplicate identifier %q", account.Identifier)
		}
		seen[account.Identifier] = true
	}
	return nil
}

// GetInlineAccount returns a detached copy of the configured account.
func (c *Config) GetInlineAccount(identifier string) *InlineAccount {
	for _, account := range c.Inline.Accounts {
		if account.Identifier == identifier {
			account.ChatIDs = slices.Clone(account.ChatIDs)
			return &account
		}
	}
	return nil
}

// UpsertInlineAccount replaces transport settings and merges explicit chat filters.
// An empty incoming filter restores all available chats. It does not save the
// config or enable scheduling.
func (c *Config) UpsertInlineAccount(account InlineAccount) error {
	if err := account.Validate(); err != nil {
		return err
	}
	account.ChatIDs = slices.Clone(account.ChatIDs)
	for index, previous := range c.Inline.Accounts {
		if previous.Identifier != account.Identifier {
			continue
		}
		if len(account.ChatIDs) > 0 {
			merged := slices.Clone(previous.ChatIDs)
			for _, id := range account.ChatIDs {
				if !slices.Contains(merged, id) {
					merged = append(merged, id)
				}
			}
			account.ChatIDs = merged
		}
		c.Inline.Accounts[index] = account
		return nil
	}
	c.Inline.Accounts = append(c.Inline.Accounts, account)
	return nil
}

// MediaPolicy resolves the provider policy and an override keyed by principal.
func (i InlineConfig) MediaPolicy(identifier string) attachmentpolicy.Policy {
	account, ok := i.AccountsConfig[identifier]
	return resolveMediaPolicy(i.Media, i.MediaScope, i.MediaMaxParticipants,
		i.MaxMediaMB, DefaultChatMaxMediaBytes, account, ok)
}
