package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestClaimFreshness(t *testing.T) {
	revision := "sha256:" + strings.Repeat("a", 64)
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
		{"expired employment", personfacts.TargetRef{Kind: personfacts.TargetEmployment, Key: "system:employment", Revision: revision}, &now, personfacts.KindFact, 365, personfacts.StatusExpired},
		{"finite employment", personfacts.TargetRef{Kind: personfacts.TargetEmployment, Key: "system:employment", Revision: revision}, &until, personfacts.KindFact, 365, personfacts.StatusFresh},
		{"event", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-event", Revision: revision}, &until, personfacts.KindEvent, 7, personfacts.StatusFresh},
		{"custom", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-favorites-food", Revision: revision}, nil, personfacts.KindFact, 365, personfacts.StatusFresh},
		{"contact_frequency", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDContactFrequency, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"primary_channel", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDPrimaryChannel, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"ask_me_about", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDAskMeAbout, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"interests_fun_now", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDInterestsFunNow, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"interests_fun_growing_up", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDInterestsFunGrowingUp, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"favorites_food", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDFavoritesFood, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
		{"favorites_place", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDFavoritesPlace, Revision: revision}, &until, personfacts.KindPreference, 90, personfacts.StatusFresh},
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
	for _, tt := range []struct {
		name   string
		target personfacts.TargetRef
		until  *time.Time
		days   int
	}{
		{"employment", personfacts.TargetRef{Kind: personfacts.TargetEmployment, Key: "system:employment", Revision: revision}, &until, 365},
		{"event", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-event", Revision: revision}, &until, 7},
		{"fact", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "custom-favorites-food", Revision: revision}, nil, 365},
		{"preference", personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: AttributeUniversalIDContactFrequency, Revision: revision}, &until, 90},
	} {
		t.Run(tt.name+" boundary", func(t *testing.T) {
			boundary := now.Add(-time.Duration(tt.days) * 24 * time.Hour)
			assert.Equal(t, personfacts.StatusStale, describePersonFactFreshness(tt.target, tt.until, &boundary, now).Status)
		})
	}
	assert.Equal(t, personfacts.StatusStale, describePersonFactFreshness(personfacts.TargetRef{Kind: personfacts.TargetEmployment, Key: "system:employment", Revision: revision}, &until, nil, now).Status)
}

func TestClaimFreshnessUsesSupportedEvidence(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("test", -12*60*60)
	t.Cleanup(func() { time.Local = local })
	r, a := require.New(t), assert.New(t)
	st, personID := newPersonFactLedgerStore(t)
	claim := personFactLedgerClaim(personID, "freshness", `"value"`, "freshness-old")
	old := time.Now().Add(-400 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	claim.Evidence[0].EventTime = old
	second := claim.Evidence[0]
	second.SourceRef = "freshness-new"
	second.EventTime = recent
	claim.Evidence = append(claim.Evidence, second)
	future := claim.Evidence[0]
	future.SourceRef = "freshness-future"
	future.EventTime = time.Now().Add(24 * time.Hour)
	claim.Evidence = append(claim.Evidence, future)
	prepared := preparePersonFactLedgerGeneration(t, personID, "freshness", []personfacts.ProposedClaim{claim}, nil)
	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	r.Len(stored.Claims, 1)
	page, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{Limit: 1})
	r.NoError(err)
	r.Len(page, 1)
	r.NotNil(page[0].Freshness.LatestSupportedAt)
	a.WithinDuration(recent, *page[0].Freshness.LatestSupportedAt, time.Microsecond)
	var newest personfacts.Evidence
	for _, e := range stored.Evidence {
		if e.Input.SourceRef == second.SourceRef {
			newest = e
		}
	}
	r.NotZero(newest.ID)
	change := personfacts.EvidenceStatusChange{EvidenceKey: newest.Key, SourceVersion: newest.Input.SourceVersion,
		Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}
	persistPersonFactLedgerGeneration(t, st, preparePersonFactLedgerGeneration(t, personID, "withdraw-new", nil,
		[]personfacts.EvidenceStatusChange{change}), nil)
	page, err = st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{Limit: 1})
	r.NoError(err)
	r.Len(page, 1)
	r.NotNil(page[0].Freshness.LatestSupportedAt)
	a.WithinDuration(old, *page[0].Freshness.LatestSupportedAt, time.Microsecond)
}
