package persontiers

import "time"

func rank(tier int) int { return 4 - tier }

func (d *Decision) transition(c Config, p Prior, m ManualState, asOf time.Time, ready bool) {
	today := asOf.Format(time.DateOnly)
	if d.Tier == 0 {
		d.Tier = 3
		d.TierSince = today
	}
	changed := p.Tier != 0 && (p.RuleVersion != RuleVersion || p.ConfigFingerprint != c.Fingerprint())
	d.RuleVersion = RuleVersion
	d.ConfigFingerprint = c.Fingerprint()
	clearCandidate := func() { d.CandidateTier = 0; d.CandidateSince = "" }
	switch {
	case rank(d.ComputedTier) > rank(d.Tier):
		d.Tier = d.ComputedTier
		d.TierSince = today
		clearCandidate()
		d.Reason = "promotion"
	case d.ComputedTier == d.Tier:
		clearCandidate()
	default:
		if d.CandidateTier == 0 || changed {
			d.CandidateSince = today
		}
		d.CandidateTier = d.ComputedTier
		since, err := time.ParseInLocation(time.DateOnly, d.CandidateSince, asOf.Location())
		if err != nil {
			since = time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, asOf.Location())
			d.CandidateSince = today
		}
		due := since.AddDate(0, 0, c.DemotionGraceDays)
		d.DemotionDue = due.Format(time.DateOnly)
		if today >= d.DemotionDue {
			switch {
			case !ready:
				d.DeferredReason = "activity_not_ready"
			case m.OpenManualRunID != "":
				d.DeferredReason = "manual_enrichment_run"
			default:
				d.Tier = d.CandidateTier
				d.TierSince = today
				clearCandidate()
				d.DemotionDue = ""
				d.Reason = "demotion"
			}
		}
	}
	if d.Reason != "" && changed {
		d.Reason = "config_change"
	}
}
