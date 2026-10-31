package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/conversationpolicy"
)

// ConversationPolicyConfig is separate from attachment limits. Overrides are
// staged configuration until all downstream consumers support cold eligibility.
type ConversationPolicyConfig struct {
	ActivityWindowDays int                           `toml:"activity_window_days"`
	Overrides          []conversationpolicy.Override `toml:"overrides"`
}

// ActivityWindow resolves the omitted/zero window to the default. Validation
// must precede this conversion to prevent duration overflow.
func (c ConversationPolicyConfig) ActivityWindow() time.Duration {
	days := c.ActivityWindowDays
	if days == 0 {
		days = conversationpolicy.DefaultActivityWindowDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// ConversationParticipantLimit returns zero for providers without room policy.
func (c *Config) ConversationParticipantLimit(sourceType string) int {
	switch strings.TrimSuffix(sourceType, ":") {
	case "beeper":
		return c.Beeper.MaxParticipants
	case "slack", "slackdump":
		return c.Slack.MaxParticipants
	case "discord":
		return c.Discord.MaxParticipants
	case "teams":
		return c.Teams.MaxParticipants
	default:
		return 0
	}
}

// ConversationOverride resolves a source-scoped declarative override. A saved
// CLI override takes precedence at the Store boundary.
func (c *Config) ConversationOverride(sourceType, sourceIdentifier, conversationID string) conversationpolicy.Tier {
	for _, o := range c.ConversationPolicy.Overrides {
		if o.SourceType == sourceType && o.SourceIdentifier == sourceIdentifier && o.ConversationID == conversationID {
			return o.Tier
		}
	}
	return ""
}

func (c *Config) validateConversationPolicy() error {
	for _, provider := range []string{"beeper", "slack", "discord", "teams"} {
		if c.ConversationParticipantLimit(provider) < 0 {
			return fmt.Errorf("[%s] max_participants must be nonnegative", provider)
		}
	}
	days := c.ConversationPolicy.ActivityWindowDays
	if days < 0 || days > int((1<<63-1)/(24*time.Hour)) {
		return errors.New("[conversation_policy] activity_window_days is out of range")
	}
	seen := make(map[[3]string]bool)
	for _, o := range c.ConversationPolicy.Overrides {
		key := [3]string{o.SourceType, o.SourceIdentifier, o.ConversationID}
		if strings.TrimSpace(o.SourceType) == "" || strings.TrimSpace(o.SourceIdentifier) == "" || strings.TrimSpace(o.ConversationID) == "" {
			return errors.New("conversation override requires source_type, source_identifier, and conversation_id")
		}
		if _, err := conversationpolicy.ParseTier(string(o.Tier)); err != nil {
			return err
		}
		if o.Tier == conversationpolicy.Stub || o.Tier == conversationpolicy.Skip {
			return fmt.Errorf("tier %s requires retention/purge support, which is not enabled", o.Tier)
		}
		if seen[key] {
			return fmt.Errorf("duplicate conversation override for %q/%q/%q", key[0], key[1], key[2])
		}
		seen[key] = true
	}
	return nil
}
