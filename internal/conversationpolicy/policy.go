// Package conversationpolicy defines chat archival tiers independently of importers.
package conversationpolicy

import (
	"errors"
	"fmt"
	"time"
)

// Tier controls retention and downstream use of a conversation.
type Tier string

const (
	Full                      Tier = "full"
	Cold                      Tier = "cold"
	Stub                      Tier = "stub"
	Skip                      Tier = "skip"
	DefaultActivityWindowDays      = 180
)

// ParseTier accepts the exact configuration spelling of a tier.
func ParseTier(value string) (Tier, error) {
	switch tier := Tier(value); tier {
	case Full, Cold, Stub, Skip:
		return tier, nil
	}
	return "", fmt.Errorf("invalid conversation tier %q (want full, cold, stub, or skip)", value)
}

// Input contains provider membership and confirmed self activity. A zero
// LastActivity means no evidence. Future activity cannot establish eligibility.
// An explicit Override wins even when the automatic participant cap is disabled.
type Input struct {
	Participants    int
	KnownSize       bool
	LastActivity    time.Time
	MaxParticipants int
	ActivityWindow  time.Duration
	Now             time.Time
	Override        Tier
}

// Evaluate selects only full or cold automatically. Unknown rosters are cold
// unless activity or an explicit override admits them. The cutoff is inclusive.
func Evaluate(in Input) (Tier, error) {
	if in.Participants < 0 || in.MaxParticipants < 0 || in.ActivityWindow <= 0 || in.Now.IsZero() {
		return "", errors.New("invalid conversation policy input: counts must be nonnegative and window and time must be set")
	}
	if in.Override != "" {
		return ParseTier(string(in.Override))
	}
	active := !in.LastActivity.IsZero() && !in.LastActivity.After(in.Now) && !in.LastActivity.Before(in.Now.Add(-in.ActivityWindow))
	if in.MaxParticipants == 0 || (in.KnownSize && in.Participants <= in.MaxParticipants) || active {
		return Full, nil
	}
	return Cold, nil
}

// Override identifies one room by its source account and provider conversation
// ID. These stable keys avoid collisions across accounts and archive subsets.
type Override struct {
	SourceType       string `toml:"source_type" json:"source_type"`
	SourceIdentifier string `toml:"source_identifier" json:"source_identifier"`
	ConversationID   string `toml:"conversation_id" json:"conversation_id"`
	Tier             Tier   `toml:"tier" json:"tier"`
}
