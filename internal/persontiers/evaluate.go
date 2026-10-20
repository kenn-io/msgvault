package persontiers

import "strings"

// Signals counts only events already filtered by the activity rules. Owner,
// Emails and HasPhoneOrChat feed the exclusions and stay out of the output.
type Signals struct {
	OutboundShort              int `json:"outbound_short"`
	OutboundLong               int `json:"outbound_long"`
	InboundShort               int `json:"inbound_short"`
	InboundLong                int `json:"inbound_long"`
	ActiveDaysShort            int `json:"active_days_short"`
	ActiveDaysLong             int `json:"active_days_long"`
	MeetingDaysLong            int `json:"meeting_days_long"`
	ChannelsLong               int `json:"channels_long"`
	ExcludedListEvents         int `json:"excluded_list_events"`
	LargeAudienceInboundEvents int `json:"large_audience_inbound_events"`
	// LastDate is the latest local date of a counted event, or empty. Previews and
	// explanations report it as last_contact_date.
	LastDate       string   `json:"-"`
	Owner          bool     `json:"-"`
	Emails         []string `json:"-"`
	HasPhoneOrChat bool     `json:"-"`
}

// ThresholdCheck compares one count with its configured threshold. A zero
// threshold is disabled and never met.
type ThresholdCheck struct {
	Name     string `json:"name"`
	Observed int    `json:"observed"`
	Required int    `json:"required"`
	Enabled  bool   `json:"enabled"`
	Met      bool   `json:"met"`
}

// Decision is the tier for one person and the rule behind it. Rule names the
// matching rule even when an exclusion then forces tier 3, and is "none" when
// no rule matched.
type Decision struct {
	Tier       int              `json:"tier"`
	Rule       string           `json:"rule"`
	Exclusion  string           `json:"exclusion"`
	Thresholds []ThresholdCheck `json:"thresholds"`
}

// Evaluate is pure. Callers validate the config first.
func Evaluate(s Signals, c Config) Decision {
	d := Decision{Tier: 3, Rule: "none"}
	check := func(name string, got, want int) bool {
		enabled := want > 0
		met := enabled && got >= want
		d.Thresholds = append(d.Thresholds, ThresholdCheck{name, got, want, enabled, met})
		return met
	}
	shortOut := check("tier1.min_outbound_short", s.OutboundShort, c.Tier1.MinOutboundShort)
	shortIn := check("tier1.min_inbound_short", s.InboundShort, c.Tier1.MinInboundShort)
	intensity := check("tier1.min_active_days_short", s.ActiveDaysShort, c.Tier1.MinActiveDaysShort)
	breadthDays := check("tier1.breadth_min_active_days_short", s.ActiveDaysShort, c.Tier1.BreadthMinActiveDaysShort)
	breadthChannels := check("tier1.breadth_min_channels_long", s.ChannelsLong, c.Tier1.BreadthMinChannelsLong)
	breadthMeetings := check("tier1.breadth_min_meeting_days_long", s.MeetingDaysLong, c.Tier1.BreadthMinMeetingDaysLong)
	longOut := check("tier2.min_outbound_long", s.OutboundLong, c.Tier2.MinOutboundLong)
	longIn := check("tier2.min_inbound_long", s.InboundLong, c.Tier2.MinInboundLong)
	longDays := check("tier2.min_active_days_long", s.ActiveDaysLong, c.Tier2.MinActiveDaysLong)
	meetings := check("tier2.min_meeting_days_long", s.MeetingDaysLong, c.Tier2.MinMeetingDaysLong)
	switch {
	case shortOut && shortIn && intensity:
		d.Tier, d.Rule = 1, "tier1_intensity"
	case shortOut && shortIn && breadthDays && (breadthChannels || breadthMeetings):
		d.Tier, d.Rule = 1, "tier1_breadth"
	case longOut && longIn && longDays:
		d.Tier, d.Rule = 2, "tier2_two_way"
	case meetings:
		d.Tier, d.Rule = 2, "tier2_meetings"
	}
	if d.Exclusion = excluded(s, c); d.Exclusion != "" {
		d.Tier = 3
	}
	return d
}

func excluded(s Signals, c Config) string {
	if s.Owner {
		return "owner"
	}
	if len(s.Emails) == 0 {
		return ""
	}
	automated, rooms := !s.HasPhoneOrChat, true
	for _, email := range s.Emails {
		local, domain, ok := strings.Cut(email, "@")
		automated = automated && ok && matches(local, c.ExcludeAddressPatterns)
		rooms = rooms && ok && matches(domain, c.ExcludeDomainPatterns)
	}
	if rooms {
		return "calendar_room"
	}
	if automated {
		return "automated_address"
	}
	return ""
}
