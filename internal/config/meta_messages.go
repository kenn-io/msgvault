package config

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/meta"
)

// MetaMessagesSource selects one inbox; TokenEnv is an environment variable name.
type MetaMessagesSource struct {
	Identifier         string   `toml:"identifier"`
	AppID              string   `toml:"app_id"`
	PageID             string   `toml:"page_id"`
	InstagramAccountID string   `toml:"instagram_account_id"`
	TokenEnv           string   `toml:"token_env"`
	Enabled            bool     `toml:"enabled"`
	Schedule           string   `toml:"schedule"`
	Actions            []string `toml:"actions"`
}

func (s MetaMessagesSource) ActionAllowed(action string) bool { return ActionsAllow(s.Actions, action) }
func findMessagesSource(list []MetaMessagesSource, id string) *MetaMessagesSource {
	for _, s := range list {
		if strings.EqualFold(s.Identifier, id) {
			return &s
		}
	}
	return nil
}
func scheduledMessagesSources(list []MetaMessagesSource) []MetaMessagesSource {
	var out []MetaMessagesSource
	for _, s := range list {
		if s.Enabled && s.Schedule != "" {
			out = append(out, s)
		}
	}
	return out
}
func (c *Config) GetFacebookMessagesSource(id string) *MetaMessagesSource {
	return findMessagesSource(c.FacebookMessages, id)
}
func (c *Config) GetInstagramMessagesSource(id string) *MetaMessagesSource {
	return findMessagesSource(c.InstagramMessages, id)
}
func (c *Config) ScheduledFacebookMessagesSources() []MetaMessagesSource {
	return scheduledMessagesSources(c.FacebookMessages)
}
func (c *Config) ScheduledInstagramMessagesSources() []MetaMessagesSource {
	return scheduledMessagesSources(c.InstagramMessages)
}

func validateMetaMessagesSources(kind string, list []MetaMessagesSource, needIG bool) error {
	assets := map[string]bool{}
	enabledIGRoutes := map[[2]string]bool{}
	for i := range list {
		s := &list[i]
		s.Identifier = strings.TrimSpace(s.Identifier)
		if s.Identifier == "" {
			return fmt.Errorf("%s[%d].identifier is empty or duplicate", kind, i)
		}
		for _, prior := range list[:i] {
			if strings.EqualFold(prior.Identifier, s.Identifier) {
				return fmt.Errorf("%s[%d].identifier is empty or duplicate", kind, i)
			}
		}
		if !meta.ValidAccountID(s.AppID) || !meta.ValidAccountID(s.PageID) || (needIG && !meta.ValidAccountID(s.InstagramAccountID)) {
			return fmt.Errorf("%s[%d] requires decimal app_id, page_id and, for Instagram, instagram_account_id", kind, i)
		}
		if !needIG && s.InstagramAccountID != "" {
			return fmt.Errorf("%s[%d].instagram_account_id is only valid for Instagram", kind, i)
		}
		key := meta.SourceIdentifier(s.AppID, s.PageID)
		if needIG {
			key = meta.InstagramSourceIdentifier(s.AppID, s.PageID, s.InstagramAccountID)
		}
		if assets[key] {
			return fmt.Errorf("%s[%d] duplicates an app/inbox tuple", kind, i)
		}
		assets[key] = true
		if needIG && s.Enabled {
			route := [2]string{s.AppID, s.InstagramAccountID}
			if enabledIGRoutes[route] {
				return fmt.Errorf("%s[%d] duplicates an enabled app/instagram_account_id webhook route", kind, i)
			}
			enabledIGRoutes[route] = true
		}
		if !envutil.ValidName(s.TokenEnv) {
			return fmt.Errorf("%s[%d].token_env must name an explicit environment variable", kind, i)
		}
		if s.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(s.Schedule); err != nil {
				return fmt.Errorf("%s[%d].schedule: %w", kind, i, err)
			}
		}
		if err := ValidateMetaActions(kind, i, s.Actions, "send", "human_agent"); err != nil {
			return err
		}
		if s.ActionAllowed("human_agent") && !s.ActionAllowed("send") {
			return fmt.Errorf("%s[%d].actions: human_agent requires send", kind, i)
		}
	}
	return nil
}
