// Package persontiers decides which saved people have enough two-way contact
// in the archive to be worth tracking. It has no database or provider side
// effects.
package persontiers

import (
	"errors"
	"fmt"
	"strings"
)

// Config holds the windows, thresholds and exclusions of the tier rules. Start
// with DefaultConfig before decoding so explicit zero alternatives stay off.
type Config struct {
	ShortWindowDays        int             `toml:"short_window_days"`
	LongWindowDays         int             `toml:"long_window_days"`
	ExcludeListMessages    bool            `toml:"exclude_list_messages"`
	ExcludeAddressPatterns []string        `toml:"exclude_address_patterns"`
	ExcludeDomainPatterns  []string        `toml:"exclude_domain_patterns"`
	Tier1                  Tier1Thresholds `toml:"tier1"`
	Tier2                  Tier2Thresholds `toml:"tier2"`
}

// Tier1Thresholds are measured over the short window, except the two breadth
// alternatives, which look across the long window.
type Tier1Thresholds struct {
	MinOutboundShort          int `toml:"min_outbound_short"`
	MinInboundShort           int `toml:"min_inbound_short"`
	MinActiveDaysShort        int `toml:"min_active_days_short"`
	BreadthMinActiveDaysShort int `toml:"breadth_min_active_days_short"`
	BreadthMinChannelsLong    int `toml:"breadth_min_channels_long"`
	BreadthMinMeetingDaysLong int `toml:"breadth_min_meeting_days_long"`
}

// Tier2Thresholds are measured over the long window.
type Tier2Thresholds struct {
	MinOutboundLong    int `toml:"min_outbound_long"`
	MinInboundLong     int `toml:"min_inbound_long"`
	MinActiveDaysLong  int `toml:"min_active_days_long"`
	MinMeetingDaysLong int `toml:"min_meeting_days_long"`
}

// ErrInvalidConfig marks every [people.tiers] error, from validation or an
// unknown key, so callers can refuse to evaluate on any of them.
var ErrInvalidConfig = errors.New("invalid people.tiers config")

// DefaultConfig returns the shipped windows, thresholds and exclusions.
func DefaultConfig() Config {
	return Config{
		ShortWindowDays: 90, LongWindowDays: 365,
		ExcludeListMessages: true,
		ExcludeAddressPatterns: []string{
			"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply",
			"notification", "notifications", "mailer-daemon", "postmaster",
			"bounce", "bounces", "newsletter", "digest", "alerts", "calendar-notification",
		},
		ExcludeDomainPatterns: []string{"resource.calendar.google.com"},
		Tier1:                 Tier1Thresholds{2, 2, 6, 3, 2, 2},
		Tier2:                 Tier2Thresholds{1, 1, 3, 2},
	}
}

// Validate rejects windows, thresholds and patterns the rules cannot evaluate.
// Every error wraps ErrInvalidConfig.
func (c Config) Validate() error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return nil
}

func (c Config) validate() error {
	if c.ShortWindowDays < 1 || c.LongWindowDays < c.ShortWindowDays || c.LongWindowDays > 3650 {
		return errors.New("people.tiers windows must satisfy 1 <= short_window_days <= long_window_days <= 3650")
	}
	required := []struct {
		name string
		n    int
	}{
		{"tier1.min_outbound_short", c.Tier1.MinOutboundShort},
		{"tier1.min_inbound_short", c.Tier1.MinInboundShort},
		{"tier1.min_active_days_short", c.Tier1.MinActiveDaysShort},
		{"tier1.breadth_min_active_days_short", c.Tier1.BreadthMinActiveDaysShort},
		{"tier2.min_outbound_long", c.Tier2.MinOutboundLong},
		{"tier2.min_inbound_long", c.Tier2.MinInboundLong},
		{"tier2.min_active_days_long", c.Tier2.MinActiveDaysLong},
	}
	for _, r := range required {
		if r.n < 1 {
			return fmt.Errorf("people.tiers.%s must be at least 1", r.name)
		}
	}
	if c.Tier1.BreadthMinChannelsLong != 0 && c.Tier1.BreadthMinChannelsLong < 2 {
		return errors.New("people.tiers.tier1.breadth_min_channels_long must be 0 or at least 2")
	}
	if c.Tier1.BreadthMinMeetingDaysLong < 0 || c.Tier2.MinMeetingDaysLong < 0 {
		return errors.New("people.tiers meeting thresholds must be 0 (disabled) or at least 1")
	}
	for _, patterns := range [][]string{c.ExcludeAddressPatterns, c.ExcludeDomainPatterns} {
		for _, p := range patterns {
			if p == "" || p != strings.ToLower(p) || strings.ContainsAny(p, "@ \t\r\n") || strings.Contains(strings.TrimSuffix(p, "*"), "*") {
				return fmt.Errorf("people.tiers exclusion pattern %q must be nonempty lower-case with at most one trailing * and no @", p)
			}
		}
	}
	return nil
}

func matches(value string, patterns []string) bool {
	value = strings.ToLower(value)
	for _, p := range patterns {
		if value == p || (strings.HasSuffix(p, "*") && strings.HasPrefix(value, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}
