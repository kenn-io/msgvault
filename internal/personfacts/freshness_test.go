package personfacts

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimFreshness(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	until := now.Add(24 * time.Hour)
	tests := []struct {
		name   string
		target TargetRef
		until  *time.Time
		kind   FactKind
		days   int
	}{
		{"fact", TargetRef{Kind: TargetEmployment}, nil, KindFact, 365},
		{"event", TargetRef{Kind: TargetEmployment}, &until, KindEvent, 7},
		{"custom", TargetRef{Kind: TargetAttribute, Key: "custom-favorites-food"}, nil, KindFact, 365},
		{"preference", TargetRef{Kind: TargetAttribute, Key: "12612daa-90b0-461b-b928-79d4a2bc07ba"}, &until, KindPreference, 90}, // gitleaks:allow -- public seeded attribute UUID
		{"commitment", TargetRef{Kind: "commitment"}, &until, KindCommitment, 90},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			freshTime := now.Add(-time.Duration(tt.days) * 24 * time.Hour).Add(time.Nanosecond)
			metadata := DescribeFreshness(tt.target, tt.until, &freshTime, now)
			assert.Equal(t, tt.kind, metadata.Kind)
			assert.Equal(t, StatusFresh, metadata.Status)
			boundary := freshTime.Add(-time.Nanosecond)
			assert.Equal(t, StatusStale, DescribeFreshness(tt.target, tt.until, &boundary, now).Status)
			assert.Equal(t, StatusStale, DescribeFreshness(tt.target, tt.until, nil, now).Status)
			assert.Equal(t, StatusExpired, DescribeFreshness(tt.target, &now, &freshTime, now).Status)
		})
	}
}

func TestClaimReadMetadataDoesNotChangePacketJSON(t *testing.T) {
	claim := Claim{ID: 1}
	before, err := json.Marshal(claim)
	require.NoError(t, err)
	claim.Freshness = DescribeFreshness(claim.Target, nil, nil, time.Now())
	after, err := json.Marshal(claim)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}
