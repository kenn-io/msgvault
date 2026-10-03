package config

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.kenn.io/msgvault/internal/chatwoot"
)

// ChatwootSource configures one account on a Chatwoot instance. A registered
// archive source is created for each included inbox, independent of this label.
type ChatwootSource struct {
	Identifier             string  `toml:"identifier"`
	URL                    string  `toml:"url"`
	AccountID              int64   `toml:"account_id"`
	APIKeyEnv              string  `toml:"api_key_env"`
	Enabled                bool    `toml:"enabled"`
	Schedule               string  `toml:"schedule"`
	Inboxes                []int64 `toml:"inboxes"`
	ExcludeInboxes         []int64 `toml:"exclude_inboxes"`
	SelfAgentIDs           []int64 `toml:"self_agent_ids"`
	IncludePrivate         *bool   `toml:"include_private"`
	Media                  *bool   `toml:"media"`
	MaxMediaMB             int64   `toml:"max_media_mb"`
	ReconcileIntervalHours int     `toml:"reconcile_interval_hours"`
}

func (s ChatwootSource) PrivateIncluded() bool { return s.IncludePrivate == nil || *s.IncludePrivate }
func (s ChatwootSource) MediaEnabled() bool    { return s.Media == nil || *s.Media }
func (s ChatwootSource) MaxMediaBytes() int64 {
	if s.MaxMediaMB == 0 {
		return DefaultChatMaxMediaBytes
	}
	return s.MaxMediaMB << 20
}
func (s ChatwootSource) ReconcileInterval() time.Duration {
	if s.ReconcileIntervalHours == 0 {
		return 24 * time.Hour
	}
	return time.Duration(s.ReconcileIntervalHours) * time.Hour
}
func (s ChatwootSource) InboxIncluded(id int64) bool {
	return id > 0 && !slices.Contains(s.ExcludeInboxes, id) && (len(s.Inboxes) == 0 || slices.Contains(s.Inboxes, id))
}

func (c *Config) GetChatwootSource(identifier string) *ChatwootSource {
	for _, src := range c.Chatwoot {
		if strings.EqualFold(src.Identifier, identifier) {
			return &src
		}
	}
	return nil
}
func (c *Config) ScheduledChatwootSources() []ChatwootSource {
	var out []ChatwootSource
	for _, src := range c.Chatwoot {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}

var chatwootEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Config) validateChatwootSources() error {
	labels := map[string]bool{}
	accounts := map[string]bool{}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	for i := range c.Chatwoot {
		src := &c.Chatwoot[i]
		src.Identifier = strings.TrimSpace(src.Identifier)
		if src.Identifier == "" {
			return fmt.Errorf("chatwoot[%d].identifier must not be empty", i)
		}
		label := strings.ToLower(src.Identifier)
		if labels[label] {
			return fmt.Errorf("chatwoot[%d].identifier is duplicate", i)
		}
		labels[label] = true
		canonical, err := chatwoot.CanonicalURL(src.URL)
		if err != nil {
			return fmt.Errorf("chatwoot[%d].url: %w", i, err)
		}
		src.URL = canonical
		if src.AccountID <= 0 {
			return fmt.Errorf("chatwoot[%d].account_id must be positive", i)
		}
		account := fmt.Sprintf("%s/accounts/%d", canonical, src.AccountID)
		if accounts[account] {
			return fmt.Errorf("chatwoot[%d] duplicates an instance/account pair", i)
		}
		accounts[account] = true
		src.APIKeyEnv = strings.TrimSpace(src.APIKeyEnv)
		if src.APIKeyEnv == "" {
			src.APIKeyEnv = "MSGVAULT_CHATWOOT_TOKEN"
		}
		if !chatwootEnvName.MatchString(src.APIKeyEnv) {
			return fmt.Errorf("chatwoot[%d].api_key_env must name an environment variable", i)
		}
		if src.MaxMediaMB < 0 || src.MaxMediaMB > math.MaxInt64/(1<<20) {
			return fmt.Errorf("chatwoot[%d].max_media_mb must be a nonnegative representable MiB limit", i)
		}
		if src.ReconcileIntervalHours < 0 || int64(src.ReconcileIntervalHours) > math.MaxInt64/int64(time.Hour) {
			return fmt.Errorf("chatwoot[%d].reconcile_interval_hours must be a nonnegative representable duration", i)
		}
		for name, ids := range map[string][]int64{"inboxes": src.Inboxes, "exclude_inboxes": src.ExcludeInboxes, "self_agent_ids": src.SelfAgentIDs} {
			for _, id := range ids {
				if id <= 0 {
					return fmt.Errorf("chatwoot[%d].%s must contain positive IDs", i, name)
				}
			}
		}
		if src.Schedule != "" {
			if _, err := parser.Parse(src.Schedule); err != nil {
				return fmt.Errorf("chatwoot[%d].schedule: %w", i, err)
			}
		}
	}
	return nil
}
