package api

import (
	"strings"

	"go.kenn.io/msgvault/internal/config"
)

// Primary meeting account identities are configured separately from confirmed
// aliases. Both participate in assignee classification, as in meetingidentity.
func personCardPrimaryEmail(cfg *config.Config, kind, identifier string) (string, error) {
	if cfg == nil {
		return "", nil
	}
	switch kind {
	case "granola":
		for _, s := range cfg.Granola {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "circleback":
		for _, s := range cfg.Circleback {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "notion_meetings":
		for _, s := range cfg.NotionMeetings {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "plaud":
		for _, s := range cfg.Plaud {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "muesli":
		for _, s := range cfg.Muesli {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "omi":
		for _, s := range cfg.Omi {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "twilio":
		for _, s := range cfg.Twilio {
			if s.Identifier == identifier {
				return s.EffectiveAccountEmail()
			}
		}
	case "gmail", "imap", "outlook", "msmail":
		for _, s := range cfg.Accounts {
			if strings.EqualFold(strings.TrimSpace(s.Email), strings.TrimSpace(identifier)) {
				return strings.ToLower(strings.TrimSpace(s.Email)), nil
			}
		}
	}
	return "", nil
}
