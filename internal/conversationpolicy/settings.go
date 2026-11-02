package conversationpolicy

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Settings is the credential-free persisted policy configuration.
type Settings struct {
	MaxParticipants    map[string]int `json:"max_participants"`
	ActivityWindowDays int            `json:"activity_window_days"`
	Overrides          []Override     `json:"overrides,omitempty"`
}

// DefaultSettings applies when no configuration has been saved. Automatic
// classification is opt-in, so every provider limit is zero (disabled).
func DefaultSettings() Settings {
	return settingsWithLimit(0)
}

// RecommendedSettings turns on automatic classification for every supported
// provider at RecommendedMaxParticipants.
func RecommendedSettings() Settings {
	return settingsWithLimit(RecommendedMaxParticipants)
}

func settingsWithLimit(limit int) Settings {
	return Settings{MaxParticipants: map[string]int{"beeper": limit, "slack": limit, "slackdump": limit, "discord": limit, "teams": limit}, ActivityWindowDays: DefaultActivityWindowDays}
}

// Supported reports whether a source has automatic conversation policy.
func Supported(sourceType string) bool {
	switch sourceType {
	case "beeper", "slack", "slackdump", "discord", "teams":
		return true
	}
	return false
}

// Validate rejects incomplete limits and unsupported retention tiers.
func (s Settings) Validate() error {
	if len(s.MaxParticipants) != 5 {
		return errors.New("conversation policy requires exactly five provider limits")
	}
	for _, provider := range []string{"beeper", "slack", "slackdump", "discord", "teams"} {
		count, ok := s.MaxParticipants[provider]
		if !ok || count < 0 {
			return fmt.Errorf("invalid conversation participant limit for %s", provider)
		}
	}
	if s.MaxParticipants["slackdump"] != s.MaxParticipants["slack"] {
		return errors.New("slackdump must use Slack's conversation participant limit")
	}
	if s.ActivityWindowDays <= 0 || s.ActivityWindowDays > int((1<<63-1)/(24*time.Hour)) {
		return errors.New("invalid conversation activity window")
	}
	seen := map[[3]string]bool{}
	for _, o := range s.Overrides {
		key := [3]string{o.SourceType, o.SourceIdentifier, o.ConversationID}
		if strings.TrimSpace(o.SourceType) == "" || strings.TrimSpace(o.SourceIdentifier) == "" || strings.TrimSpace(o.ConversationID) == "" {
			return errors.New("conversation override requires source and room identifiers")
		}
		if o.Tier != Full && o.Tier != Cold {
			return errors.New("conversation override supports only full and cold")
		}
		if seen[key] {
			return errors.New("duplicate conversation override")
		}
		seen[key] = true
	}
	return nil
}
