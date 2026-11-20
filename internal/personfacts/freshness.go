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
