package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func optInItem(status, delivery string, timestamp int64) metawebhook.Item {
	return metawebhook.Item{AppID: "9", Object: "page", EntryID: "100", Field: "messaging_optins", DeliveryID: delivery, Index: 3,
		Value: []byte(fmt.Sprintf(`{"sender":{"id":"300"},"recipient":{"id":"100"},"timestamp":%d,"optin":{"type":"notification_messages","notification_messages_token":"synthetic-mm-token-1","notification_messages_timezone":"UTC","token_expiry_timestamp":"1893456000","user_token_status":"REFRESHED","title":"Example offers","notification_messages_status":%q}}`, timestamp, status))}
}
func applyOptIn(t *testing.T, st *store.Store, item metawebhook.Item) metawebhook.ItemOutcome {
	t.Helper()
	out, err := HandleMarketingOptIn(t.Context(), st, item)
	require.NoError(t, err)
	return out
}
func TestOptInWebhookActivates(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src, id := subscriptionFixture(t)
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, optInItem("", "delivery-1", 1793577600123)).Class)
	rows := subscriptionRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.Equal("active", rows[0].Status)
	assertions.Equal("300", rows[0].PSID.String)
	assertions.Equal("UTC", rows[0].Timezone)
	assertions.True(rows[0].OptedInAt.Valid)
	assertions.False(rows[0].LastConfirmedAt.Valid, "webhook does not replace a complete list walk")
	participant, err := ResolveDMActor(t.Context(), st, id, src, "300", "")
	requirements.NoError(err)
	assertions.Equal(participant, rows[0].ParticipantID.Int64)
	events, err := st.LatestConsentEventsContext(t.Context(), []int64{rows[0].ID})
	requirements.NoError(err)
	requirements.Len(events[rows[0].ID], 1)
	assertions.Equal("delivery-1:3", events[rows[0].ID][0].EvidenceRef)
}
func TestStopNotificationsWithSpaces(t *testing.T) {
	for _, status := range []string{"STOP NOTIFICATIONS", "STOP_NOTIFICATIONS"} {
		t.Run(status, func(t *testing.T) {
			st, _, id := subscriptionFixture(t)
			require.Equal(t, metawebhook.Applied, applyOptIn(t, st, optInItem("", "delivery-1", 1793577600123)).Class)
			require.Equal(t, metawebhook.Applied, applyOptIn(t, st, optInItem(status, "delivery-2", 1793577601123)).Class)
			assert.Equal(t, "stopped", subscriptionRows(t, st, id)[0].Status)
		})
	}
}
func TestResumeAfterLocalUnsubscribe(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, _, id := subscriptionFixture(t)
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, optInItem("", "delivery-1", 1793577600123)).Class)
	row := subscriptionRows(t, st, id)[0]
	requirements.NoError(st.SetSubscriptionStatusContext(t.Context(), row.ID, "unsubscribed", "local_unsubscribe", "owner", "local-1"))
	old := optInItem("RESUME NOTIFICATIONS", "old-resume", 1793577601123)
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, old).Class)
	assertions.Equal("unsubscribed", subscriptionRows(t, st, id)[0].Status)
	fresh := optInItem("RESUME_NOTIFICATIONS", "new-resume", time.Now().Add(time.Minute).UnixMilli())
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, fresh).Class)
	assertions.Equal("active", subscriptionRows(t, st, id)[0].Status)
}
func TestOptInReplayIdempotent(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, _, id := subscriptionFixture(t)
	item := optInItem("", "delivery-1", 1793577600123)
	for range 2 {
		requirements.Equal(metawebhook.Applied, applyOptIn(t, st, item).Class)
	}
	rows := subscriptionRows(t, st, id)
	requirements.Len(rows, 1)
	events, err := st.LatestConsentEventsContext(t.Context(), []int64{rows[0].ID})
	requirements.NoError(err)
	assertions.Len(events[rows[0].ID], 1)
	item.DeliveryID = "delivery-2"
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, item).Class)
	events, err = st.LatestConsentEventsContext(t.Context(), []int64{rows[0].ID})
	requirements.NoError(err)
	assertions.Len(events[rows[0].ID], 2, "same index from another delivery is distinct evidence")
}
func TestOptInRejectsMisroutingAndMissingEvidence(t *testing.T) {
	for _, mutation := range []string{"page", "object", "evidence", "json"} {
		t.Run(mutation, func(t *testing.T) {
			st, _, id := subscriptionFixture(t)
			item := optInItem("", "delivery-1", 1793577600123)
			switch mutation {
			case "page":
				item.EntryID = "101"
			case "object":
				item.Object = "instagram"
			case "evidence":
				item.DeliveryID = ""
			case "json":
				item.Value = []byte(`{"optin":{"notification_messages_token":"synthetic-mm-token-1"}}`)
			}
			out, err := HandleMarketingOptIn(t.Context(), st, item)
			require.NoError(t, err)
			assert.Contains(t, []metawebhook.Class{metawebhook.Invalid, metawebhook.Unrouted}, out.Class)
			assert.Empty(t, subscriptionRows(t, st, id))
		})
	}
}

func marketingReceipt(t *testing.T, custom bool) (*store.Store, store.Broadcast, store.MarketingSubscription) {
	t.Helper()
	st, _, id := subscriptionFixture(t)
	psid := "300"
	if custom {
		psid = ""
	}
	w, err := st.StartListWalkContext(t.Context(), id, time.Now())
	require.NoError(t, err)
	sub, err := st.UpsertListedSubscriptionContext(t.Context(), id, w, store.ListedSubscription{Token: "synthetic-mm-token-1", PSID: psid})
	require.NoError(t, err)
	b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: id, AdAccountID: "700", Name: "Example offers", MessageJSON: []byte(`{"text":"Example offer"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
	require.NoError(t, err)
	require.NoError(t, st.SavePlanContext(t.Context(), b.ID, b.Revision, "digest", []store.RecipientDecisionRow{{SubscriptionID: sub.ID, Eligible: true}}))
	require.NoError(t, st.TransitionBroadcastContext(t.Context(), b.ID, "planned", "campaign_pending", ""))
	require.NoError(t, st.SetCampaignContext(t.Context(), b.ID, "55", time.Now().Add(-time.Hour), time.Now().Add(7*24*time.Hour), false))
	_, rows, err := st.GetBroadcastContext(t.Context(), b.ID)
	require.NoError(t, err)
	claimed, err := st.ClaimRecipientContext(t.Context(), b.ID, sub.ID, rows[0].RowRevision, time.Now())
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, st.FinishRecipientContext(t.Context(), b.ID, sub.ID, "accepted", "", "", "tracking-1"))
	return st, b, sub
}
func marketingItem(field, payload string) metawebhook.Item {
	return metawebhook.Item{AppID: "9", Object: "page", EntryID: "100", Field: field, DeliveryID: "delivery-1", Index: 1, Time: 1793577600, Value: []byte(payload)}
}

const marketingEcho = `{"mid":"mid-1","marketing_message_tracking_id":"tracking-1","message_id":"55","page_id":"100","recipient_id":"300","messenger_subscription_token":"synthetic-mm-token-1","timestamp":1793577600123}`

func applyMarketing(t *testing.T, st *store.Store, field, payload string) metawebhook.ItemOutcome {
	t.Helper()
	out, err := HandleMarketingMessageEvent(t.Context(), st, field, marketingItem(field, payload))
	require.NoError(t, err)
	return out
}
func receiptRow(t *testing.T, st *store.Store, b store.Broadcast) store.BroadcastRecipient {
	t.Helper()
	_, rows, err := st.GetBroadcastContext(t.Context(), b.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}
func TestMarketingEchoPersistsArchiveRow(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, _ := marketingReceipt(t, false)
	requirements.Equal(metawebhook.Applied, applyMarketing(t, st, "marketing_message_echoes", marketingEcho).Class)
	row := receiptRow(t, st, b)
	requirements.True(row.MessageID.Valid)
	assertions.Equal("mid-1", row.MID)
	ref, err := st.MessageConversation(row.MessageID.Int64)
	requirements.NoError(err)
	assertions.Equal("psid:300", ref.SourceConversationID)
	mine, err := st.GetMessageIsFromMe(row.MessageID.Int64)
	requirements.NoError(err)
	assertions.True(mine)
	text, raw, err := st.GetMessageBodyAndRawContext(t.Context(), row.MessageID.Int64)
	requirements.NoError(err)
	assertions.Equal("Example offer", text)
	assertions.NotContains(string(raw), "synthetic-mm-token")
	w := &dmWriter{store: st, sourceID: b.SourceID, src: DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}}
	standard := dmItem(w, "message_echoes", `{"sender":{"id":"100"},"recipient":{"id":"300"},"timestamp":1793577600123,"message":{"mid":"mid-1","text":"Example offer","is_echo":true}}`)
	requirements.Equal(metawebhook.Applied, dmApply(t, dmTestHandler(t, w), standard).Class)
	n, err := st.CountMessagesWithRaw(b.SourceID)
	requirements.NoError(err)
	assertions.EqualValues(1, n)
	metadata, err := st.GetMessageMetadataContext(t.Context(), row.MessageID.Int64)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assertions.Equal(b.ID, fields["broadcast_id"])
	assertions.Equal("55", fields["campaign_id"])
}
func TestMarketingEchoWithoutMIDNoArchive(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, _ := marketingReceipt(t, true)
	requirements.Equal(metawebhook.Applied, applyMarketing(t, st, "marketing_message_echoes", `{"marketing_message_tracking_id":"tracking-1","message_id":"55","page_id":"100","messenger_subscription_token":"synthetic-mm-token-1"}`).Class)
	assertions.False(receiptRow(t, st, b).MessageID.Valid)
	n, err := st.CountMessagesWithRaw(b.SourceID)
	requirements.NoError(err)
	assertions.Zero(n)
}
func TestDeliveryFailedMatchedByCampaignAndFingerprint(t *testing.T) {
	assertions := assert.New(t)

	st, b, _ := marketingReceipt(t, false)
	require.Equal(t, metawebhook.Applied, applyMarketing(t, st, "marketing_message_delivery_failed", `{"message_id":"55","page_id":"100","messenger_subscription_token":"synthetic-mm-token-1","error_message":"provider-secret synthetic-mm-token-1"}`).Class)
	row := receiptRow(t, st, b)
	assertions.Equal("delivery_failed", row.State)
	assertions.Empty(row.ErrorCode)
	assertions.Equal("send_rejected", row.Reason)
}
func TestClickMatchedByMID(t *testing.T) {
	st, b, _ := marketingReceipt(t, false)
	require.Equal(t, metawebhook.Applied, applyMarketing(t, st, "marketing_message_echoes", marketingEcho).Class)
	require.Equal(t, metawebhook.Applied, applyMarketing(t, st, "marketing_message_clicks", `{"mid":"mid-1","page_id":"100","messenger_subscription_token":"synthetic-mm-token-1"}`).Class)
	assert.Equal(t, "accepted", receiptRow(t, st, b).State)
}
func TestMarketingUnmatchedNonTrackingIgnored(t *testing.T) {
	st, b, _ := marketingReceipt(t, false)
	for _, payload := range []string{`{"mid":"unknown"}`, `{"message_id":"55","messenger_subscription_token":"synthetic-mm-token-other"}`} {
		assert.Equal(t, metawebhook.Ignored, applyMarketing(t, st, "marketing_message_reads", payload).Class)
	}
	assert.Equal(t, "accepted", receiptRow(t, st, b).State)
}

func marketingReceiptWorker(t *testing.T, st *store.Store, field, payload string) (*metawebhook.Worker, *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, func() time.Time { return now })
	require.NoError(t, err)
	body := fmt.Sprintf(`{"object":"page","entry":[{"id":"100","changes":[{"field":%q,"value":%s}]}]}`, field, payload)
	_, _, err = spool.Accept("9", []byte(body))
	require.NoError(t, err)
	registry := metawebhook.NewRegistry()
	registry.Register("page", field, func(ctx context.Context, _ string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		outcomes := make([]metawebhook.ItemOutcome, len(items))
		for i, item := range items {
			outcome, err := HandleMarketingMessageEvent(ctx, st, field, item)
			assert.NoError(t, err)
			outcomes[i] = outcome
		}
		return outcomes
	})
	return metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: registry}), &now
}

func TestMarketingTrackingReceiptBeforeSendResult(t *testing.T) {
	for _, state := range []string{"delivered", "read"} {
		t.Run(state, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newSenderFixture(t, 1)
			f.warm(t)
			field := "marketing_message_reads"
			if state == "delivered" {
				field = "marketing_message_deliveries"
			}
			worker, now := marketingReceiptWorker(t, f.st, field, `{"marketing_message_tracking_id":"tracking-1"}`)
			f.api.onSend = func() {
				row := receiptRow(t, f.st, f.b)
				assertions.Equal("dispatching", row.State)
				assertions.Empty(row.TrackingID)
				summary, err := worker.DrainOnce(t.Context())
				requirements.NoError(err)
				assertions.Equal(1, summary.Retried)
			}
			_, err := f.sender.Send(t.Context(), f.req)
			requirements.NoError(err)
			assertions.Equal("accepted", receiptRow(t, f.st, f.b).State)
			*now = now.Add(time.Hour)
			summary, err := worker.DrainOnce(t.Context())
			requirements.NoError(err)
			assertions.Equal(1, summary.Applied)
			assertions.Equal(state, receiptRow(t, f.st, f.b).State)
			_, err = f.sender.Send(t.Context(), f.req)
			requirements.NoError(err)
			assertions.Equal(1, f.api.sends)
		})
	}
}

func TestMarketingUnknownTrackingReceiptRetryIsBounded(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, b, _ := marketingReceipt(t, false)
	worker, now := marketingReceiptWorker(t, st, "marketing_message_reads", `{"marketing_message_tracking_id":"unknown"}`)
	for attempt := 1; attempt <= 12; attempt++ {
		summary, err := worker.DrainOnce(t.Context())
		requirements.NoError(err)
		if attempt < 12 {
			assertions.Equal(1, summary.Retried)
		} else {
			assertions.Equal(1, summary.Failed)
		}
		*now = now.Add(time.Hour)
	}
	assertions.Equal("accepted", receiptRow(t, st, b).State)
}
func TestMarketingEventTokenNotPersisted(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, _ := marketingReceipt(t, false)
	item := optInItem("", "delivery-optin", 1793577600123)
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, item).Class)
	var payload map[string]any
	requirements.NoError(json.Unmarshal([]byte(marketingEcho), &payload))
	payload["text"] = "synthetic-mm-token-1"
	payload["nested"] = map[string]any{"token": "synthetic-mm-token-1"}
	encoded, err := json.Marshal(payload)
	requirements.NoError(err)
	requirements.Equal(metawebhook.Applied, applyMarketing(t, st, "marketing_message_echoes", string(encoded)).Class)
	row := receiptRow(t, st, b)
	text, raw, err := st.GetMessageBodyAndRawContext(t.Context(), row.MessageID.Int64)
	requirements.NoError(err)
	metadata, err := st.GetMessageMetadataContext(t.Context(), row.MessageID.Int64)
	requirements.NoError(err)
	assertions.NotContains(text+string(raw)+metadata.String, "synthetic-mm-token-1")
	rows, err := st.DB().Query("SELECT detail FROM meta_marketing_consent_events")
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var detail string
		requirements.NoError(rows.Scan(&detail))
		assertions.NotContains(detail, "synthetic-mm-token-1")
	}
	requirements.NoError(rows.Err())
	out, err := HandleMarketingMessageEvent(t.Context(), st, "marketing_message_reads", marketingItem("marketing_message_reads", `{"timestamp":"synthetic-mm-token-1"}`))
	assertions.Equal(metawebhook.Invalid, out.Class)
	if err != nil {
		assertions.NotContains(err.Error(), "synthetic-mm-token-1")
	}
}
func TestMarketingEventsBoundToSourceAndRecipient(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, _ := marketingReceipt(t, false)
	_, err := st.GetOrCreateSource(SourceTypeFacebookMessages, "app:9:page:101")
	requirements.NoError(err)
	item := marketingItem("marketing_message_reads", `{"marketing_message_tracking_id":"tracking-1"}`)
	item.EntryID = "101"
	out, err := HandleMarketingMessageEvent(t.Context(), st, item.Field, item)
	requirements.NoError(err)
	assertions.Equal(metawebhook.Retry, out.Class)
	assertions.True(out.CountAttempt)
	assertions.Equal("accepted", receiptRow(t, st, b).State)
	for _, payload := range []string{
		`{"marketing_message_tracking_id":"tracking-1","mid":"mid-1","page_id":"101","recipient_id":"300"}`,
		`{"marketing_message_tracking_id":"tracking-1","mid":"mid-1","page_id":"100","recipient_id":"301"}`,
	} {
		assertions.Equal(metawebhook.Invalid, applyMarketing(t, st, "marketing_message_echoes", payload).Class)
	}
	var count int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assertions.Zero(count)
}
func TestMarketingDeliveryThenReadMonotonic(t *testing.T) {
	st, b, _ := marketingReceipt(t, false)
	for _, field := range []string{"marketing_message_deliveries", "marketing_message_reads", "marketing_message_deliveries"} {
		require.Equal(t, metawebhook.Applied, applyMarketing(t, st, field, `{"marketing_message_tracking_id":"tracking-1"}`).Class)
	}
	assert.Equal(t, "read", receiptRow(t, st, b).State)
}
func TestSubscribeMarketingFieldsExplicitOnly(t *testing.T) {
	requirements := require.New(t)

	fields := []string{"messaging_optins", "marketing_message_echoes", "marketing_message_deliveries", "marketing_message_reads", "marketing_message_clicks", "marketing_message_delivery_failed"}
	calls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		assert.Equal(t, "feed", r.PostForm.Get("subscribed_fields"))
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	requirements.Error(c.Subscribe(t.Context(), "100", nil))
	assert.Zero(t, calls)
	requirements.NoError(c.Subscribe(t.Context(), "100", []string{"feed"}))
	// A separate client keeps the transport assertion on the actual encoded form.
	c = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		assert.ElementsMatch(t, fields, strings.Split(r.PostForm.Get("subscribed_fields"), ","))
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	requirements.NoError(c.Subscribe(t.Context(), "100", fields))
}

func TestMarketingEchoBusySourceDoesNotConsumeAttempt(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, b, _ := marketingReceipt(t, false)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), b.SourceID)
	requirements.NoError(err)
	defer func() { require.NoError(t, execution.Release()) }()
	out, err := HandleMarketingMessageEvent(t.Context(), st, "marketing_message_echoes", marketingItem("marketing_message_echoes", marketingEcho))
	requirements.NoError(err)
	assertions.Equal(metawebhook.Retry, out.Class)
	assertions.False(out.CountAttempt)
	assertions.False(receiptRow(t, st, b).MessageID.Valid)
}
func TestOptInOlderSenderDoesNotRelinkParticipant(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, _, id := subscriptionFixture(t)
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, optInItem("", "new-optin", 1793577601123)).Class)
	before := subscriptionRows(t, st, id)[0]
	item := optInItem("", "old-optin", 1793577600123)
	item.Value = []byte(strings.ReplaceAll(string(item.Value), `"300"`, `"301"`))
	requirements.Equal(metawebhook.Applied, applyOptIn(t, st, item).Class)
	after := subscriptionRows(t, st, id)[0]
	assertions.Equal(before.PSID, after.PSID)
	assertions.Equal(before.ParticipantID, after.ParticipantID)
}

func TestDeliveryFailedCategoryUsesOnlyAllowlistedValues(t *testing.T) {
	for _, tc := range []struct{ message, want string }{{"WEEKLY_MM_LOAD_CAP_REACHED", "frequency_cap"}, {"RECIPIENT_NOT_AVAILABLE", "recipient_unavailable"}, {"provider-secret", "send_rejected"}} {
		t.Run(tc.want, func(t *testing.T) {
			st, b, _ := marketingReceipt(t, false)
			payload := fmt.Sprintf(`{"message_id":"55","messenger_subscription_token":"synthetic-mm-token-1","error_message":%q}`, tc.message)
			require.Equal(t, metawebhook.Applied, applyMarketing(t, st, "marketing_message_delivery_failed", payload).Class)
			assert.Equal(t, tc.want, receiptRow(t, st, b).Reason)
		})
	}
}

func TestMarketingEventSecretInIdentifiersRejected(t *testing.T) {
	for _, field := range []string{"mid", "marketing_message_tracking_id"} {
		t.Run(field, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			st, b, _ := marketingReceipt(t, false)
			var payload map[string]any
			requirements.NoError(json.Unmarshal([]byte(marketingEcho), &payload))
			payload[field] = "identifier-synthetic-mm-token-1"
			encoded, err := json.Marshal(payload)
			requirements.NoError(err)
			out, err := HandleMarketingMessageEvent(t.Context(), st, "marketing_message_echoes", marketingItem("marketing_message_echoes", string(encoded)))
			requirements.NoError(err)
			assertions.Equal(metawebhook.Invalid, out.Class)
			assertions.Empty(receiptRow(t, st, b).MID)
			var n int
			requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
			assertions.Zero(n)
		})
	}
}

func TestMarketingEventConflictingReceiptHintsDoNotOverwriteArchive(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, first, sub := marketingReceipt(t, false)
	requirements.Equal(metawebhook.Applied, applyMarketing(t, st, "marketing_message_echoes", marketingEcho).Class)
	second, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: first.SourceID, AdAccountID: "700", Name: "Another offer", MessageJSON: []byte(`{"text":"Another offer"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
	requirements.NoError(err)
	requirements.NoError(st.SavePlanContext(t.Context(), second.ID, second.Revision, "digest-2", []store.RecipientDecisionRow{{SubscriptionID: sub.ID, Eligible: true}}))
	requirements.NoError(st.TransitionBroadcastContext(t.Context(), second.ID, "planned", "campaign_pending", ""))
	requirements.NoError(st.SetCampaignContext(t.Context(), second.ID, "56", time.Now().Add(-time.Hour), time.Now().Add(7*24*time.Hour), false))
	row := receiptRow(t, st, second)
	claimed, err := st.ClaimRecipientContext(t.Context(), second.ID, sub.ID, row.RowRevision, time.Now())
	requirements.NoError(err)
	requirements.True(claimed)
	requirements.NoError(st.FinishRecipientContext(t.Context(), second.ID, sub.ID, "accepted", "", "", "tracking-2"))
	payload := strings.ReplaceAll(strings.ReplaceAll(marketingEcho, "tracking-1", "tracking-2"), `"message_id":"55"`, `"message_id":"56"`)
	out, err := HandleMarketingMessageEvent(t.Context(), st, "marketing_message_echoes", marketingItem("marketing_message_echoes", payload))
	requirements.NoError(err)
	assertions.Equal(metawebhook.Invalid, out.Class)
	metadata, err := st.GetMessageMetadataContext(t.Context(), receiptRow(t, st, first).MessageID.Int64)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assertions.Equal(first.ID, fields["broadcast_id"])
	assertions.Equal("55", fields["campaign_id"])
	assertions.False(receiptRow(t, st, second).MessageID.Valid)
}
