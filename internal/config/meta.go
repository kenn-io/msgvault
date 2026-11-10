package config

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/meta"
)

// FacebookCommentsSource selects one Page, for [[facebook_comments]] and for
// its Messenger inbox in [[facebook_messages]]. Tokens exist only in the named
// daemon environment variable; the profile label is not an archive identity.
type FacebookCommentsSource struct {
	Identifier string `toml:"identifier"`
	AppID      string `toml:"app_id"`
	PageID     string `toml:"page_id"`
	TokenEnv   string `toml:"token_env"`
	Enabled    bool   `toml:"enabled"`
	Schedule   string `toml:"schedule"`
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
	if err := validateFacebookPageProfiles("facebook_comments", c.FacebookComments); err != nil {
		return err
	}
	return validateFacebookPageProfiles("facebook_messages", c.FacebookMessages)
}

// validateFacebookPageProfiles checks one section of Page profiles, such as
// [[facebook_comments]] or [[facebook_messages]].
func validateFacebookPageProfiles(section string, profiles []FacebookCommentsSource) error {
	assets := map[string]bool{}
	for i := range profiles {
		source := &profiles[i]
		source.Identifier = strings.TrimSpace(source.Identifier)
		if source.Identifier == "" {
			return fmt.Errorf("%s[%d].identifier is empty or duplicate", section, i)
		}
		for _, prior := range profiles[:i] {
			if strings.EqualFold(prior.Identifier, source.Identifier) {
				return fmt.Errorf("%s[%d].identifier is empty or duplicate", section, i)
			}
		}
		if !meta.ValidAccountID(source.AppID) || !meta.ValidAccountID(source.PageID) {
			return fmt.Errorf("%s[%d] requires decimal app_id and page_id", section, i)
		}
		key := meta.SourceIdentifier(source.AppID, source.PageID)
		if assets[key] {
			return fmt.Errorf("%s[%d] duplicates an app/Page pair", section, i)
		}
		assets[key] = true
		if !envutil.ValidName(source.TokenEnv) {
			return fmt.Errorf("%s[%d].token_env must name an explicit environment variable", section, i)
		}
		if source.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(source.Schedule); err != nil {
				return fmt.Errorf("%s[%d].schedule: %w", section, i, err)
			}
		}
	}
	return nil
}

// InstagramCommentsSource selects one linked professional account, for
// [[instagram_comments]] and for its inbox in [[instagram_messages]]. Tokens
// exist only in the named daemon environment variable; the profile label is
// not an archive identity.
type InstagramCommentsSource struct {
	Identifier         string `toml:"identifier"`
	AppID              string `toml:"app_id"`
	PageID             string `toml:"page_id"`
	InstagramAccountID string `toml:"instagram_account_id"`
	TokenEnv           string `toml:"token_env"`
	Enabled            bool   `toml:"enabled"`
	Schedule           string `toml:"schedule"`
	Mentions           bool   `toml:"mentions"` // also archive tagged media as an instagram_mentions source
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
	if err := validateInstagramProfiles("instagram_comments", c.InstagramComments); err != nil {
		return err
	}
	for i, source := range c.InstagramMessages {
		if source.Mentions {
			return fmt.Errorf("instagram_messages[%d].mentions is only valid on [[instagram_comments]]", i)
		}
	}
	return validateInstagramProfiles("instagram_messages", c.InstagramMessages)
}

// validateInstagramProfiles checks one section of linked Instagram account
// profiles, such as [[instagram_comments]] or [[instagram_messages]].
func validateInstagramProfiles(section string, profiles []InstagramCommentsSource) error {
	assets := map[string]bool{}
	for i := range profiles {
		source := &profiles[i]
		source.Identifier = strings.TrimSpace(source.Identifier)
		if source.Identifier == "" {
			return fmt.Errorf("%s[%d].identifier is empty or duplicate", section, i)
		}
		for _, prior := range profiles[:i] {
			if strings.EqualFold(prior.Identifier, source.Identifier) {
				return fmt.Errorf("%s[%d].identifier is empty or duplicate", section, i)
			}
		}
		if !meta.ValidAccountID(source.AppID) || !meta.ValidAccountID(source.PageID) || !meta.ValidAccountID(source.InstagramAccountID) {
			return fmt.Errorf("%s[%d] requires decimal app_id, page_id and instagram_account_id", section, i)
		}
		key := meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID)
		if assets[key] {
			return fmt.Errorf("%s[%d] duplicates an app/Page/Instagram tuple", section, i)
		}
		assets[key] = true
		if !envutil.ValidName(source.TokenEnv) {
			return fmt.Errorf("%s[%d].token_env must name an explicit environment variable", section, i)
		}
		if source.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(source.Schedule); err != nil {
				return fmt.Errorf("%s[%d].schedule: %w", section, i, err)
			}
		}
	}
	return nil
}
