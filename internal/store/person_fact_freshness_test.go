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
		{"contact_frequency", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDContactFrequency}, &until, personfacts.KindPreference, 90},
		{"primary_channel", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDPrimaryChannel}, &until, personfacts.KindPreference, 90},
		{"ask_me_about", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDAskMeAbout}, &until, personfacts.KindPreference, 90},
		{"interests_fun_now", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDInterestsFunNow}, &until, personfacts.KindPreference, 90},
		{"interests_fun_growing_up", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDInterestsFunGrowingUp}, &until, personfacts.KindPreference, 90},
		{"favorites_food", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDFavoritesFood}, &until, personfacts.KindPreference, 90},
		{"favorites_place", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDFavoritesPlace}, &until, personfacts.KindPreference, 90},
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
