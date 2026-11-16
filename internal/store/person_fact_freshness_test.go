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
		status personfacts.FreshnessStatus
	}{
		{"expired employment", personfacts.TargetRef{Kind: personfacts.TargetEmployment}, &now, personfacts.KindFact, 365, personfacts.StatusExpired},
		{"finite employment", personfacts.TargetRef{Kind: personfacts.TargetEmployment}, &until, personfacts.KindFact, 365, personfacts.StatusFresh},
		{"event", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-event"}, &until, personfacts.KindEvent, 7, personfacts.StatusFresh},
		{"custom", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-favorites-food"}, nil, personfacts.KindFact, 365, personfacts.StatusFresh},
		{"contact_frequency", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDContactFrequency}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"primary_channel", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDPrimaryChannel}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"ask_me_about", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDAskMeAbout}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"interests_fun_now", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDInterestsFunNow}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"interests_fun_growing_up", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDInterestsFunGrowingUp}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"favorites_food", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDFavoritesFood}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"favorites_place", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDFavoritesPlace}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := assert.New(t)
			freshTime := now.Add(-time.Duration(tt.days) * 24 * time.Hour).Add(time.Nanosecond)
			metadata := describePersonFactFreshness(tt.target, tt.until, &freshTime, now)
			a.Equal(tt.kind, metadata.Kind)
			a.Equal(tt.status, metadata.Status)
		})
	}
	boundary := now.Add(-365 * 24 * time.Hour)
	target := personfacts.TargetRef{Kind: personfacts.TargetEmployment}
	assert.Equal(t, personfacts.StatusStale, describePersonFactFreshness(target, &until, &boundary, now).Status)
	assert.Equal(t, personfacts.StatusStale, describePersonFactFreshness(target, &until, nil, now).Status)
}
