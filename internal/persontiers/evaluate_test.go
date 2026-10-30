package persontiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every fixture is reachable: active days never exceed the messages and
// meeting days that could produce them, and channels count message channels.
func TestEvaluateThresholds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base      Signals
		set       func(*Signals, int)
		threshold int
		wantTier  int
		wantRule  string
	}{
		{"outbound short", Signals{InboundShort: 6, InboundLong: 6, ActiveDaysShort: 6, ActiveDaysLong: 6}, func(s *Signals, n int) { s.OutboundShort, s.OutboundLong = n, n }, 2, 1, ""},
		{"inbound short", Signals{OutboundShort: 6, OutboundLong: 6, ActiveDaysShort: 6, ActiveDaysLong: 6}, func(s *Signals, n int) { s.InboundShort, s.InboundLong = n, n }, 2, 1, ""},
		{"intensity days", Signals{OutboundShort: 4, OutboundLong: 4, InboundShort: 4, InboundLong: 4}, func(s *Signals, n int) { s.ActiveDaysShort, s.ActiveDaysLong = n, n }, 6, 1, ""},
		{"breadth days", Signals{OutboundShort: 2, OutboundLong: 2, InboundShort: 2, InboundLong: 2, ChannelsLong: 2}, func(s *Signals, n int) { s.ActiveDaysShort, s.ActiveDaysLong = n, n }, 3, 1, ""},
		{"breadth channels", Signals{OutboundShort: 2, OutboundLong: 2, InboundShort: 2, InboundLong: 2, ActiveDaysShort: 3, ActiveDaysLong: 3}, func(s *Signals, n int) { s.ChannelsLong = n }, 2, 1, ""},
		{"breadth meeting days", Signals{OutboundShort: 2, OutboundLong: 2, InboundShort: 2, InboundLong: 2, ActiveDaysShort: 3, ActiveDaysLong: 3}, func(s *Signals, n int) { s.MeetingDaysLong = n }, 2, 1, "tier1_breadth"},
		{"outbound long", Signals{InboundLong: 3, ActiveDaysLong: 3}, func(s *Signals, n int) { s.OutboundLong = n }, 1, 2, ""},
		{"inbound long", Signals{OutboundLong: 3, ActiveDaysLong: 3}, func(s *Signals, n int) { s.InboundLong = n }, 1, 2, ""},
		{"long days", Signals{OutboundLong: 2, InboundLong: 2}, func(s *Signals, n int) { s.ActiveDaysLong = n }, 3, 2, ""},
		{"meeting days", Signals{}, func(s *Signals, n int) { s.MeetingDaysLong, s.ActiveDaysLong = n, n }, 2, 2, "tier2_meetings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			for _, delta := range []int{-1, 0, 1} {
				s := tc.base
				tc.set(&s, tc.threshold+delta)
				d := Evaluate(s, DefaultConfig())
				if delta < 0 {
					assert.Greater(d.Tier, tc.wantTier)
					continue
				}
				assert.Equal(tc.wantTier, d.Tier)
				if tc.wantRule != "" {
					assert.Equal(tc.wantRule, d.Rule)
				}
			}
		})
	}
}

func TestExclusionsForceTierThree(t *testing.T) {
	checks := assert.New(t)
	cfg := DefaultConfig()
	cfg.ExcludeDomainPatterns = []string{"rooms.example.com"}
	// Two-way contact that would reach tier 2 without an exclusion.
	active := Signals{OutboundLong: 2, InboundLong: 2, ActiveDaysLong: 4}
	for _, tc := range []struct {
		name      string
		owner     bool
		emails    []string
		phone     bool
		tier      int
		exclusion string
	}{
		{"unbound saved", false, nil, false, 2, ""},
		{"owner", true, nil, false, 3, "owner"},
		{"automated", false, []string{"noreply@example.com", "mailer-daemon@example.org"}, false, 3, "automated_address"},
		{"mixed", false, []string{"noreply@example.com", "alex@example.org"}, false, 2, ""},
		{"phone defeats automated exclusion", false, []string{"noreply@example.com"}, true, 2, ""},
		{"room", false, []string{"room@rooms.example.com"}, false, 3, "calendar_room"},
		{"human plus room", false, []string{"room@rooms.example.com", "alex@example.com"}, false, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			s := active
			s.Owner, s.Emails, s.HasPhoneOrChat = tc.owner, tc.emails, tc.phone
			d := Evaluate(s, cfg)
			assert.Equal(tc.tier, d.Tier)
			assert.Equal(tc.exclusion, d.Exclusion)
			assert.Equal("tier2_two_way", d.Rule, "the matching rule stays visible for explain")
		})
	}
	cfg.ExcludeAddressPatterns = []string{"alerts*"}
	checks.Equal("automated_address", Evaluate(Signals{Emails: []string{"alerts-weekly@example.com"}}, cfg).Exclusion)
	checks.Empty(Evaluate(Signals{Emails: []string{"myalerts@example.com"}}, cfg).Exclusion)
}

func TestDisabledAlternatives(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tier1.BreadthMinChannelsLong = 0
	cfg.Tier1.BreadthMinMeetingDaysLong = 0
	cfg.Tier2.MinMeetingDaysLong = 0
	require.NoError(t, cfg.Validate())
	breadth := Evaluate(Signals{
		OutboundShort: 2, OutboundLong: 2, InboundShort: 2, InboundLong: 2,
		ActiveDaysShort: 3, ActiveDaysLong: 100, MeetingDaysLong: 100, ChannelsLong: 2,
	}, cfg)
	assert.Equal(t, "tier2_two_way", breadth.Rule, "channels and meeting days no longer make tier 1")
	meetings := Evaluate(Signals{ActiveDaysLong: 100, MeetingDaysLong: 100}, cfg)
	assert.Equal(t, 3, meetings.Tier)
}

func TestEvaluateReportsEveryThresholdCheck(t *testing.T) {
	assert := assert.New(t)
	d := Evaluate(Signals{OutboundShort: 1, OutboundLong: 1, InboundShort: 2, InboundLong: 2, ActiveDaysShort: 3, ActiveDaysLong: 3}, DefaultConfig())
	require.Len(t, d.Thresholds, 10)
	assert.Equal(ThresholdCheck{Name: "tier1.min_outbound_short", Observed: 1, Required: 2, Enabled: true}, d.Thresholds[0])
	assert.Equal(ThresholdCheck{Name: "tier1.min_inbound_short", Observed: 2, Required: 2, Enabled: true, Met: true}, d.Thresholds[1])
}
