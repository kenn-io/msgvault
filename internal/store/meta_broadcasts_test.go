package store_test

import (
	"database/sql"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func broadcastFixture(t *testing.T) (*store.Store, store.Broadcast, []store.MarketingSubscription) {
	t.Helper()
	st, src := marketingStore(t)
	w, err := st.StartListWalkContext(t.Context(), src, time.Now())
	require.NoError(t, err)
	subs := []store.MarketingSubscription{listedSubscription(t, st, src, w, "synthetic-mm-token-1"), listedSubscription(t, st, src, w, "synthetic-mm-token-2")}
	b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: src, AdAccountID: "700", Name: "Example offers", MessageJSON: []byte(`{"text":"Example"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{subs[0].ID, subs[1].ID})
	require.NoError(t, err)
	return st, b, subs
}
func planBroadcast(t *testing.T, st *store.Store, b store.Broadcast, subs []store.MarketingSubscription) []store.BroadcastRecipient {
	t.Helper()
	decisions := []store.RecipientDecisionRow{}
	for _, sub := range subs {
		decisions = append(decisions, store.RecipientDecisionRow{SubscriptionID: sub.ID, Eligible: true})
	}
	require.NoError(t, st.SavePlanContext(t.Context(), b.ID, b.Revision, "digest-1", decisions))
	_, rows, err := st.GetBroadcastContext(t.Context(), b.ID)
	require.NoError(t, err)
	return rows
}
func readyBroadcast(t *testing.T, st *store.Store, b store.Broadcast, subs []store.MarketingSubscription) []store.BroadcastRecipient {
	t.Helper()
	rows := planBroadcast(t, st, b, subs)
	require.NoError(t, st.TransitionBroadcastContext(t.Context(), b.ID, "planned", "campaign_pending", ""))
	require.NoError(t, st.SetCampaignContext(t.Context(), b.ID, "55", time.Now().Add(-time.Minute), time.Now().Add(7*24*time.Hour), false))
	return rows
}
func acceptRecipient(t *testing.T, st *store.Store, b store.Broadcast, row store.BroadcastRecipient, tracking string) {
	t.Helper()
	ok, err := st.ClaimRecipientContext(t.Context(), b.ID, row.SubscriptionID, row.RowRevision, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, st.FinishRecipientContext(t.Context(), b.ID, row.SubscriptionID, "accepted", "", "", tracking))
}
func TestCreateBroadcastStableIDAndRevision(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, _ := broadcastFixture(t)
	assertions.Equal(int64(1), b.Revision)
	assertions.Equal("draft", b.State)
	got, rows, err := st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal(b.ID, got.ID)
	assertions.Len(rows, 2)
	assertions.NotEmpty(got.MessageSHA256)
	list, err := st.ListBroadcastsContext(t.Context(), b.SourceID)
	requirements.NoError(err)
	requirements.Len(list, 1)
}
func TestBroadcastIDFormat(t *testing.T) {
	_, b, _ := broadcastFixture(t)
	assert.Regexp(t, `^broadcast-[0-9a-f]{32}$`, b.ID)
}
func TestUpdateMessageClearsPlanAndBumpsRevision(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	planBroadcast(t, st, b, subs)
	edited, err := st.UpdateBroadcastMessageContext(t.Context(), b.ID, 1, []byte(`{"text":"Edited"}`))
	requirements.NoError(err)
	assertions.Equal(int64(2), edited.Revision)
	assertions.Empty(edited.PlanDigest)
	assertions.Equal("draft", edited.State)
	assertions.NotEqual(b.MessageSHA256, edited.MessageSHA256)
	_, err = st.UpdateBroadcastMessageContext(t.Context(), b.ID, 1, []byte(`{}`))
	requirements.Error(err)
}
func TestUpdateRefusedAfterSending(t *testing.T) {
	st, b, subs := broadcastFixture(t)
	readyBroadcast(t, st, b, subs)
	_, err := st.UpdateBroadcastMessageContext(t.Context(), b.ID, 1, []byte(`{}`))
	require.ErrorIs(t, err, store.ErrBroadcastFrozen)
}
func TestClaimRecipientSingleWinner(t *testing.T) {
	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	var wg sync.WaitGroup
	wins := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			ok, err := st.ClaimRecipientContext(t.Context(), b.ID, rows[0].SubscriptionID, rows[0].RowRevision, time.Now())
			wins <- ok
			errs <- err
		})
	}
	wg.Wait()
	close(wins)
	close(errs)
	n := 0
	for ok := range wins {
		if ok {
			n++
		}
	}
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, n)
}
func TestExpireAbandonedClaims(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	ok, err := st.ClaimRecipientContext(t.Context(), b.ID, rows[0].SubscriptionID, rows[0].RowRevision, time.Now().Add(-3*time.Minute))
	requirements.NoError(err)
	requirements.True(ok)
	n, err := st.ExpireAbandonedClaimsContext(t.Context(), b.ID, time.Now().Add(-2*time.Minute))
	requirements.NoError(err)
	assertions.Equal(1, n)
	_, rows, err = st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("remote_unknown", rows[0].State)
	assertions.Equal("planned", rows[1].State)
}
func TestMarketingEventMonotonic(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	for _, kind := range []string{"read", "delivered", "delivery_failed"} {
		ok, err := st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: kind, TrackingID: "tracking-1", At: time.Now()})
		requirements.NoError(err)
		assertions.True(ok)
	}
	_, rows, err := st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("read", rows[0].State)
}
func TestMarketingEventUnknownTracking(t *testing.T) {
	st, b, _ := broadcastFixture(t)
	ok, err := st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "read", TrackingID: "missing"})
	require.NoError(t, err)
	assert.False(t, ok)
}
func TestMarketingEventDeliveryFailedByCampaignAndFingerprint(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	ok, err := st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "delivery_failed", CampaignID: "55", Fingerprint: subs[0].Fingerprint, At: time.Now()})
	requirements.NoError(err)
	assertions.True(ok)
	_, rows, err = st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("delivery_failed", rows[0].State)
	assertions.Equal("planned", rows[1].State)
}
func TestMarketingEventClickByMID(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	ok, err := st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "echo", TrackingID: "tracking-1", MID: "mid-1"})
	requirements.NoError(err)
	assertions.True(ok)
	ok, err = st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "click", MID: "mid-1"})
	requirements.NoError(err)
	assertions.True(ok)
}
func TestEchoLinksArchivedMessage(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	conv, err := st.EnsureConversationWithType(b.SourceID, "psid:300", "direct_chat", "Example")
	requirements.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: b.SourceID, ConversationID: conv, SourceMessageID: "mid-1", MessageType: "chat"})
	requirements.NoError(err)
	ok, err := st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "echo", TrackingID: "tracking-1", MID: "mid-1", MessageID: sql.NullInt64{Int64: mid, Valid: true}})
	requirements.NoError(err)
	assertions.True(ok)
	_, rows, err = st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal(mid, rows[0].MessageID.Int64)
	assertions.Equal("mid-1", rows[0].MID)
}
func TestTrackingIDUnique(t *testing.T) {
	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	ok, err := st.ClaimRecipientContext(t.Context(), b.ID, rows[1].SubscriptionID, rows[1].RowRevision, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	require.Error(t, st.FinishRecipientContext(t.Context(), b.ID, rows[1].SubscriptionID, "accepted", "", "", "tracking-1"))
}
func TestSetCampaignCAS(t *testing.T) {
	st, b, _ := broadcastFixture(t)
	require.Error(t, st.SetCampaignContext(t.Context(), b.ID, "55", time.Now(), time.Now().Add(time.Hour), false))
}
func TestCancelLeavesFinishedRows(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	requirements.NoError(st.CancelBroadcastContext(t.Context(), b.ID))
	got, rows, err := st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("cancelled", got.State)
	assertions.Equal("accepted", rows[0].State)
	assertions.Equal("cancelled", rows[1].State)
	last, err := st.LastAcceptedSendContext(t.Context(), []int64{subs[0].ID, subs[1].ID})
	requirements.NoError(err)
	assertions.False(last[subs[0].ID].IsZero())
	assertions.Equal(last[subs[0].ID], last[subs[1].ID], "tokens for the same recipient share accepted-send history")
}
func TestSavePlanRejectsUnknownOrMissingRecipient(t *testing.T) {
	st, b, subs := broadcastFixture(t)
	require.Error(t, st.SavePlanContext(t.Context(), b.ID, 1, "bad", []store.RecipientDecisionRow{{SubscriptionID: subs[0].ID, Eligible: true}}))
	got, _, err := st.GetBroadcastContext(t.Context(), b.ID)
	require.NoError(t, err)
	assert.Equal(t, "draft", got.State)
}

func TestMarketingEventResolvesUnknownAndKeepsTracking(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	ok, err := st.ClaimRecipientContext(t.Context(), b.ID, rows[0].SubscriptionID, rows[0].RowRevision, time.Now())
	requirements.NoError(err)
	requirements.True(ok)
	requirements.NoError(st.FinishRecipientContext(t.Context(), b.ID, rows[0].SubscriptionID, "remote_unknown", "", "", ""))
	ok, err = st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "echo", CampaignID: "55", Fingerprint: subs[0].Fingerprint, TrackingID: "tracking-recovered", MID: "mid-recovered"})
	requirements.NoError(err)
	requirements.True(ok)
	ok, err = st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "read", TrackingID: "tracking-recovered"})
	requirements.NoError(err)
	assertions.True(ok)
	_, rows, err = st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("read", rows[0].State)
}
func TestMarketingEventMIDUnique(t *testing.T) {
	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	acceptRecipient(t, st, b, rows[1], "tracking-2")
	ok, err := st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "echo", TrackingID: "tracking-1", MID: "mid-unique"})
	require.NoError(t, err)
	require.True(t, ok)
	_, err = st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: b.SourceID, Kind: "echo", TrackingID: "tracking-2", MID: "mid-unique"})
	require.Error(t, err)
}

func TestMarketingEventRequiresMatchingSource(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-1")
	other, err := st.GetOrCreateSource("facebook_messages", "app:9:page:101")
	requirements.NoError(err)
	ev := store.MarketingEvent{Kind: "read", TrackingID: "tracking-1"}
	matched, err := st.ApplyMarketingEventContext(t.Context(), ev)
	requirements.Error(err)
	assertions.False(matched)
	ev.SourceID = other.ID
	_, found, err := st.FindMarketingEventRecipientContext(t.Context(), ev)
	requirements.NoError(err)
	assertions.False(found)
	matched, err = st.ApplyMarketingEventContext(t.Context(), ev)
	requirements.NoError(err)
	assertions.False(matched)
	_, rows, err = st.GetBroadcastContext(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("accepted", rows[0].State)
}

func TestClaimRecipientRejectsRevokedConsent(t *testing.T) {
	for _, status := range []string{"stopped", "unsubscribed", "absent"} {
		t.Run(status, func(t *testing.T) {
			requirements := require.New(t)
			st, b, subs := broadcastFixture(t)
			rows := readyBroadcast(t, st, b, subs)
			// Revoke after the sender has read its plan but before its claim.
			requirements.NoError(st.SetSubscriptionStatusContext(t.Context(), subs[0].ID, status, "stop", "webhook", "stop-before-claim"))
			claimed, err := st.ClaimRecipientContext(t.Context(), b.ID, subs[0].ID, rows[0].RowRevision, time.Now())
			requirements.NoError(err)
			assert.False(t, claimed)
			_, got, err := st.GetBroadcastContext(t.Context(), b.ID)
			requirements.NoError(err)
			assert.Equal(t, "planned", got[0].State)
		})
	}
}

func TestLastAcceptedSendAcrossRecipientTokens(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, b, subs := broadcastFixture(t)
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-history")
	// These two fixture tokens share their Page-scoped recipient ID.
	last, err := st.LastAcceptedSendContext(t.Context(), []int64{subs[0].ID, subs[1].ID})
	requirements.NoError(err)
	requirements.False(last[subs[0].ID].IsZero())
	assertions.Equal(last[subs[0].ID], last[subs[1].ID])
	// A matching PSID under a different source does not share history.
	other, err := st.GetOrCreateSource("facebook_messages", "app:9:page:101")
	requirements.NoError(err)
	walk, err := st.StartListWalkContext(t.Context(), other.ID, time.Now())
	requirements.NoError(err)
	otherSub := listedSubscription(t, st, other.ID, walk, "synthetic-other-token")
	last, err = st.LastAcceptedSendContext(t.Context(), []int64{otherSub.ID})
	requirements.NoError(err)
	assertions.Empty(last)
}

func TestRemoveSourceCascadesMarketingRecords(t *testing.T) {
	for _, withBroadcast := range []bool{false, true} {
		name := "empty_walk"
		if withBroadcast {
			name = "broadcast_receipts"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			st, src := marketingStore(t)
			walk, err := st.StartListWalkContext(t.Context(), src, time.Now())
			requirements.NoError(err)
			if withBroadcast {
				sub := listedSubscription(t, st, src, walk, "synthetic-delete-token")
				_, err = st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: src, AdAccountID: "700", Name: "Offers", MessageJSON: []byte(`{"text":"Example"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
				requirements.NoError(err)
			}
			requirements.NoError(st.RemoveSource(src))
			for _, table := range []string{"meta_marketing_list_walks", "meta_marketing_subscriptions", "meta_marketing_consent_events", "meta_broadcasts", "meta_broadcast_recipients"} {
				var n int
				requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
				assert.Zero(t, n, table)
			}
		})
	}
}

func TestLastAcceptedSendAcrossParticipantTokens(t *testing.T) {
	requirements := require.New(t)
	st, b, subs := broadcastFixture(t)
	participant, err := st.EnsureParticipant("subscriber@example.com", "Example Subscriber", "example.com")
	requirements.NoError(err)
	for i, sub := range subs {
		_, err = st.UpsertListedSubscriptionContext(t.Context(), b.SourceID, sub.LastListWalkID.Int64, store.ListedSubscription{Token: sub.Token, PSID: strconv.Itoa(300 + i), ParticipantID: sql.NullInt64{Int64: participant, Valid: true}})
		requirements.NoError(err)
	}
	rows := readyBroadcast(t, st, b, subs)
	acceptRecipient(t, st, b, rows[0], "tracking-participant-history")
	last, err := st.LastAcceptedSendContext(t.Context(), []int64{subs[0].ID, subs[1].ID})
	requirements.NoError(err)
	requirements.False(last[subs[0].ID].IsZero())
	assert.Equal(t, last[subs[0].ID], last[subs[1].ID])
}
