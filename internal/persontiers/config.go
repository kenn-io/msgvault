// Package persontiers plans relationship maintenance from archived interaction
// metadata. It has no database or provider side effects.
package persontiers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/robfig/cron/v3"
)

const RuleVersion = 1

// Config separates semantic rules from operational scheduling switches. Start
// with DefaultConfig before decoding so explicit zero alternatives stay off.
type Config struct {
	Enabled                bool            `toml:"enabled" json:"-"`
	Apply                  bool            `toml:"apply" json:"-"`
	Schedule               string          `toml:"schedule" json:"-"`
	ShortWindowDays        int             `toml:"short_window_days" json:"short_window_days"`
	LongWindowDays         int             `toml:"long_window_days" json:"long_window_days"`
	DemotionGraceDays      int             `toml:"demotion_grace_days" json:"demotion_grace_days"`
	MinimumTier            int             `toml:"minimum_tier" json:"minimum_tier"`
	Tier1Lanes             []string        `toml:"tier1_lanes" json:"tier1_lanes"`
	Tier2Lanes             []string        `toml:"tier2_lanes" json:"tier2_lanes"`
	ExcludeListMessages    bool            `toml:"exclude_list_messages" json:"exclude_list_messages"`
	ExcludeAddressPatterns []string        `toml:"exclude_address_patterns" json:"exclude_address_patterns"`
	ExcludeDomainPatterns  []string        `toml:"exclude_domain_patterns" json:"exclude_domain_patterns"`
	PreviewRetentionDays   int             `toml:"preview_retention_days" json:"-"`
	Tier1                  Tier1Thresholds `toml:"tier1" json:"tier1"`
	Tier2                  Tier2Thresholds `toml:"tier2" json:"tier2"`
}

type Tier1Thresholds struct {
	MinOutboundShort          int `toml:"min_outbound_short" json:"min_outbound_short"`
	MinInboundShort           int `toml:"min_inbound_short" json:"min_inbound_short"`
	MinActiveDaysShort        int `toml:"min_active_days_short" json:"min_active_days_short"`
	BreadthMinActiveDaysShort int `toml:"breadth_min_active_days_short" json:"breadth_min_active_days_short"`
	BreadthMinChannelsLong    int `toml:"breadth_min_channels_long" json:"breadth_min_channels_long"`
	BreadthMinMeetingDaysLong int `toml:"breadth_min_meeting_days_long" json:"breadth_min_meeting_days_long"`
}

type Tier2Thresholds struct {
	MinOutboundLong    int `toml:"min_outbound_long" json:"min_outbound_long"`
	MinInboundLong     int `toml:"min_inbound_long" json:"min_inbound_long"`
	MinActiveDaysLong  int `toml:"min_active_days_long" json:"min_active_days_long"`
	MinMeetingDaysLong int `toml:"min_meeting_days_long" json:"min_meeting_days_long"`
}

func DefaultConfig() Config {
	return Config{
		Schedule: "45 1 * * *", ShortWindowDays: 90, LongWindowDays: 365,
		DemotionGraceDays: 30, MinimumTier: 3, PreviewRetentionDays: 90,
		Tier1Lanes: []string{"tracking", "briefs", "enrichment"}, Tier2Lanes: []string{"tracking"},
		ExcludeListMessages:    true,
		ExcludeAddressPatterns: []string{"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply", "notification", "notifications", "mailer-daemon", "postmaster", "bounce", "bounces", "newsletter", "digest", "alerts", "calendar-notification"},
		ExcludeDomainPatterns:  []string{"resource.calendar.google.com"},
		Tier1:                  Tier1Thresholds{2, 2, 6, 3, 2, 2}, Tier2: Tier2Thresholds{1, 1, 3, 2},
	}
}

func (c Config) Validate() error {
	if c.ShortWindowDays < 1 || c.LongWindowDays < c.ShortWindowDays || c.LongWindowDays > 3650 {
		return errors.New("people.tiers windows must satisfy 1 <= short_window_days <= long_window_days <= 3650")
	}
	if c.DemotionGraceDays < 0 || c.DemotionGraceDays > 365 || c.MinimumTier < 2 || c.MinimumTier > 3 || c.PreviewRetentionDays < 1 || c.PreviewRetentionDays > 3650 {
		return errors.New("people.tiers requires grace 0..365, minimum_tier 2..3 and preview_retention_days 1..3650")
	}
	for name, n := range map[string]int{"tier1.min_outbound_short": c.Tier1.MinOutboundShort, "tier1.min_inbound_short": c.Tier1.MinInboundShort, "tier1.min_active_days_short": c.Tier1.MinActiveDaysShort, "tier1.breadth_min_active_days_short": c.Tier1.BreadthMinActiveDaysShort, "tier2.min_outbound_long": c.Tier2.MinOutboundLong, "tier2.min_inbound_long": c.Tier2.MinInboundLong, "tier2.min_active_days_long": c.Tier2.MinActiveDaysLong} {
		if n < 1 {
			return fmt.Errorf("people.tiers.%s must be at least 1", name)
		}
	}
	if c.Tier1.BreadthMinChannelsLong != 0 && c.Tier1.BreadthMinChannelsLong < 2 {
		return errors.New("people.tiers.tier1.breadth_min_channels_long must be 0 or at least 2")
	}
	if c.Tier1.BreadthMinMeetingDaysLong < 0 || c.Tier2.MinMeetingDaysLong < 0 {
		return errors.New("people.tiers meeting thresholds must be 0 (disabled) or at least 1")
	}
	for i, lanes := range [][]string{c.Tier1Lanes, c.Tier2Lanes} {
		seen := map[string]bool{}
		for _, lane := range lanes {
			if seen[lane] || !slices.Contains([]string{"tracking", "briefs", "enrichment"}, lane) || (i == 1 && (lane == "enrichment" || !slices.Contains(c.Tier1Lanes, lane))) {
				return fmt.Errorf("people.tiers.tier%d_lanes has invalid, duplicate or non-subset lane %q", i+1, lane)
			}
			seen[lane] = true
		}
		if seen["briefs"] && !seen["tracking"] {
			return fmt.Errorf("people.tiers.tier%d_lanes: briefs requires tracking", i+1)
		}
	}
	for _, patterns := range [][]string{c.ExcludeAddressPatterns, c.ExcludeDomainPatterns} {
		for _, p := range patterns {
			if p == "" || p != strings.ToLower(p) || strings.ContainsAny(p, "@ \t\r\n") || strings.Contains(strings.TrimSuffix(p, "*"), "*") {
				return fmt.Errorf("people.tiers exclusion pattern %q must be nonempty lower-case with at most one trailing * and no @", p)
			}
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(c.Schedule); err != nil {
		return fmt.Errorf("people.tiers.schedule: %w", err)
	}
	return nil
}

// Fingerprint ignores operational switches, and normalizes unordered rule
// lists without changing the caller's config. Config has only JSON-safe fields.
func (c Config) Fingerprint() string {
	c.Tier1Lanes = sorted(c.Tier1Lanes)
	c.Tier2Lanes = sorted(c.Tier2Lanes)
	c.ExcludeAddressPatterns = sorted(c.ExcludeAddressPatterns)
	c.ExcludeDomainPatterns = sorted(c.ExcludeDomainPatterns)
	b, _ := json.Marshal(c, json.Deterministic(true))
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func sorted(values []string) []string { v := slices.Clone(values); slices.Sort(v); return v }

func matches(value string, patterns []string) bool {
	value = strings.ToLower(value)
	for _, p := range patterns {
		if value == p || (strings.HasSuffix(p, "*") && strings.HasPrefix(value, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}
