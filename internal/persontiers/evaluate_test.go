package persontiers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluateRules(t *testing.T) {
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		signals Signals
		tier    int
		rule    string
	}{
		{"intensity", Signals{OutboundShort: 14, InboundShort: 22, ActiveDaysShort: 19}, 1, "tier1_intensity"},
		{"chat only below intensity", Signals{OutboundShort: 9, InboundShort: 11, ActiveDaysShort: 5, OutboundLong: 9, InboundLong: 11, ActiveDaysLong: 17, ChannelsLong: 1}, 2, "tier2_two_way"},
		{"breadth channels", Signals{OutboundShort: 3, InboundShort: 2, ActiveDaysShort: 3, ChannelsLong: 2}, 1, "tier1_breadth"},
		{"breadth meetings", Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 3, MeetingDaysLong: 2}, 1, "tier1_breadth"},
		{"known", Signals{OutboundLong: 1, InboundLong: 2, ActiveDaysLong: 4}, 2, "tier2_two_way"},
		{"meetings only", Signals{MeetingDaysLong: 5}, 2, "tier2_meetings"},
		{"newsletter", Signals{ExcludedListEvents: 40}, 3, "none"},
		{"cold outreach", Signals{InboundShort: 3, InboundLong: 3, ActiveDaysShort: 3, ActiveDaysLong: 3}, 3, "none"},
		{"large group and two DMs", Signals{OutboundShort: 2, OutboundLong: 2, ActiveDaysShort: 2, ActiveDaysLong: 2, LargeAudienceInboundEvents: 58}, 3, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.signals, DefaultConfig(), Prior{}, ManualState{}, asOf, true)
			assert.Equal(t, tc.tier, d.ComputedTier)
			assert.Equal(t, tc.tier, d.Tier)
			assert.Equal(t, tc.rule, d.Rule)
		})
	}
}

func TestEvaluateThresholds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base      Signals
		set       func(*Signals, int)
		threshold int
		wantTier  int
	}{
		{"outbound short", Signals{InboundShort: 2, ActiveDaysShort: 6}, func(s *Signals, n int) { s.OutboundShort = n }, 2, 1},
		{"inbound short", Signals{OutboundShort: 2, ActiveDaysShort: 6}, func(s *Signals, n int) { s.InboundShort = n }, 2, 1},
		{"intensity days", Signals{OutboundShort: 2, InboundShort: 2}, func(s *Signals, n int) { s.ActiveDaysShort = n }, 6, 1},
		{"breadth days", Signals{OutboundShort: 2, InboundShort: 2, ChannelsLong: 2}, func(s *Signals, n int) { s.ActiveDaysShort = n }, 3, 1},
		{"breadth channels", Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 3}, func(s *Signals, n int) { s.ChannelsLong = n }, 2, 1},
		{"breadth meeting days", Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 3}, func(s *Signals, n int) { s.MeetingDaysLong = n }, 2, 1},
		{"outbound long", Signals{InboundLong: 1, ActiveDaysLong: 3}, func(s *Signals, n int) { s.OutboundLong = n }, 1, 2},
		{"inbound long", Signals{OutboundLong: 1, ActiveDaysLong: 3}, func(s *Signals, n int) { s.InboundLong = n }, 1, 2},
		{"long days", Signals{OutboundLong: 1, InboundLong: 1}, func(s *Signals, n int) { s.ActiveDaysLong = n }, 3, 2},
		{"meeting days", Signals{}, func(s *Signals, n int) { s.MeetingDaysLong = n }, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, delta := range []int{-1, 0, 1} {
				s := tc.base
				tc.set(&s, tc.threshold+delta)
				d := Evaluate(s, DefaultConfig(), Prior{}, ManualState{}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true)
				if delta < 0 {
					assert.Greater(t, d.ComputedTier, tc.wantTier)
				} else {
					assert.Equal(t, tc.wantTier, d.ComputedTier)
				}
			}
		})
	}
}

func TestExclusionsAndMinimum(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ExcludeDomainPatterns = []string{"rooms.example.com"}
	cfg.MinimumTier = 2
	for _, tc := range []struct {
		name string
		s    Signals
		tier int
		rule string
	}{
		{"unbound saved", Signals{}, 2, "minimum_tier"},
		{"owner", Signals{Owner: true}, 3, "owner"},
		{"automated", Signals{Emails: []string{"noreply@example.com", "mailer-daemon@example.org"}}, 3, "automated_address"},
		{"mixed", Signals{Emails: []string{"noreply@example.com", "alex@example.org"}}, 2, "minimum_tier"},
		{"phone defeats automated exclusion", Signals{Emails: []string{"noreply@example.com"}, HasPhoneOrChat: true}, 2, "minimum_tier"},
		{"phone only", Signals{HasPhoneOrChat: true}, 2, "minimum_tier"},
		{"room", Signals{Emails: []string{"room@rooms.example.com"}}, 3, "calendar_room"},
		{"human plus room", Signals{Emails: []string{"room@rooms.example.com", "alex@example.com"}}, 2, "minimum_tier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.s, cfg, Prior{}, ManualState{}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true)
			assert.Equal(t, tc.tier, d.ComputedTier)
			assert.Equal(t, tc.rule, d.Rule)
		})
	}
	cfg.ExcludeAddressPatterns = []string{"alerts*"}
	assert.Equal(t, "automated_address", Evaluate(Signals{Emails: []string{"ALERTS-weekly@example.com"}}, cfg, Prior{}, ManualState{}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true).Rule)
	assert.Equal(t, "minimum_tier", Evaluate(Signals{Emails: []string{"myalerts@example.com"}}, cfg, Prior{}, ManualState{}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true).Rule)
}

func TestDisabledAlternatives(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tier1.BreadthMinChannelsLong = 0
	cfg.Tier1.BreadthMinMeetingDaysLong = 0
	cfg.Tier2.MinMeetingDaysLong = 0
	require.NoError(t, cfg.Validate())
	d := Evaluate(Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 3, MeetingDaysLong: 100, ChannelsLong: 100}, cfg, Prior{}, ManualState{}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true)
	assert.Equal(t, 3, d.ComputedTier)
}

func TestLaneConfigChangePreservesActivityDeferralWithOpenManualTarget(t *testing.T) {
	cfg := DefaultConfig()
	// Zero grace makes the demotion due even when a new lane configuration
	// restarts its candidate date. Readiness remains the first deferral.
	cfg.DemotionGraceDays = 0
	prior := Prior{
		Tier: 1, CandidateTier: 3, CandidateSince: "2026-09-01",
		RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint(),
	}
	cfg.Tier1Lanes = []string{}
	cfg.Tier2Lanes = []string{}
	require.NoError(t, cfg.Validate())
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	manual := ManualState{TrackingProvenance: "auto", OpenManualRunID: "synthetic-manual-run"}

	decision := Evaluate(Signals{}, cfg, prior, manual, asOf, false)

	assert.Equal(t, 1, decision.Tier)
	assert.Equal(t, 3, decision.ComputedTier)
	assert.Equal(t, "2026-10-17", decision.DemotionDue)
	assert.Equal(t, "kept", decision.Lanes["tracking"])
	assert.Equal(t, "activity_not_ready", decision.DeferredReason)
}
