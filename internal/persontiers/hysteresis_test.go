package persontiers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHysteresis(t *testing.T) {
	cfg := DefaultConfig()
	start := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	prior := Prior{Tier: 1, TierSince: "2026-09-01", RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint()}
	d := Evaluate(Signals{}, cfg, prior, ManualState{}, start, true)
	assert.Equal(t, 1, d.Tier)
	assert.Equal(t, 3, d.CandidateTier)
	assert.Equal(t, "2026-10-17", d.CandidateSince)
	prior = d.AppliedPrior()
	known := Signals{OutboundLong: 1, InboundLong: 1, ActiveDaysLong: 3}
	d = Evaluate(known, cfg, prior, ManualState{}, start.AddDate(0, 0, 13), true)
	assert.Equal(t, 2, d.CandidateTier)
	assert.Equal(t, "2026-10-17", d.CandidateSince)
	prior = d.AppliedPrior()
	d = Evaluate(known, cfg, prior, ManualState{}, start.AddDate(0, 0, 29), true)
	assert.Equal(t, 1, d.Tier)
	d = Evaluate(known, cfg, prior, ManualState{}, start.AddDate(0, 0, 30), true)
	assert.Equal(t, 2, d.Tier)
	assert.Equal(t, "demotion", d.Reason)
	assert.Zero(t, d.CandidateTier)
	assert.Empty(t, d.CandidateSince)
	assert.Equal(t, "2026-11-16", d.TierSince)
}

func TestHysteresisDeferralsAndRecovery(t *testing.T) {
	cfg := DefaultConfig()
	asOf := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	prior := Prior{Tier: 1, TierSince: "2026-09-01", CandidateTier: 3, CandidateSince: "2026-10-17", RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint()}
	for _, tc := range []struct {
		name   string
		ready  bool
		manual ManualState
		want   string
	}{
		{"not ready", false, ManualState{}, "activity_not_ready"},
		{"manual run", true, ManualState{OpenManualRunID: "run-example"}, "manual_enrichment_run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(Signals{}, cfg, prior, tc.manual, asOf, tc.ready)
			assert.Equal(t, 1, d.Tier)
			assert.Equal(t, tc.want, d.DeferredReason)
			assert.Equal(t, 3, d.CandidateTier)
		})
	}
	close := Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6}
	d := Evaluate(close, cfg, prior, ManualState{}, asOf, false)
	assert.Equal(t, 1, d.Tier)
	assert.Zero(t, d.CandidateTier)
	assert.Empty(t, d.CandidateSince)
	assert.Empty(t, d.DeferredReason)
	d = Evaluate(close, cfg, Prior{Tier: 3, RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint()}, ManualState{}, asOf, false)
	assert.Equal(t, 1, d.Tier)
	assert.Equal(t, "promotion", d.Reason)
}

func TestHysteresisChangedPolicyAndCalendar(t *testing.T) {
	cfg := DefaultConfig()
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	asOf := time.Date(2026, 11, 16, 0, 0, 0, 0, zone)
	prior := Prior{Tier: 1, CandidateTier: 3, CandidateSince: "2026-10-17", RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint()}
	assert.Equal(t, 3, Evaluate(Signals{}, cfg, prior, ManualState{}, asOf, true).Tier)
	prior.RuleVersion = 0
	d := Evaluate(Signals{}, cfg, prior, ManualState{}, asOf, true)
	assert.Equal(t, 1, d.Tier)
	assert.Equal(t, "2026-11-16", d.CandidateSince)
	prior.RuleVersion = RuleVersion
	prior.ConfigFingerprint = "old-config"
	d = Evaluate(Signals{}, cfg, prior, ManualState{}, asOf, true)
	assert.Equal(t, 1, d.Tier)
	assert.Equal(t, "2026-11-16", d.CandidateSince)
	cfg.DemotionGraceDays = 0
	d = Evaluate(Signals{}, cfg, prior, ManualState{}, asOf, true)
	assert.Equal(t, 3, d.Tier)
	assert.Equal(t, "config_change", d.Reason)
}

func TestLanePlanManualPrecedence(t *testing.T) {
	cfg := DefaultConfig()
	s := Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6}
	m := ManualState{TrackingProvenance: "manual", BriefsProvenance: "manual", OptOuts: []string{"enrichment"}}
	d := Evaluate(s, cfg, Prior{}, m, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true)
	assert.Equal(t, "blocked_manual", d.Lanes["tracking"])
	assert.Equal(t, "blocked_manual", d.Lanes["briefs"])
	assert.Equal(t, "blocked_opt_out", d.Lanes["enrichment"])
	assert.False(t, d.EnrichmentEligible)
	m = ManualState{OptOuts: []string{"tracking"}}
	d = Evaluate(s, cfg, Prior{}, m, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true)
	assert.Equal(t, "blocked_opt_out", d.Lanes["tracking"])
	assert.Equal(t, "blocked_opt_out", d.Lanes["briefs"])
	assert.False(t, d.EnrichmentEligible)
	d = Evaluate(Signals{}, cfg, Prior{}, ManualState{TrackingProvenance: "manual"}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), true)
	assert.Equal(t, 3, d.Tier)
	assert.True(t, d.EnrichmentEligible)
}

func TestLanePlanRemovesAutoBriefsWithoutTracking(t *testing.T) {
	cfg := DefaultConfig()
	asOf := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	prior := Prior{Tier: 1, CandidateTier: 3, CandidateSince: "2026-10-17", RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint()}
	d := Evaluate(Signals{}, cfg, prior, ManualState{TrackingProvenance: "auto", BriefsProvenance: "auto"}, asOf, true)
	assert.Equal(t, 3, d.Tier)
	assert.Equal(t, "removed", d.Lanes["tracking"])
	assert.Equal(t, "removed", d.Lanes["briefs"])
	d = Evaluate(Signals{}, cfg, Prior{}, ManualState{}, asOf, true)
	assert.Equal(t, "unchanged", d.Lanes["briefs"])
}

func TestLaneConfigChangePreservesOpenManualTargetTracking(t *testing.T) {
	cfg := DefaultConfig()
	prior := Prior{Tier: 1, RuleVersion: RuleVersion, ConfigFingerprint: cfg.Fingerprint()}
	cfg.Tier1Lanes = []string{}
	cfg.Tier2Lanes = []string{}
	require.NoError(t, cfg.Validate())
	signals := Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6}
	m := ManualState{TrackingProvenance: "auto", BriefsProvenance: "auto", OpenManualRunID: "manual-example"}
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	d := Evaluate(signals, cfg, prior, m, asOf, true)
	assert.Equal(t, "kept", d.Lanes["tracking"])
	assert.Equal(t, "removed", d.Lanes["briefs"])
	assert.Equal(t, "manual_enrichment_run", d.DeferredReason)
	assert.False(t, d.EnrichmentEligible)
	m.OpenManualRunID = ""
	d = Evaluate(signals, cfg, prior, m, asOf, true)
	assert.Equal(t, "removed", d.Lanes["tracking"])
	assert.Empty(t, d.DeferredReason)
}
