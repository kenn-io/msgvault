package personfacts

import "time"

// FactKind classifies immutable claim identity, independently of model wording.
type FactKind string

type FreshnessStatus string

const (
	KindFact       FactKind        = "fact"
	KindEvent      FactKind        = "event"
	KindPreference FactKind        = "preference"
	KindCommitment FactKind        = "commitment"
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
	case target.Kind == "commitment":
		kind, days = KindCommitment, 90
	case target.Kind == TargetAttribute && seededPreference(target.Key):
		kind, days = KindPreference, 90
	case validUntil != nil:
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
	case "59e9a7d3-4904-4d0e-97d1-d0680e1e9e55", // primary_channel
		"93c658a1-2346-4a6e-98c2-abfa29209334", // ask_me_about
		"e8c815d5-3568-429b-a93b-00220b0a1b43", // interests_fun_now
		"7ffd6a23-74cc-4e05-ad6b-7d765d883a94", // interests_fun_growing_up
		"12612daa-90b0-461b-b928-79d4a2bc07ba", // favorites_food
		"be2bccc1-c395-4bb4-8f68-02e745ea7b22": // favorites_place
		return true
	}
	return false
}
