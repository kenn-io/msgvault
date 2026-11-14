package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestClaimFreshness(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	until := now.Add(24 * time.Hour)
	tests := []struct {
		name   string
		target personfacts.TargetRef
		until  *time.Time
		kind   personfacts.FactKind
		days   int
	}{
		{"finite employment", personfacts.TargetRef{Kind: personfacts.TargetEmployment}, &until, personfacts.KindFact, 365},
		{"event", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-event"}, &until, personfacts.KindEvent, 7},
		{"custom", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-favorites-food"}, nil, personfacts.KindFact, 365},
		{"primary_channel", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "59e9a7d3-4904-4d0e-97d1-d0680e1e9e55"}, &until, personfacts.KindPreference, 90},          // gitleaks:allow -- public seeded attribute UUID
		{"ask_me_about", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "93c658a1-2346-4a6e-98c2-abfa29209334"}, &until, personfacts.KindPreference, 90},             // gitleaks:allow -- public seeded attribute UUID
		{"interests_fun_now", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "e8c815d5-3568-429b-a93b-00220b0a1b43"}, &until, personfacts.KindPreference, 90},        // gitleaks:allow -- public seeded attribute UUID
		{"interests_fun_growing_up", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "7ffd6a23-74cc-4e05-ad6b-7d765d883a94"}, &until, personfacts.KindPreference, 90}, // gitleaks:allow -- public seeded attribute UUID
		{"favorites_food", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "12612daa-90b0-461b-b928-79d4a2bc07ba"}, &until, personfacts.KindPreference, 90},           // gitleaks:allow -- public seeded attribute UUID
		{"favorites_place", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "be2bccc1-c395-4bb4-8f68-02e745ea7b22"}, &until, personfacts.KindPreference, 90},          // gitleaks:allow -- public seeded attribute UUID
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := assert.New(t)
			freshTime := now.Add(-time.Duration(tt.days) * 24 * time.Hour).Add(time.Nanosecond)
			metadata := describePersonFactFreshness(tt.target, tt.until, &freshTime, now)
			a.Equal(tt.kind, metadata.Kind)
			a.Equal(personfacts.StatusFresh, metadata.Status)
			boundary := freshTime.Add(-time.Nanosecond)
			a.Equal(personfacts.StatusStale, describePersonFactFreshness(tt.target, tt.until, &boundary, now).Status)
			a.Equal(personfacts.StatusStale, describePersonFactFreshness(tt.target, tt.until, nil, now).Status)
			a.Equal(personfacts.StatusExpired, describePersonFactFreshness(tt.target, &now, &freshTime, now).Status)
		})
	}
}
