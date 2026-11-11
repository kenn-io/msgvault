package personfacts

import "time"

// FactKind classifies immutable claim identity, independently of model wording.
type FactKind string

type FreshnessStatus string

const (
	KindFact       FactKind        = "fact"
	KindEvent      FactKind        = "event"
	KindCommitment FactKind        = "commitment"
	KindPreference FactKind        = "preference"
	StatusFresh    FreshnessStatus = "fresh"
	StatusStale    FreshnessStatus = "stale"
	StatusExpired  FreshnessStatus = "expired"
)

// Freshness is read metadata. It is never included in inference packets or keys.
type Freshness struct {
	Kind              FactKind
	Status            FreshnessStatus
	LatestSupportedAt *time.Time
}

// DescribeFreshness uses the newest currently supported evidence, not the date
// of extraction. Expiry takes precedence, including at the exact boundary.
func DescribeFreshness(target TargetRef, validUntil, latestSupported *time.Time, now time.Time) Freshness {
	kind := KindFact
	days := 365
	switch {
	case target.Kind == TargetCommitment:
		kind, days = KindCommitment, 90
	case target.Kind == TargetAttribute && seededPreference(target.Key):
		kind, days = KindPreference, 90
	case validUntil != nil && target.Kind != TargetEmployment:
		kind, days = KindEvent, 7
	}
	result := Freshness{Kind: kind, Status: StatusStale, LatestSupportedAt: latestSupported}
	if validUntil != nil && !now.Before(*validUntil) {
		result.Status = StatusExpired
	} else if latestSupported != nil && now.Before(latestSupported.Add(time.Duration(days)*24*time.Hour)) {
		result.Status = StatusFresh
	}
	return result
}

// These IDs belong to the shipped preference categories. Names, slugs and
// revisions can change without reclassifying historical claims.
func seededPreference(key string) bool {
	switch key {
	case AttributeUniversalIDPrimaryChannel, // primary_channel
		AttributeUniversalIDAskMeAbout,            // ask_me_about
		AttributeUniversalIDInterestsFunNow,       // interests_fun_now
		AttributeUniversalIDInterestsFunGrowingUp, // interests_fun_growing_up
		AttributeUniversalIDFavoritesFood,         // favorites_food
		AttributeUniversalIDFavoritesPlace:        // favorites_place
		return true
	}
	return false
}
