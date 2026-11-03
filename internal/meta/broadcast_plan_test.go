package meta

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

var planNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func planSubscription(id int64) store.MarketingSubscription {
	return store.MarketingSubscription{ID: id, Status: "active", Token: "synthetic-mm-token-1", PSID: sql.NullString{String: "300", Valid: true}, TokenExpiryAt: sql.NullTime{Time: planNow.Add(time.Hour), Valid: true}, LastConfirmedAt: sql.NullTime{Time: planNow.Add(-time.Hour), Valid: true}}
}
func planEvents(id int64) map[int64][]store.ConsentEvent {
	return map[int64][]store.ConsentEvent{id: {{Kind: "listed", WalkComplete: true, OccurredAt: planNow.Add(-time.Hour)}}}
}
func planPolicy() PlanPolicy {
	return PlanPolicy{MaxRecipients: 100, MinInterval: 24 * time.Hour, ConsentMaxAge: 24 * time.Hour}
}
func TestPlanRefusalReasons(t *testing.T) {
	for _, tt := range []struct {
		name, want    string
		status        string
		fresh, expiry time.Time
		events        []store.ConsentEvent
		last          time.Time
	}{
		{name: "eligible", status: "active", fresh: planNow.Add(-time.Hour), expiry: planNow.Add(time.Hour), events: planEvents(1)[1]},
		{name: "stopped", status: "stopped", want: "consent_stopped"},
		{name: "local unsubscribe", status: "unsubscribed", want: "consent_stopped"},
		{name: "absent", status: "absent", want: "consent_missing"},
		{name: "no evidence", status: "active", want: "consent_missing"},
		{name: "old complete walk", status: "active", fresh: planNow.Add(-25 * time.Hour), expiry: planNow.Add(time.Hour), events: planEvents(1)[1], want: "consent_stale"},
		{name: "no confirmation", status: "active", expiry: planNow.Add(time.Hour), events: planEvents(1)[1], want: "consent_stale"},
		{name: "expired", status: "active", fresh: planNow.Add(-time.Hour), expiry: planNow, events: planEvents(1)[1], want: "consent_expired"},
		{name: "expiry unknown", status: "active", fresh: planNow.Add(-time.Hour), events: planEvents(1)[1], want: "consent_expired"},
		{name: "recent send", status: "active", fresh: planNow.Add(-time.Hour), expiry: planNow.Add(time.Hour), events: planEvents(1)[1], last: planNow.Add(-23 * time.Hour), want: "frequency_cap"},
		{name: "interval boundary", status: "active", fresh: planNow.Add(-time.Hour), expiry: planNow.Add(time.Hour), events: planEvents(1)[1], last: planNow.Add(-24 * time.Hour)},
		{name: "stop after optin", status: "active", events: []store.ConsentEvent{{Kind: "optin", OccurredAt: planNow.Add(-2 * time.Hour)}, {Kind: "stop", OccurredAt: planNow.Add(-time.Hour)}}, want: "consent_stopped"},
		{name: "resume after stop", status: "active", fresh: planNow.Add(-time.Hour), expiry: planNow.Add(time.Hour), events: []store.ConsentEvent{{Kind: "stop", OccurredAt: planNow.Add(-2 * time.Hour)}, {Kind: "resume", OccurredAt: planNow.Add(-time.Hour)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sub := planSubscription(1)
			sub.Status = tt.status
			sub.LastConfirmedAt = sql.NullTime{Time: tt.fresh, Valid: !tt.fresh.IsZero()}
			sub.TokenExpiryAt = sql.NullTime{Time: tt.expiry, Valid: !tt.expiry.IsZero()}
			got := PlanRecipients(planNow, planPolicy(), []store.MarketingSubscription{sub}, map[int64][]store.ConsentEvent{1: tt.events}, map[int64]time.Time{1: tt.last})
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].Reason)
			assert.Equal(t, tt.want == "", got[0].Eligible)
		})
	}
}
func TestPlanStaleAfterPartialWalk(t *testing.T) {
	sub := planSubscription(1)
	sub.LastConfirmedAt.Time = planNow.Add(-25 * time.Hour)
	events := planEvents(1)
	events[1] = append(events[1], store.ConsentEvent{Kind: "listed", WalkComplete: false, OccurredAt: planNow})
	got := PlanRecipients(planNow, planPolicy(), []store.MarketingSubscription{sub}, events, nil)
	assert.Equal(t, "consent_stale", got[0].Reason)
	sub.LastConfirmedAt.Time = planNow
	events[1] = events[1][1:]
	got = PlanRecipients(planNow, planPolicy(), []store.MarketingSubscription{sub}, events, nil)
	assert.Equal(t, "consent_missing", got[0].Reason)
}
func TestPlanNextEligibleTimeHonoured(t *testing.T) {
	for _, offset := range []time.Duration{-time.Second, 0, time.Second} {
		sub := planSubscription(1)
		sub.NextEligibleAt = sql.NullTime{Time: planNow.Add(offset), Valid: true}
		got := PlanRecipients(planNow, planPolicy(), []store.MarketingSubscription{sub}, planEvents(1), nil)
		assert.Equal(t, offset <= 0, got[0].Eligible)
	}
}
func TestPlanOneSendPerParticipant(t *testing.T) {
	for _, withParticipant := range []bool{false, true} {
		a, b := planSubscription(1), planSubscription(2)
		if withParticipant {
			a.ParticipantID = sql.NullInt64{Int64: 50, Valid: true}
			b.ParticipantID = a.ParticipantID
			b.PSID.String = "301"
		}
		events := planEvents(1)
		events[2] = planEvents(2)[2]
		got := PlanRecipients(planNow, planPolicy(), []store.MarketingSubscription{b, a, a}, events, nil)
		require.Len(t, got, 2)
		assert.True(t, got[0].Eligible)
		assert.Equal(t, "duplicate", got[1].Reason)
	}
}
func TestPlanCapDeterministic(t *testing.T) {
	assertions := assert.New(t)

	var subs []store.MarketingSubscription
	events := map[int64][]store.ConsentEvent{}
	for _, id := range []int64{3, 1, 2} {
		sub := planSubscription(id)
		sub.PSID = sql.NullString{}
		subs = append(subs, sub)
		events[id] = planEvents(id)[id]
	}
	policy := planPolicy()
	policy.MaxRecipients = 2
	got := PlanRecipients(planNow, policy, subs, events, nil)
	require.Len(t, got, 3)
	assertions.Equal(int64(1), got[0].SubscriptionID)
	assertions.True(got[0].Eligible)
	assertions.True(got[1].Eligible)
	assertions.Equal("over_cap", got[2].Reason)
	subs[1].Status = "stopped"
	got = PlanRecipients(planNow, policy, subs, events, nil)
	assertions.Equal("consent_stopped", got[0].Reason)
	assertions.True(got[1].Eligible)
	assertions.True(got[2].Eligible)
}
func TestPlanDigestChangesOnAnyInput(t *testing.T) {
	assertions := assert.New(t)

	b := store.Broadcast{ID: "broadcast-example", SourceID: 1, AdAccountID: "700", Revision: 1, MessageSHA256: "hash-1", DailyBudgetCents: 500, CampaignDays: 7, MinGapSeconds: 0}
	d := []RecipientDecision{{SubscriptionID: 1, Eligible: true}, {SubscriptionID: 2, Reason: "consent_stopped"}}
	original := PlanDigest(b, d)
	assertions.Len(original, 64)
	assertions.Equal(original, PlanDigest(b, []RecipientDecision{d[1], d[0]}))
	for _, change := range []func(*store.Broadcast){func(b *store.Broadcast) { b.ID = "other" }, func(b *store.Broadcast) { b.SourceID++ }, func(b *store.Broadcast) { b.AdAccountID = "701" }, func(b *store.Broadcast) { b.Revision++ }, func(b *store.Broadcast) { b.MessageSHA256 = "hash-2" }, func(b *store.Broadcast) { b.DailyBudgetCents++ }, func(b *store.Broadcast) { b.CampaignDays++ }, func(b *store.Broadcast) { b.MinGapSeconds++ }} {
		edited := b
		change(&edited)
		assertions.NotEqual(original, PlanDigest(edited, d))
	}
	for _, change := range []func(*RecipientDecision){func(d *RecipientDecision) { d.SubscriptionID++ }, func(d *RecipientDecision) { d.ParticipantID++ }, func(d *RecipientDecision) { d.Eligible = false }, func(d *RecipientDecision) { d.Reason = "consent_stale" }} {
		edited := append([]RecipientDecision(nil), d...)
		change(&edited[0])
		assertions.NotEqual(original, PlanDigest(b, edited))
	}
}
