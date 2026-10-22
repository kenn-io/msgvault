package persontiers

import (
	"slices"
	"strings"
	"time"
)

// Signals counts only events already filtered by the activity rules. Emails
// and identifier flags are ephemeral exclusion inputs, never audit content.
type Signals struct {
	OutboundShort              int        `json:"outbound_short"`
	OutboundLong               int        `json:"outbound_long"`
	InboundShort               int        `json:"inbound_short"`
	InboundLong                int        `json:"inbound_long"`
	ActiveDaysShort            int        `json:"active_days_short"`
	ActiveDaysLong             int        `json:"active_days_long"`
	MeetingDaysLong            int        `json:"meeting_days_long"`
	ChannelsLong               int        `json:"channels_long"`
	LastDirectAt               *time.Time `json:"last_direct_at"`
	ExcludedListEvents         int        `json:"excluded_list_events"`
	LargeAudienceInboundEvents int        `json:"large_audience_inbound_events"`
	CoPresenceEvents           int        `json:"co_presence_events"`
	ObservedEvents             int        `json:"observed_events"`
	Owner                      bool       `json:"-"`
	Emails                     []string   `json:"-"`
	HasPhoneOrChat             bool       `json:"-"`
}

// Prior is applied authority. Observational shadow state is evaluated through
// a separate Prior; it must never seed the applied evaluator.
type Prior struct {
	Tier              int    `json:"tier"`
	TierSince         string `json:"tier_since"`
	CandidateTier     int    `json:"candidate_tier"`
	CandidateSince    string `json:"candidate_since"`
	RuleVersion       int    `json:"rule_version"`
	ConfigFingerprint string `json:"config_fingerprint"`
}

type ManualState struct {
	TrackingProvenance string   `json:"tracking_provenance"`
	BriefsProvenance   string   `json:"briefs_provenance"`
	OptOuts            []string `json:"opt_outs"`
	OpenManualRunID    string   `json:"open_manual_run_id"`
}

type ThresholdCheck struct {
	Name     string `json:"name"`
	Observed int    `json:"observed"`
	Required int    `json:"required"`
	Enabled  bool   `json:"enabled"`
	Met      bool   `json:"met"`
}

type Decision struct {
	Prior
	ComputedTier       int               `json:"computed_tier"`
	Rule               string            `json:"rule"`
	Reason             string            `json:"reason"`
	DeferredReason     string            `json:"deferred_reason"`
	DemotionDue        string            `json:"demotion_due"`
	ActivityReady      bool              `json:"activity_ready"`
	Lanes              map[string]string `json:"lanes"`
	AppliedLanes       []string          `json:"applied_lanes"`
	EnrichmentEligible bool              `json:"enrichment_eligible"`
	Thresholds         []ThresholdCheck  `json:"thresholds"`
}

// AppliedPrior returns only hysteresis authority, without explanation fields.
func (d Decision) AppliedPrior() Prior { return d.Prior }

// Evaluate is pure: callers validate config and provide asOf in the projector's
// timezone. It plans metadata changes; Store must recheck manual state and
// stale-run fences inside the apply transaction.
func Evaluate(s Signals, c Config, prior Prior, m ManualState, asOf time.Time, ready bool) Decision {
	d := Decision{Prior: prior, ComputedTier: 3, Rule: "none", ActivityReady: ready, Lanes: map[string]string{}}
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
		d.ComputedTier = 1
		d.Rule = "tier1_intensity"
	case shortOut && shortIn && breadthDays && (breadthChannels || breadthMeetings):
		d.ComputedTier = 1
		d.Rule = "tier1_breadth"
	case longOut && longIn && longDays:
		d.ComputedTier = 2
		d.Rule = "tier2_two_way"
	case meetings:
		d.ComputedTier = 2
		d.Rule = "tier2_meetings"
	}
	if d.ComputedTier > c.MinimumTier {
		d.ComputedTier = c.MinimumTier
		d.Rule = "minimum_tier"
	}
	if exclusion := excluded(s, c); exclusion != "" {
		d.ComputedTier = 3
		d.Rule = exclusion
	}
	d.transition(c, prior, m, asOf, ready)
	if d.Tier == 1 {
		d.AppliedLanes = slices.Clone(c.Tier1Lanes)
	} else if d.Tier == 2 {
		d.AppliedLanes = slices.Clone(c.Tier2Lanes)
	} else {
		d.AppliedLanes = []string{}
	}
	tracking := m.TrackingProvenance == "manual" || (slices.Contains(d.AppliedLanes, "tracking") && !slices.Contains(m.OptOuts, "tracking"))
	// A lane edit can remove tracking without changing the tier. Preserve an
	// existing auto row for exact human-requested work on that path too.
	holdTracking := !tracking && m.TrackingProvenance == "auto" && m.OpenManualRunID != ""
	if holdTracking && d.DeferredReason == "" {
		d.DeferredReason = "manual_enrichment_run"
	}
	for _, lane := range []string{"tracking", "briefs"} {
		provenance := m.TrackingProvenance
		if lane == "briefs" {
			provenance = m.BriefsProvenance
		}
		wanted := slices.Contains(d.AppliedLanes, lane)
		blocked := slices.Contains(m.OptOuts, lane) || (lane == "briefs" && wanted && !tracking)
		switch {
		case provenance == "manual":
			d.Lanes[lane] = "blocked_manual"
		case lane == "tracking" && holdTracking:
			d.Lanes[lane] = "kept"
		case provenance == "auto" && (!wanted || blocked):
			d.Lanes[lane] = "removed"
		case blocked:
			d.Lanes[lane] = "blocked_opt_out"
		case wanted && provenance == "auto":
			d.Lanes[lane] = "kept"
		case wanted:
			d.Lanes[lane] = "created"
		case provenance == "auto":
			d.Lanes[lane] = "removed"
		default:
			d.Lanes[lane] = "unchanged"
		}
	}
	d.EnrichmentEligible = tracking && !slices.Contains(m.OptOuts, "enrichment") && (m.TrackingProvenance == "manual" || slices.Contains(d.AppliedLanes, "enrichment"))
	d.Lanes["enrichment"] = "unchanged"
	if slices.Contains(m.OptOuts, "enrichment") {
		d.Lanes["enrichment"] = "blocked_opt_out"
	} else if d.EnrichmentEligible {
		d.Lanes["enrichment"] = "eligible"
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
