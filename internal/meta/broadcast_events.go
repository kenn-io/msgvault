package meta

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func marketingSource(st *store.Store, item metawebhook.Item) (DMSource, *store.Source, error) {
	src := DMSource{AppID: item.AppID, PageID: item.EntryID, Platform: PlatformMessenger}
	if item.Object != "page" || !ValidAccountID(src.AppID) || !ValidAccountID(src.PageID) {
		return src, nil, ErrDMSourceUnrouted
	}
	source, err := st.GetSourceByTypeAndIdentifier(SourceTypeFacebookMessages, src.Identifier())
	if err == nil && (source.HistoryOnly || source.MergedIntoSourceID != 0) {
		err = ErrDMSourceUnrouted
	}
	return src, source, err
}
func marketingResult(err error) (metawebhook.ItemOutcome, error) {
	if err == nil {
		return dmOutcome(metawebhook.Applied, ""), nil
	}
	if errors.Is(err, store.ErrSyncAlreadyActive) {
		return metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "source_busy"}, nil
	}
	if errors.Is(err, ErrDMSourceUnrouted) || errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrSourceRetired) {
		return dmOutcome(metawebhook.Unrouted, "source_missing"), nil
	}
	return dmOutcome(metawebhook.Retry, "archive_unavailable"), errors.New("marketing event archive unavailable")
}

// HandleMarketingOptIn applies consent using the receiver's durable evidence ID.
// The daemon checks the enabled broadcast profile before calling this function.
func HandleMarketingOptIn(ctx context.Context, st *store.Store, item metawebhook.Item) (metawebhook.ItemOutcome, error) {
	src, source, err := marketingSource(st, item)
	if err != nil {
		return marketingResult(err)
	}
	var e struct {
		Sender    DMParticipant `json:"sender"`
		Recipient DMParticipant `json:"recipient"`
		Timestamp int64         `json:"timestamp"`
		Optin     struct {
			Type            string             `json:"type"`
			Token           string             `json:"notification_messages_token"`
			Timezone        string             `json:"notification_messages_timezone"`
			Expiry          marketingTimestamp `json:"token_expiry_timestamp"`
			Status          string             `json:"notification_messages_status"`
			UserTokenStatus string             `json:"user_token_status"`
			Title           string             `json:"title"`
		} `json:"optin"`
	}
	if item.Field != "messaging_optins" || item.DeliveryID == "" || item.Index < 0 || len(item.Value) > 256<<10 || json.Unmarshal(item.Value, &e) != nil {
		return dmOutcome(metawebhook.Invalid, "invalid_consent"), nil //nolint:nilerr // Invalid signed data is a terminal outcome, not a retry.
	}
	if e.Optin.Type == "" {
		return dmOutcome(metawebhook.Invalid, "invalid_consent"), nil
	}
	if e.Optin.Type != "notification_messages" {
		return dmOutcome(metawebhook.Ignored, ""), nil
	}
	if !ValidAccountID(e.Sender.ID) || e.Sender.ID == src.PageID || e.Recipient.ID != src.PageID || e.Timestamp <= 0 || e.Optin.Token == "" {
		return dmOutcome(metawebhook.Invalid, "invalid_consent"), nil
	}
	switch e.Optin.Status {
	case "", "STOP NOTIFICATIONS", "RESUME NOTIFICATIONS":
	case "STOP_NOTIFICATIONS", "RESUME_NOTIFICATIONS":
		slog.DebugContext(ctx, "marketing consent status spelling variant", "variant", e.Optin.Status)
	default:
		return dmOutcome(metawebhook.Invalid, "invalid_consent"), nil
	}
	sub, err := st.ApplyConsentWebhookContext(ctx, source.ID, store.ConsentWebhook{PSID: e.Sender.ID, Token: e.Optin.Token, Title: e.Optin.Title, Timezone: e.Optin.Timezone, Status: e.Optin.Status, UserTokenStatus: e.Optin.UserTokenStatus, Expiry: e.Optin.Expiry.Time, At: time.UnixMilli(e.Timestamp).UTC(), EvidenceRef: item.DeliveryID + ":" + strconv.Itoa(item.Index)})
	if err != nil {
		return marketingResult(err)
	}
	if !sub.PSID.Valid {
		return marketingResult(nil)
	}
	participant, err := ResolveDMActor(ctx, st, source.ID, src, sub.PSID.String, "")
	if err == nil {
		err = st.SetSubscriptionParticipantContext(ctx, sub.ID, participant)
	}
	return marketingResult(err)
}

type marketingMessageEvent struct {
	TrackingID   string `json:"marketing_message_tracking_id"`
	MID          string `json:"mid"`
	CampaignID   string `json:"message_id"`
	PageID       string `json:"page_id"`
	RecipientID  string `json:"recipient_id"`
	Token        string `json:"messenger_subscription_token"`
	ErrorMessage string `json:"error_message"`
	Timestamp    int64  `json:"timestamp"`
}

// HandleMarketingMessageEvent correlates only receipts owned by this inbox.
// Incoming provider text and raw subscription tokens are never archived.
func HandleMarketingMessageEvent(ctx context.Context, st *store.Store, field string, item metawebhook.Item) (metawebhook.ItemOutcome, error) {
	src, source, err := marketingSource(st, item)
	if err != nil {
		return marketingResult(err)
	}
	kind := map[string]string{"marketing_message_echoes": "echo", "marketing_message_deliveries": "delivered", "marketing_message_reads": "read", "marketing_message_clicks": "click", "marketing_message_delivery_failed": "delivery_failed"}[field]
	var e marketingMessageEvent
	value := item.Value
	// Messaging envelopes wrap the event in its field; changes carry it directly.
	var members map[string]jsontext.Value
	if len(value) > 256<<10 || json.Unmarshal(value, &members) != nil {
		return dmOutcome(metawebhook.Invalid, "invalid_event"), nil //nolint:nilerr // Invalid signed data is a terminal outcome, not a retry.
	}
	if nested, ok := members[field]; ok {
		value = nested
	}
	if kind == "" || item.Field != field || json.Unmarshal(value, &e) != nil || (e.PageID != "" && e.PageID != src.PageID) || (e.RecipientID != "" && !ValidAccountID(e.RecipientID)) || (e.MID != "" && !safeObjectID(e.MID)) {
		return dmOutcome(metawebhook.Invalid, "invalid_event"), nil //nolint:nilerr // Invalid signed data is a terminal outcome, not a retry.
	}
	ev := store.MarketingEvent{SourceID: source.ID, Kind: kind, TrackingID: e.TrackingID, MID: e.MID, CampaignID: e.CampaignID}
	if kind == "delivery_failed" {
		ev.FailureCategory = string(ClassifyMarketingError(&APIError{Kind: e.ErrorMessage}, false).Category)
	}
	if e.Token != "" {
		ev.Fingerprint = store.TokenFingerprint(e.Token)
	}
	if e.Timestamp > 0 {
		ev.At = time.UnixMilli(e.Timestamp).UTC()
	} else if item.Time > 0 {
		ev.At = time.Unix(item.Time, 0).UTC()
	} else {
		ev.At = time.Now().UTC()
	}
	row, found, err := st.FindMarketingEventRecipientContext(ctx, ev)
	if err != nil {
		return marketingResult(err)
	}
	if !found {
		if e.TrackingID != "" {
			// A signed receipt can arrive before the send response commits its
			// tracking ID. Keep it in the durable spool for correlation on retry.
			// Count attempts so unrelated receipts reach the receiver's retry cap.
			return metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "receipt_pending", CountAttempt: true}, nil
		}
		return dmOutcome(metawebhook.Ignored, ""), nil
	}
	if e.MID != "" && row.MID == "" {
		byMID, matched, err := st.FindMarketingEventRecipientContext(ctx, store.MarketingEvent{SourceID: source.ID, MID: e.MID})
		if err != nil {
			return marketingResult(err)
		}
		if matched && (byMID.BroadcastID != row.BroadcastID || byMID.SubscriptionID != row.SubscriptionID) {
			return dmOutcome(metawebhook.Invalid, "receipt_mismatch"), nil
		}
	}
	b, _, err := st.GetBroadcastContext(ctx, row.BroadcastID)
	if err != nil {
		return marketingResult(err)
	}
	subs, err := st.ListMarketingSubscriptionsContext(ctx, source.ID, store.SubscriptionFilter{SubscriptionIDs: []int64{row.SubscriptionID}})
	if err != nil {
		return marketingResult(err)
	}
	if len(subs) != 1 {
		return dmOutcome(metawebhook.Invalid, "subscription_missing"), nil
	}
	sub := subs[0]
	for _, id := range []string{e.TrackingID, e.MID, e.CampaignID, e.RecipientID} {
		if sub.Token != "" && strings.Contains(id, sub.Token) {
			return dmOutcome(metawebhook.Invalid, "invalid_event"), nil
		}
	}
	if (e.CampaignID != "" && e.CampaignID != b.CampaignID) || (e.Token != "" && ev.Fingerprint != sub.Fingerprint) || (e.RecipientID != "" && e.RecipientID != sub.PSID.String) || (row.TrackingID != "" && e.TrackingID != "" && row.TrackingID != e.TrackingID) || (row.MID != "" && e.MID != "" && row.MID != e.MID) {
		return dmOutcome(metawebhook.Invalid, "receipt_mismatch"), nil
	}
	if kind == "echo" && e.MID != "" && sub.PSID.Valid {
		id, err := archiveMarketingEcho(ctx, st, src, b, sub, e.MID, ev.At)
		if err != nil {
			return marketingResult(err)
		}
		ev.MessageID = sql.NullInt64{Int64: id, Valid: true}
	}
	_, err = st.ApplyMarketingEventContext(ctx, ev)
	return marketingResult(err)
}

func archiveMarketingEcho(ctx context.Context, st *store.Store, src DMSource, b store.Broadcast, sub store.MarketingSubscription, mid string, at time.Time) (id int64, err error) {
	execution, err := st.AcquireSyncExecutionContext(ctx, b.SourceID)
	if err != nil {
		return 0, err
	}
	defer func() {
		if releaseErr := execution.Release(); err == nil {
			err = releaseErr
		}
	}()
	current, err := st.GetSourceByIDContext(ctx, b.SourceID)
	if err != nil {
		return 0, err
	}
	if current.HistoryOnly || current.MergedIntoSourceID != 0 {
		return 0, store.ErrSourceRetired
	}
	var message struct {
		Text       string `json:"text"`
		Attachment struct {
			Payload struct {
				Elements []struct {
					Title    string `json:"title"`
					Subtitle string `json:"subtitle"`
				} `json:"elements"`
			} `json:"payload"`
		} `json:"attachment"`
	}
	if json.Unmarshal(b.MessageJSON, &message) != nil {
		return 0, errors.New("invalid broadcast message")
	}
	parts := []string{message.Text}
	for _, e := range message.Attachment.Payload.Elements {
		parts = append(parts, e.Title, e.Subtitle)
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	text = strings.ReplaceAll(text, sub.Token, sub.Fingerprint)
	event := messagingEvent{Sender: DMParticipant{ID: src.PageID}, Recipient: DMParticipant{ID: sub.PSID.String}, Timestamp: at.UnixMilli(), Message: &dmEventMessage{Mid: mid, Text: text, IsEcho: true}}
	rec, err := recordFromEvent(src, event)
	if err != nil {
		return 0, err
	}
	rec.Meta.BroadcastID, rec.Meta.CampaignID = b.ID, b.CampaignID
	w := dmWriter{store: st, sourceID: b.SourceID, src: src}
	id, _, err = w.persist(ctx, rec)
	return id, err
}
