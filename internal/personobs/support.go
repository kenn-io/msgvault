package personobs

import "go.kenn.io/msgvault/internal/personfacts"

// PremiseSupported deliberately ignores the semantic recheck queue.
func PremiseSupported(o Observation) bool {
	return o.Status == StatusActive && o.SupportFlag == SupportFlagNone && (o.Level != LevelExplicit || o.Origin == OriginOwner || o.SupportedCount > 0)
}
func DirectnessFor(level Level, origin Origin, a Attribution) personfacts.EvidenceDirectness {
	if level != LevelExplicit {
		return personfacts.Indirect
	}
	if origin == OriginOwner || a == AttributionReported {
		return personfacts.DirectOther
	}
	if a == AttributionSelf || a == AttributionAssent {
		return personfacts.DirectSelf
	}
	return personfacts.Indirect
}
