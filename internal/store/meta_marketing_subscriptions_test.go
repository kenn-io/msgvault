package store_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func marketingStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("facebook_messages", "app:9:page:100")
	require.NoError(t, err)
	return st, source.ID
}
func listedSubscription(t *testing.T, st *store.Store, source, walk int64, token string) store.MarketingSubscription {
	t.Helper()
	sub, err := st.UpsertListedSubscriptionContext(t.Context(), source, walk, store.ListedSubscription{Token: token, PSID: "300", TopicTitle: "Example offers", Expiry: time.Now().Add(48 * time.Hour), At: time.Now().UTC()})
	require.NoError(t, err)
	return sub
}
func TestUpsertListedCreatesActiveWithEvent(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src := marketingStore(t)
	w, err := st.StartListWalkContext(t.Context(), src, time.Now())
	requirements.NoError(err)
	sub := listedSubscription(t, st, src, w, "synthetic-mm-token-1")
	assertions.Equal("active", sub.Status)
	assertions.False(sub.LastConfirmedAt.Valid, "partial walk cannot establish freshness")
	again := listedSubscription(t, st, src, w, "synthetic-mm-token-1")
	assertions.Equal(sub.ID, again.ID)
	events, err := st.LatestConsentEventsContext(t.Context(), []int64{sub.ID})
	requirements.NoError(err)
	requirements.Len(events[sub.ID], 1)
	assertions.False(events[sub.ID][0].WalkComplete)
	_, err = st.FinishListWalkContext(t.Context(), w, "complete", 1, 1, "")
	requirements.NoError(err)
	got, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.True(got[0].LastConfirmedAt.Valid)
	events, err = st.LatestConsentEventsContext(t.Context(), []int64{sub.ID})
	requirements.NoError(err)
	assertions.True(events[sub.ID][0].WalkComplete)
}
func TestUpsertListedDoesNotReactivateStopped(t *testing.T) {
	for _, status := range []string{"stopped", "unsubscribed"} {
		t.Run(status, func(t *testing.T) {
			st, src := marketingStore(t)
			w, err := st.StartListWalkContext(t.Context(), src, time.Now())
			require.NoError(t, err)
			sub := listedSubscription(t, st, src, w, "synthetic-mm-token-1")
			require.NoError(t, st.SetSubscriptionStatusContext(t.Context(), sub.ID, status, "local_unsubscribe", "owner", "owner-1"))
			sub = listedSubscription(t, st, src, w, "synthetic-mm-token-1")
			assert.Equal(t, status, sub.Status)
		})
	}
}
func TestMarkUnlistedAbsentOnlyOlderWalk(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src := marketingStore(t)
	w, err := st.StartListWalkContext(t.Context(), src, time.Now())
	requirements.NoError(err)
	first := listedSubscription(t, st, src, w, "synthetic-mm-token-1")
	second := listedSubscription(t, st, src, w, "synthetic-mm-token-2")
	_, err = st.FinishListWalkContext(t.Context(), w, "complete", 1, 2, "")
	requirements.NoError(err)
	w2, err := st.StartListWalkContext(t.Context(), src, time.Now())
	requirements.NoError(err)
	listedSubscription(t, st, src, w2, "synthetic-mm-token-2")
	n, err := st.FinishListWalkContext(t.Context(), w2, "complete", 1, 1, "")
	requirements.NoError(err)
	assertions.Equal(1, n)
	got, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{Status: "absent"})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(first.ID, got[0].ID)
	events, err := st.LatestConsentEventsContext(t.Context(), []int64{first.ID, second.ID})
	requirements.NoError(err)
	assertions.Equal("absent", events[first.ID][0].Kind)
}
func TestConsentWebhookStopResumeIdempotent(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src := marketingStore(t)
	now := time.Now().UTC().Add(-time.Hour)
	in := store.ConsentWebhook{PSID: "300", Token: "synthetic-mm-token-1", At: now, EvidenceRef: "delivery-1:0"}
	sub, err := st.ApplyConsentWebhookContext(t.Context(), src, in)
	requirements.NoError(err)
	assertions.Equal("active", sub.Status)
	in.Status = "STOP NOTIFICATIONS"
	in.At = now.Add(time.Minute)
	in.EvidenceRef = "delivery-2:0"
	sub, err = st.ApplyConsentWebhookContext(t.Context(), src, in)
	requirements.NoError(err)
	assertions.Equal("stopped", sub.Status)
	_, err = st.ApplyConsentWebhookContext(t.Context(), src, in)
	requirements.NoError(err)
	events, err := st.LatestConsentEventsContext(t.Context(), []int64{sub.ID})
	requirements.NoError(err)
	requirements.Len(events[sub.ID], 2)
	in.Status = "RESUME NOTIFICATIONS"
	in.At = now.Add(2 * time.Minute)
	in.EvidenceRef = "delivery-3:0"
	sub, err = st.ApplyConsentWebhookContext(t.Context(), src, in)
	requirements.NoError(err)
	assertions.Equal("active", sub.Status)
	requirements.NoError(st.SetSubscriptionStatusContext(t.Context(), sub.ID, "unsubscribed", "local_unsubscribe", "owner", "owner-1"))
	in.EvidenceRef = "late-old-resume:0"
	sub, err = st.ApplyConsentWebhookContext(t.Context(), src, in)
	requirements.NoError(err)
	assertions.Equal("unsubscribed", sub.Status)
}
func TestConsentEventDetailHasNoToken(t *testing.T) {
	requirements := require.New(t)

	st, src := marketingStore(t)
	sub, err := st.ApplyConsentWebhookContext(t.Context(), src, store.ConsentWebhook{Token: "synthetic-mm-token-1", At: time.Now(), EvidenceRef: "delivery:0"})
	requirements.NoError(err)
	requirements.NoError(st.RecordConsentEventContext(t.Context(), sub.ID, "provider_unsubscribe_failed", "owner", "result-1", map[string]any{"token": "synthetic-mm-token-1", "nested": map[string]any{"message": "secret synthetic-mm-token-1"}}))
	rows, err := st.DB().Query("SELECT detail FROM meta_marketing_consent_events")
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var detail string
		requirements.NoError(rows.Scan(&detail))
		assert.NotContains(t, detail, "synthetic-mm-token-1")
	}
	requirements.NoError(rows.Err())
}
func TestFingerprintStable(t *testing.T) {
	sum := sha256.Sum256([]byte("synthetic-mm-token-1"))
	assert.Equal(t, hex.EncodeToString(sum[:])[:16], store.TokenFingerprint("synthetic-mm-token-1"))
}
func TestListWalkSingleFlight(t *testing.T) {
	requirements := require.New(t)

	st, src := marketingStore(t)
	now := time.Now().UTC()
	old, err := st.StartListWalkContext(t.Context(), src, now.Add(-61*time.Minute))
	requirements.NoError(err)
	_, err = st.StartListWalkContext(t.Context(), src, now)
	requirements.NoError(err)
	_, err = st.StartListWalkContext(t.Context(), src, now)
	requirements.ErrorIs(err, store.ErrWalkInProgress)
	var state string
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT state FROM meta_marketing_list_walks WHERE id=?"), old).Scan(&state))
	assert.Equal(t, "abandoned", state)
}
func TestPartialWalkCannotLaterEstablishAbsence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, src := marketingStore(t)
	w, err := st.StartListWalkContext(t.Context(), src, time.Now())
	requirements.NoError(err)
	sub := listedSubscription(t, st, src, w, "synthetic-partial-token")
	n, err := st.FinishListWalkContext(t.Context(), w, "partial", 1, 1, "rate_limited")
	requirements.NoError(err)
	assertions.Zero(n)
	_, err = st.FinishListWalkContext(t.Context(), w, "complete", 1, 1, "")
	requirements.Error(err)
	rows, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(sub.ID, rows[0].ID)
	assertions.Equal("active", rows[0].Status)
	assertions.False(rows[0].LastConfirmedAt.Valid)
}
func TestListWalkCreatesNoSyncRun(t *testing.T) {
	requirements := require.New(t)

	st, src := marketingStore(t)
	var before, after int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM sync_runs").Scan(&before))
	w, err := st.StartListWalkContext(t.Context(), src, time.Now())
	requirements.NoError(err)
	_, err = st.FinishListWalkContext(t.Context(), w, "complete", 1, 0, "")
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM sync_runs").Scan(&after))
	assert.Equal(t, before, after)
}
func TestMarketingSubscriptionConstraintsAndScope(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src := marketingStore(t)
	w, err := st.StartListWalkContext(t.Context(), src, time.Now())
	requirements.NoError(err)
	sub := listedSubscription(t, st, src, w, "synthetic-mm-token-1")
	var participant int64
	requirements.NoError(st.DB().QueryRow("INSERT INTO participants(email_address) VALUES ('subscriber@example.test') RETURNING id").Scan(&participant))
	requirements.NoError(st.SetSubscriptionParticipantContext(t.Context(), sub.ID, participant))
	at := time.Now().Add(24 * time.Hour).UTC()
	requirements.NoError(st.PushNextEligibleContext(t.Context(), sub.ID, at))
	got, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{ParticipantIDs: []int64{participant}})
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(sql.NullInt64{Int64: participant, Valid: true}, got[0].ParticipantID)
	assertions.WithinDuration(at, got[0].NextEligibleAt.Time, time.Second)
	_, err = st.UpsertListedSubscriptionContext(t.Context(), src+100, w, store.ListedSubscription{Token: "synthetic-mm-token-2"})
	requirements.Error(err)
	requirements.Error(st.SetSubscriptionParticipantContext(t.Context(), sub.ID, participant+100))
}

func TestMarkUnlistedPreservesConsentNewerThanWalk(t *testing.T) {
	for _, status := range []string{"", "RESUME NOTIFICATIONS"} {
		t.Run(status, func(t *testing.T) {
			requirements := require.New(t)
			st, src := marketingStore(t)
			start := time.Now().UTC().Add(-time.Minute)
			walk, err := st.StartListWalkContext(t.Context(), src, start)
			requirements.NoError(err)
			fresh, err := st.ApplyConsentWebhookContext(t.Context(), src, store.ConsentWebhook{Token: "synthetic-fresh-token", At: start.Add(time.Second), EvidenceRef: "fresh-during-walk", Status: status})
			requirements.NoError(err)
			old, err := st.ApplyConsentWebhookContext(t.Context(), src, store.ConsentWebhook{Token: "synthetic-old-token", At: start.Add(-time.Second), EvidenceRef: "old-before-walk", Status: status})
			requirements.NoError(err)
			n, err := st.FinishListWalkContext(t.Context(), walk, "complete", 1, 0, "")
			requirements.NoError(err)
			assert.Equal(t, 1, n)
			rows, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{Status: "active"})
			requirements.NoError(err)
			requirements.Len(rows, 1)
			assert.Equal(t, fresh.ID, rows[0].ID)
			rows, err = st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{Status: "absent"})
			requirements.NoError(err)
			requirements.Len(rows, 1)
			assert.Equal(t, old.ID, rows[0].ID)
		})
	}
}

func TestPartialListingCannotReusePreAbsenceConfirmation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, source := marketingStore(t)
	walk, err := st.StartListWalkContext(t.Context(), source, time.Now())
	requirements.NoError(err)
	sub := listedSubscription(t, st, source, walk, "synthetic-returning-token")
	_, err = st.FinishListWalkContext(t.Context(), walk, "complete", 1, 1, "")
	requirements.NoError(err)
	walk, err = st.StartListWalkContext(t.Context(), source, time.Now())
	requirements.NoError(err)
	_, err = st.FinishListWalkContext(t.Context(), walk, "complete", 1, 0, "")
	requirements.NoError(err)
	walk, err = st.StartListWalkContext(t.Context(), source, time.Now())
	requirements.NoError(err)
	sub = listedSubscription(t, st, source, walk, sub.Token)
	assertions.False(sub.LastConfirmedAt.Valid, "a partial reappearance cannot reuse confirmation from before absence")
	_, err = st.FinishListWalkContext(t.Context(), walk, "complete", 1, 1, "")
	requirements.NoError(err)
	rows, err := st.ListMarketingSubscriptionsContext(t.Context(), source, store.SubscriptionFilter{})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.True(rows[0].LastConfirmedAt.Valid)
}

func TestListingPreservesFrequencyCooldown(t *testing.T) {
	for _, delta := range []time.Duration{0, -time.Hour, time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, source := marketingStore(t)
			walk, err := st.StartListWalkContext(t.Context(), source, time.Now())
			requirements.NoError(err)
			sub := listedSubscription(t, st, source, walk, "synthetic-capped-token")
			cooldown := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
			requirements.NoError(st.PushNextEligibleContext(t.Context(), sub.ID, cooldown))
			var provider time.Time
			if delta != 0 {
				provider = cooldown.Add(delta)
			}
			sub, err = st.UpsertListedSubscriptionContext(t.Context(), source, walk, store.ListedSubscription{Token: sub.Token, NextEligible: provider})
			requirements.NoError(err)
			want := cooldown
			if provider.After(want) {
				want = provider
			}
			requirements.True(sub.NextEligibleAt.Valid)
			assertions.WithinDuration(want, sub.NextEligibleAt.Time, time.Microsecond)
		})
	}
}

func TestCompleteWalkReconcilesAbsenceAtomically(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, source := marketingStore(t)
	walk, err := st.StartListWalkContext(t.Context(), source, time.Now())
	requirements.NoError(err)
	old := listedSubscription(t, st, source, walk, "synthetic-omitted-token")
	_, err = st.FinishListWalkContext(t.Context(), walk, "complete", 1, 1, "")
	requirements.NoError(err)
	walk, err = st.StartListWalkContext(t.Context(), source, time.Now())
	requirements.NoError(err)
	fresh := listedSubscription(t, st, source, walk, "synthetic-present-token")
	triggerSQL := `CREATE TRIGGER fail_marketing_absence BEFORE INSERT ON meta_marketing_consent_events
 WHEN NEW.kind='absent' BEGIN SELECT RAISE(ABORT, 'injected absence failure'); END`
	dropSQL := `DROP TRIGGER fail_marketing_absence`
	if st.IsPostgreSQL() {
		triggerSQL = `CREATE FUNCTION fail_marketing_absence() RETURNS trigger AS $$
 BEGIN RAISE EXCEPTION 'injected absence failure'; END; $$ LANGUAGE plpgsql;
 CREATE TRIGGER fail_marketing_absence BEFORE INSERT ON meta_marketing_consent_events
 FOR EACH ROW WHEN (NEW.kind='absent') EXECUTE FUNCTION fail_marketing_absence()`
		dropSQL = `DROP TRIGGER fail_marketing_absence ON meta_marketing_consent_events; DROP FUNCTION fail_marketing_absence()`
	}
	_, err = st.DB().Exec(triggerSQL)
	requirements.NoError(err)
	_, err = st.FinishListWalkContext(t.Context(), walk, "complete", 1, 1, "")
	requirements.Error(err)
	var state string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT state FROM meta_marketing_list_walks WHERE id=?`), walk).Scan(&state))
	assertions.Equal("running", state)
	rows, err := st.ListMarketingSubscriptionsContext(t.Context(), source, store.SubscriptionFilter{})
	requirements.NoError(err)
	for _, sub := range rows {
		assertions.Equal("active", sub.Status)
		if sub.ID == fresh.ID {
			assertions.False(sub.LastConfirmedAt.Valid, "confirmation must roll back with failed absence reconciliation")
		}
	}
	_, err = st.DB().Exec(dropSQL)
	requirements.NoError(err)
	_, err = st.FinishListWalkContext(t.Context(), walk, "complete", 1, 1, "")
	requirements.NoError(err)
	rows, err = st.ListMarketingSubscriptionsContext(t.Context(), source, store.SubscriptionFilter{Status: "absent"})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(old.ID, rows[0].ID)
}
