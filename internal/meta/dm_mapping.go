package meta

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type dmReferral struct {
	AdID string `json:"ad_id,omitempty"`
}
type dmStoryMetadata struct {
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
}
type dmConversationMetadata struct {
	Version        int    `json:"version"`
	AppID          string `json:"app_id"`
	PageID         string `json:"page_id"`
	IGID           string `json:"ig_id,omitempty"`
	ThreadID       string `json:"thread_id,omitempty"`
	BackfillCapped bool   `json:"backfill_capped,omitempty"`
}
type dmAttachmentMetadata struct {
	Type      string         `json:"type"`
	StickerID jsontext.Value `json:"sticker_id,omitempty"`
}
type dmMetadata struct {
	Version               int                    `json:"version"`
	AppID                 string                 `json:"app_id"`
	PageID                string                 `json:"page_id"`
	IGID                  string                 `json:"ig_id,omitempty"`
	ThreadID              string                 `json:"thread_id,omitempty"`
	Attachments           []dmAttachmentMetadata `json:"attachments,omitempty"`
	Shares                int                    `json:"shares,omitempty"`
	Story                 *dmStoryMetadata       `json:"story,omitempty"`
	Referral              *dmReferral            `json:"referral,omitempty"`
	IsUnsupported         bool                   `json:"is_unsupported,omitempty"`
	EchoAppID             string                 `json:"echo_app_id,omitempty"`
	Channel               string                 `json:"channel,omitempty"`
	BroadcastID           string                 `json:"broadcast_id,omitempty"`
	CampaignID            string                 `json:"campaign_id,omitempty"`
	EditCount             int                    `json:"edit_count,omitempty"`
	LegacyGraphObservedAt int64                  `json:"graph_observed_at,omitempty"`
	ReactionTimes         map[string]int64       `json:"reaction_times,omitempty"`
	ReferencedMessageID   string                 `json:"referenced_message_id,omitempty"`
}
type messagingEvent struct {
	Sender    DMParticipant   `json:"sender"`
	Recipient DMParticipant   `json:"recipient"`
	Timestamp int64           `json:"timestamp"`
	Message   *dmEventMessage `json:"message,omitempty"`
	Referral  *dmReferral     `json:"referral,omitempty"`
	Raw       []byte          `json:"-"`
}
type dmEventMessage struct {
	Mid           string         `json:"mid"`
	Text          string         `json:"text,omitempty"`
	IsEcho        bool           `json:"is_echo,omitempty"`
	AppID         jsontext.Value `json:"app_id,omitempty"`
	Metadata      string         `json:"metadata,omitempty"`
	Attachments   []DMAttachment `json:"attachments,omitempty"`
	ReplyTo       *DMReplyTo     `json:"reply_to,omitempty"`
	Referral      *dmReferral    `json:"referral,omitempty"`
	IsUnsupported bool           `json:"is_unsupported,omitempty"`
	IsDeleted     bool           `json:"is_deleted,omitempty"`
}
type dmRecord struct {
	Mid, ThreadID, CustomerID, CustomerName string
	FromMe                                  bool
	FromWebhookEvent                        bool
	SentAt                                  time.Time
	Text                                    string
	Meta                                    dmMetadata
	ReplyToMid                              string
	Raw                                     []byte
	FreshGraphMessage                       bool
	GraphObservedAt                         int64
}
type dmWriter struct {
	store    *store.Store
	sourceID int64
	src      DMSource
}

func dmOwner(src DMSource) string {
	if src.Platform == PlatformInstagram {
		return src.IGID
	}
	return src.PageID
}

func dmGraphObservedKey(sourceID int64, mid string) string {
	return fmt.Sprintf("meta.dm_graph_observed:%d:%s", sourceID, mid)
}

func (w *dmWriter) loadGraphObservedAt(ctx context.Context, mid string, legacy int64) (int64, bool, error) {
	value, found, err := w.store.GetArchiveMarker(ctx, dmGraphObservedKey(w.sourceID, mid))
	if err != nil || !found {
		return legacy, false, err
	}
	observedAt, err := strconv.ParseInt(value, 10, 64)
	if err != nil || observedAt < 0 {
		return 0, false, errors.New("invalid direct message Graph observation marker")
	}
	return observedAt, true, nil
}

func (w *dmWriter) saveGraphObservedAt(ctx context.Context, mid string, observedAt int64) error {
	if observedAt <= 0 {
		return errors.New("invalid direct message Graph observation time")
	}
	return w.store.AdvanceArchiveWatermarkContext(ctx, dmGraphObservedKey(w.sourceID, mid), observedAt, 0)
}

func dmDerivedMetadata(attachments []DMAttachment, reply *DMReplyTo) dmMetadata {
	m := dmMetadata{}
	for _, a := range attachments {
		m.Attachments = append(m.Attachments, dmAttachmentMetadata{Type: a.Type, StickerID: a.Payload.StickerID})
		if a.Type == "story_mention" {
			m.Story = &dmStoryMetadata{Kind: "mention"}
		}
	}
	if reply != nil && reply.Story != nil {
		m.Story = &dmStoryMetadata{ID: reply.Story.ID, Kind: "reply"}
	}
	return m
}

func recordFromNode(src DMSource, m DMMessage) (dmRecord, error) {
	rec := dmRecord{Mid: m.ID, Text: m.Message, Raw: m.Raw, FromMe: m.From.ID == dmOwner(src), CustomerID: m.From.ID, CustomerName: m.From.Name, FreshGraphMessage: m.MessagePresent || m.Message != ""}
	if rec.CustomerName == "" {
		rec.CustomerName = m.From.Username
	}
	if rec.FromMe {
		rec.CustomerID, rec.CustomerName = "", ""
		for _, to := range m.To {
			if to.ID != "" && to.ID != dmOwner(src) {
				rec.CustomerID, rec.CustomerName = to.ID, to.Name
				if rec.CustomerName == "" {
					rec.CustomerName = to.Username
				}
				break
			}
		}
	}
	if m.CreatedTime != "" {
		var err error
		rec.SentAt, err = time.Parse("2006-01-02T15:04:05-0700", m.CreatedTime)
		if err != nil {
			rec.SentAt, err = time.Parse(time.RFC3339, m.CreatedTime)
		}
		if err != nil {
			return dmRecord{}, errors.New("invalid message creation time")
		}
	}
	rec.Meta = dmDerivedMetadata(m.Attachments, m.ReplyTo)
	if m.Story != nil && rec.Meta.Story == nil {
		// Graph exposes the story ID without distinguishing mentions from replies.
		rec.Meta.Story = &dmStoryMetadata{ID: m.Story.ID}
	}
	rec.Meta.Shares = m.Shares
	rec.Meta.IsUnsupported = m.IsUnsupported
	if m.ReplyTo != nil {
		rec.ReplyToMid = m.ReplyTo.Mid
	}
	return rec, nil
}

func recordFromEvent(src DMSource, e messagingEvent) (dmRecord, error) {
	if e.Message == nil || e.Timestamp <= 0 {
		return dmRecord{}, errors.New("invalid messaging event")
	}
	m := e.Message
	rec := dmRecord{Mid: m.Mid, Text: m.Text, CustomerID: e.Sender.ID, CustomerName: e.Sender.Name, FromMe: m.IsEcho, FromWebhookEvent: true, SentAt: time.UnixMilli(e.Timestamp).UTC(), Raw: e.Raw, ReplyToMid: ""}
	if m.IsEcho {
		if e.Sender.ID != dmOwner(src) {
			return dmRecord{}, errors.New("misrouted echo")
		}
		rec.CustomerID, rec.CustomerName = e.Recipient.ID, e.Recipient.Name
	} else if e.Recipient.ID != dmOwner(src) {
		return dmRecord{}, errors.New("misrouted message")
	}
	rec.Meta = dmDerivedMetadata(m.Attachments, m.ReplyTo)
	rec.Meta.Channel = "messaging"
	rec.Meta.IsUnsupported = m.IsUnsupported
	rec.Meta.Referral = m.Referral
	if rec.Meta.Referral == nil {
		rec.Meta.Referral = e.Referral
	}
	if len(m.AppID) > 0 {
		var app string
		if json.Unmarshal(m.AppID, &app) != nil {
			app = string(m.AppID)
		}
		if !ValidAccountID(app) {
			return dmRecord{}, errors.New("invalid echo app ID")
		}
		rec.Meta.EchoAppID = app
	}
	if m.ReplyTo != nil {
		rec.ReplyToMid = m.ReplyTo.Mid
	}
	if len(rec.Raw) == 0 {
		var err error
		rec.Raw, err = json.Marshal(e)
		if err != nil {
			return dmRecord{}, err
		}
	}
	return rec, nil
}

func (w *dmWriter) persist(ctx context.Context, rec dmRecord) (int64, bool, error) {
	if !safeObjectID(rec.Mid) || !ValidAccountID(rec.CustomerID) || rec.CustomerID == dmOwner(w.src) || (rec.ReplyToMid != "" && (!safeObjectID(rec.ReplyToMid) || rec.ReplyToMid == rec.Mid)) {
		return 0, false, errors.New("invalid direct message identity")
	}
	prior, err := w.store.StoredMessagesContext(ctx, w.sourceID, dmRawFormat, []string{rec.Mid, rec.ReplyToMid})
	if err != nil {
		return 0, false, err
	}
	existing, exists := prior[rec.Mid]
	graphObservedAt, graphMarkerFound, err := w.loadGraphObservedAt(ctx, rec.Mid, 0)
	if err != nil {
		return 0, false, err
	}
	key := "psid:" + rec.CustomerID
	if w.src.Platform == PlatformInstagram {
		key = "igsid:" + rec.CustomerID
	}
	conversations, err := w.store.ConversationMetadataBatchContext(ctx, w.sourceID, []string{key})
	if err != nil {
		return 0, false, err
	}
	var conv dmConversationMetadata
	if saved := conversations[key]; saved.Valid {
		if json.Unmarshal([]byte(saved.String), &conv) != nil || conv.Version != dmMetadataVersion {
			return 0, false, errors.New("unsupported direct chat metadata version")
		}
		if conv.AppID != w.src.AppID || conv.PageID != w.src.PageID || conv.IGID != w.src.IGID {
			return 0, false, errors.New("direct chat metadata identity mismatch")
		}
	}
	for _, mid := range []string{rec.Mid, rec.ReplyToMid} {
		if row, ok := prior[mid]; ok {
			ref, err := w.store.MessageConversation(row.ID)
			if err != nil {
				return 0, false, err
			}
			if ref.SourceConversationID != key || ref.Type != "direct_chat" {
				return 0, false, errors.New("message belongs to a different conversation")
			}
		}
	}
	if rec.FreshGraphMessage && rec.GraphObservedAt == 0 {
		rec.GraphObservedAt = time.Now().UTC().UnixMilli()
	}
	meta := rec.Meta
	meta.Version, meta.AppID, meta.PageID, meta.IGID = dmMetadataVersion, w.src.AppID, w.src.PageID, w.src.IGID
	meta.ThreadID = rec.ThreadID
	meta.ReferencedMessageID = rec.ReplyToMid
	var saved sql.NullString
	var legacyGraphObservedAt int64
	if exists {
		saved, err = w.store.GetMessageMetadataContext(ctx, existing.ID)
		if err != nil {
			return 0, false, err
		}
		var old dmMetadata
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &old) != nil || old.Version != dmMetadataVersion {
			return 0, false, errors.New("unsupported direct message metadata version")
		}
		if old.AppID != w.src.AppID || old.PageID != w.src.PageID || old.IGID != w.src.IGID {
			return 0, false, errors.New("direct message metadata identity mismatch")
		}
		legacyGraphObservedAt = old.LegacyGraphObservedAt
		markerNeedsUpdate := !graphMarkerFound
		if old.LegacyGraphObservedAt > graphObservedAt {
			graphObservedAt = old.LegacyGraphObservedAt
			markerNeedsUpdate = true
		}
		if graphObservedAt > 0 && markerNeedsUpdate {
			if err = w.saveGraphObservedAt(ctx, rec.Mid, graphObservedAt); err != nil {
				return 0, false, err
			}
		}
		// Missing projection fields are not provider retractions.
		meta.EditCount = old.EditCount
		meta.ReactionTimes = old.ReactionTimes
		if meta.ThreadID == "" {
			meta.ThreadID = old.ThreadID
		}
		if meta.ReferencedMessageID == "" {
			meta.ReferencedMessageID = old.ReferencedMessageID
		}
		if len(meta.Attachments) == 0 {
			meta.Attachments = old.Attachments
		}
		if meta.Shares == 0 {
			meta.Shares = old.Shares
		}
		if meta.Story == nil {
			meta.Story = old.Story
		} else if old.Story != nil {
			if meta.Story.ID == "" {
				meta.Story.ID = old.Story.ID
			}
			if meta.Story.Kind == "" {
				meta.Story.Kind = old.Story.Kind
			}
		}
		if meta.Referral == nil {
			meta.Referral = old.Referral
		}
		meta.IsUnsupported = meta.IsUnsupported || old.IsUnsupported
		if meta.EchoAppID == "" {
			meta.EchoAppID = old.EchoAppID
		}
		if old.Channel != "" {
			meta.Channel = old.Channel
		}
		if meta.BroadcastID == "" {
			meta.BroadcastID = old.BroadcastID
		}
		if meta.CampaignID == "" {
			meta.CampaignID = old.CampaignID
		}
		if existing.SentAt.Valid {
			rec.SentAt = existing.SentAt.Time
		}
		if existing.SourceIsFromMe.Valid {
			rec.FromMe = existing.SourceIsFromMe.Bool
		}
		// A Graph response without a message field does not retract archived text.
		// Webhook replays also cannot replace a newer Graph observation, even if
		// no edit webhook was observed. Explicitly empty Graph messages remain
		// authoritative because FreshGraphMessage preserves field presence.
		preserveText := !rec.FreshGraphMessage && !rec.FromWebhookEvent
		if old.EditCount > 0 && (rec.FromWebhookEvent || !rec.FreshGraphMessage) {
			preserveText = true
		}
		if rec.FromWebhookEvent && graphObservedAt > 0 && rec.SentAt.UnixMilli() <= graphObservedAt {
			preserveText = true
		}
		if rec.FreshGraphMessage && graphObservedAt > rec.GraphObservedAt {
			preserveText = true
		}
		if preserveText {
			rec.Text, _, err = w.store.GetMessageBodyAndRawContext(ctx, existing.ID)
			if err != nil {
				return 0, false, err
			}
		}
		if len(existing.Raw) > 0 {
			rec.Raw = existing.Raw
		}
	}
	if exists && rec.CustomerName == "" {
		titles, err := w.store.ConversationTitlesBatchContext(ctx, w.sourceID, []string{key})
		if err != nil {
			return 0, false, err
		}
		rec.CustomerName = titles[key]
	}
	customer, err := ResolveDMActor(ctx, w.store, w.sourceID, w.src, rec.CustomerID, rec.CustomerName)
	if err != nil {
		return 0, false, err
	}
	owner, err := ResolveDMActor(ctx, w.store, w.sourceID, w.src, dmOwner(w.src), "")
	if err != nil {
		return 0, false, err
	}
	sender, recipient, fromName := customer, owner, rec.CustomerName
	if rec.FromMe {
		sender, recipient, fromName = owner, customer, ""
	}
	if exists && existing.SenderID.Valid {
		sender = existing.SenderID.Int64
	}
	msg := &store.Message{SourceID: w.sourceID, SourceMessageID: rec.Mid, MessageType: w.src.Platform.SourceType(), SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: rec.SentAt, Valid: !rec.SentAt.IsZero()}, IsFromMe: rec.FromMe, Snippet: sql.NullString{String: metaMessageSnippet(rec.Text), Valid: true}, SizeEstimate: int64(len(rec.Text))}
	blob, err := json.Marshal(meta)
	if err != nil {
		return 0, false, err
	}
	data := &store.MessagePersistData{
		Message: msg, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "direct_chat", Title: rec.CustomerName, PreserveExistingParticipants: true, Participants: []store.ConversationParticipantRef{{ParticipantID: customer, Role: "member"}, {ParticipantID: owner, Role: "member"}}},
		Metadata: &sql.NullString{String: string(blob), Valid: true}, BodyText: sql.NullString{String: rec.Text, Valid: true}, RawMIME: rec.Raw, RawFormat: dmRawFormat, PreserveLabels: true,
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{fromName}}, {Type: "to", ParticipantIDs: []int64{recipient}}}, FTS: &store.FTSDoc{Body: rec.Text, FromAddr: fromName},
	}
	unchanged := false
	if exists {
		unchanged, err = w.snapshotMatches(ctx, existing, data)
		if err != nil {
			return 0, false, err
		}
	}
	id := existing.ID
	metadataUnchanged := saved.Valid == data.Metadata.Valid && meetingarchive.JSONEvidenceEqual([]byte(saved.String), []byte(data.Metadata.String))
	if !unchanged || !metadataUnchanged {
		if rec.FreshGraphMessage {
			watermark := store.ArchiveWatermark{
				Key:        dmGraphObservedKey(w.sourceID, rec.Mid),
				ObservedAt: rec.GraphObservedAt,
				Floor:      legacyGraphObservedAt,
			}
			var stale bool
			id, stale, err = w.store.PersistMessageContextWithArchiveWatermark(ctx, data, watermark)
			if err != nil {
				return 0, false, err
			}
			if stale {
				return existing.ID, false, nil
			}
		} else {
			id, err = w.store.PersistMessageContext(ctx, data)
		}
		if err != nil {
			return 0, false, err
		}
	} else if rec.FreshGraphMessage {
		if err = w.saveGraphObservedAt(ctx, rec.Mid, rec.GraphObservedAt); err != nil {
			return id, !exists, err
		}
	}
	ref, err := w.store.MessageConversation(id)
	if err != nil {
		return id, !exists, err
	}
	conv.Version, conv.AppID, conv.PageID, conv.IGID = dmMetadataVersion, w.src.AppID, w.src.PageID, w.src.IGID
	if meta.ThreadID != "" {
		conv.ThreadID = meta.ThreadID
	}
	blob, err = json.Marshal(conv)
	if err != nil {
		return id, !exists, err
	}
	if err = w.store.SetConversationMetadataContext(ctx, ref.ConversationID, sql.NullString{String: string(blob), Valid: true}); err != nil {
		return id, !exists, err
	}
	return id, !exists, w.store.ResolveMetaMessageRepliesContext(ctx, w.sourceID, id, dmMetadataVersion)
}

func (w *dmWriter) snapshotMatches(ctx context.Context, prior store.StoredMessage, data *store.MessagePersistData) (bool, error) {
	msg := data.Message
	if prior.SenderID != msg.SenderID || !prior.SourceIsFromMe.Valid || prior.SourceIsFromMe.Bool != msg.IsFromMe ||
		prior.MessageType != msg.MessageType || prior.Subject != msg.Subject ||
		prior.SentAt.Valid != msg.SentAt.Valid || (prior.SentAt.Valid && !prior.SentAt.Time.Equal(msg.SentAt.Time)) ||
		prior.Snippet != msg.Snippet || prior.SizeEstimate != msg.SizeEstimate || !bytes.Equal(prior.Raw, data.RawMIME) {
		return false, nil
	}
	matches, err := w.store.MessageContentMatchesContext(ctx, prior.ID, data.BodyText, *data.FTS)
	if err != nil || !matches {
		return matches, err
	}
	return w.store.MessageRecipientsMatchContext(ctx, prior.ID, data.Recipients)
}
