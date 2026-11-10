package config

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/meta"
)

// FacebookCommentsSource selects one Page. Tokens exist only in the named
// daemon environment variable; the profile label is not an archive identity.
type FacebookCommentsSource struct {
	Identifier string `toml:"identifier"`
	AppID      string `toml:"app_id"`
	PageID     string `toml:"page_id"`
	TokenEnv   string `toml:"token_env"`
	Enabled    bool   `toml:"enabled"`
	Schedule   string `toml:"schedule"`
	// Actions opts the profile in to changing comments on Meta.
	Actions []string `toml:"actions"`
}

func (c *Config) GetFacebookCommentsSource(identifier string) *FacebookCommentsSource {
	for _, source := range c.FacebookComments {
		if strings.EqualFold(source.Identifier, identifier) {
			return &source
		}
	}
	return nil
}

func (c *Config) ScheduledFacebookCommentsSources() []FacebookCommentsSource {
	var sources []FacebookCommentsSource
	for _, source := range c.FacebookComments {
		if source.Enabled && source.Schedule != "" {
			sources = append(sources, source)
		}
	}
	return sources
}

func (c *Config) validateFacebookCommentsSources() error {
	assets := map[string]bool{}
	for i := range c.FacebookComments {
		source := &c.FacebookComments[i]
		source.Identifier = strings.TrimSpace(source.Identifier)
		if source.Identifier == "" {
			return fmt.Errorf("facebook_comments[%d].identifier is empty or duplicate", i)
		}
		for _, prior := range c.FacebookComments[:i] {
			if strings.EqualFold(prior.Identifier, source.Identifier) {
				return fmt.Errorf("facebook_comments[%d].identifier is empty or duplicate", i)
			}
		}
		if !meta.ValidAccountID(source.AppID) || !meta.ValidAccountID(source.PageID) {
			return fmt.Errorf("facebook_comments[%d] requires decimal app_id and page_id", i)
		}
		key := meta.SourceIdentifier(source.AppID, source.PageID)
		if assets[key] {
			return fmt.Errorf("facebook_comments[%d] duplicates an app/Page pair", i)
		}
		assets[key] = true
		if !envutil.ValidName(source.TokenEnv) {
			return fmt.Errorf("facebook_comments[%d].token_env must name an explicit environment variable", i)
		}
		if err := validateCommentActions(source.Actions); err != nil {
			return fmt.Errorf("facebook_comments[%d].actions: %w", i, err)
		}
		if source.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(source.Schedule); err != nil {
				return fmt.Errorf("facebook_comments[%d].schedule: %w", i, err)
			}
		}
	}
	return nil
}

// InstagramCommentsSource selects one linked professional account. Tokens exist only in the named
// daemon environment variable; the profile label is not an archive identity.
type InstagramCommentsSource struct {
	Identifier         string `toml:"identifier"`
	AppID              string `toml:"app_id"`
	PageID             string `toml:"page_id"`
	InstagramAccountID string `toml:"instagram_account_id"`
	TokenEnv           string `toml:"token_env"`
	Enabled            bool   `toml:"enabled"`
	Schedule           string `toml:"schedule"`
	// Actions opts the profile in to changing comments on Meta.
	Actions []string `toml:"actions"`
}

func (c *Config) GetInstagramCommentsSource(identifier string) *InstagramCommentsSource {
	for _, source := range c.InstagramComments {
		if strings.EqualFold(source.Identifier, identifier) {
			return &source
		}
	}
	return nil
}

func (c *Config) ScheduledInstagramCommentsSources() []InstagramCommentsSource {
	var sources []InstagramCommentsSource
	for _, source := range c.InstagramComments {
		if source.Enabled && source.Schedule != "" {
			sources = append(sources, source)
		}
	}
	return sources
}

func (c *Config) validateInstagramCommentsSources() error {
	assets := map[string]bool{}
	for i := range c.InstagramComments {
		source := &c.InstagramComments[i]
		source.Identifier = strings.TrimSpace(source.Identifier)
		if source.Identifier == "" {
			return fmt.Errorf("instagram_comments[%d].identifier is empty or duplicate", i)
		}
		for _, prior := range c.InstagramComments[:i] {
			if strings.EqualFold(prior.Identifier, source.Identifier) {
				return fmt.Errorf("instagram_comments[%d].identifier is empty or duplicate", i)
			}
		}
		if !meta.ValidAccountID(source.AppID) || !meta.ValidAccountID(source.PageID) || !meta.ValidAccountID(source.InstagramAccountID) {
			return fmt.Errorf("instagram_comments[%d] requires decimal app_id, page_id and instagram_account_id", i)
		}
		key := meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID)
		if assets[key] {
			return fmt.Errorf("instagram_comments[%d] duplicates an app/Page/Instagram tuple", i)
		}
		assets[key] = true
		if !envutil.ValidName(source.TokenEnv) {
			return fmt.Errorf("instagram_comments[%d].token_env must name an explicit environment variable", i)
		}
		if err := validateCommentActions(source.Actions); err != nil {
			return fmt.Errorf("instagram_comments[%d].actions: %w", i, err)
		}
		if source.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(source.Schedule); err != nil {
				return fmt.Errorf("instagram_comments[%d].schedule: %w", i, err)
			}
		}
	}
	return nil
}

// validateCommentActions accepts the comment changes a profile can opt in to.
func validateCommentActions(actions []string) error {
	for _, action := range actions {
		if action != string(meta.ActionHide) && action != string(meta.ActionDelete) {
			return fmt.Errorf("unknown action %q; use \"hide\" or \"delete\"", action)
		}
	}
	return nil
}
