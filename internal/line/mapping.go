package line

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

func bodyText(m Message) string {
	switch m.Type {
	case "text":
		return m.Text
	case "sticker":
		parts := []string{"[Sticker]"}
		if m.Text != "" {
			parts = append(parts, m.Text)
		}
		if len(m.Keywords) > 0 {
			parts = append(parts, strings.Join(m.Keywords, ", "))
		}
		return strings.Join(parts, "\n")
	case "location":
		parts := []string{}
		if m.Title != "" {
			parts = append(parts, m.Title)
		}
		if m.Address != "" {
			parts = append(parts, m.Address)
		}
		parts = append(parts, strconv.FormatFloat(m.Latitude, 'f', -1, 64)+","+strconv.FormatFloat(m.Longitude, 'f', -1, 64))
		return strings.Join(parts, "\n")
	case "image", "video", "audio", "file":
		if m.FileName != "" {
			return m.FileName
		}
		return "[" + m.Type + "]"
	default:
		return "[LINE " + m.Type + "]"
	}
}

func metadataFor(e Event, prior sql.NullString) (sql.NullString, error) {
	metadata := map[string]jsontext.Value{}
	if prior.Valid && prior.String != "" {
		if err := json.Unmarshal([]byte(prior.String), &metadata); err != nil {
			return sql.NullString{}, err
		}
	}
	if metadata == nil {
		metadata = map[string]jsontext.Value{}
	}
	if e.Type == "messageEdited" {
		delete(metadata, "emojis")
		delete(metadata, "mention")
	}
	var native map[string]jsontext.Value
	if e.Message != nil {
		if err := json.Unmarshal(e.Message.Raw, &native); err != nil {
			return sql.NullString{}, err
		}
		maps.Copy(metadata, native)
	}
	eventID, err := json.Marshal(e.WebhookEventID)
	if err != nil {
		return sql.NullString{}, err
	}
	metadata["webhookEventId"] = eventID
	if e.Type == "messageEdited" {
		stamp, err := json.Marshal(e.Timestamp)
		if err != nil {
			return sql.NullString{}, err
		}
		metadata["line_edited_at"] = stamp
		metadata["line_edit_event_id"] = eventID
	}
	raw, err := json.Marshal(metadata)
	return sql.NullString{String: string(raw), Valid: true}, err
}

// message returns zero when a deleted message needs no provider enrichment.
func (p *Processor) message(ctx context.Context, e Event, previous store.MessageMetadataRecord, key, kind string) (int64, error) {
	var messageID int64
	var err error
	if previous.ID != 0 {
		if !previous.SourceConversationID.Valid || previous.SourceConversationID.String != key {
			return 0, invalidEvent("LINE message identity belongs to a different chat")
		}
		messageID = previous.ID
	} else {
		name := ""
		if e.Source.UserID != "" {
			if cached, fresh, err := p.cachedProfile(ctx, e.Source.UserID); err != nil {
				return 0, err
			} else if fresh {
				name = cached.DisplayName
			}
		}
		pid, err := p.participant(ctx, key, e.Source.UserID, name)
		if err != nil {
			return 0, err
		}
		metadata, err := metadataFor(e, sql.NullString{})
		if err != nil {
			return 0, err
		}
		text := bodyText(*e.Message)
		data := &store.MessagePersistData{
			Message:      &store.Message{SourceID: p.sourceID, SourceMessageID: e.Message.ID, MessageType: SourceType, SentAt: sql.NullTime{Time: time.UnixMilli(e.Timestamp).UTC(), Valid: true}, SenderID: sql.NullInt64{Int64: pid, Valid: pid > 0}, Snippet: sql.NullString{String: meetingarchive.Snippet(text), Valid: text != ""}, SizeEstimate: int64(len(text)), PreserveAttachmentStats: true},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: kind, Title: p.groupNames[key], Participants: []store.ConversationParticipantRef{{ParticipantID: pid, Role: "member"}}, PreserveExistingParticipants: true},
			Metadata:     &metadata, BodyText: sql.NullString{String: text, Valid: true}, RawMIME: e.Raw, RawFormat: "line_webhook_json", Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{pid}, DisplayNames: []string{name}}}, PreserveLabels: true, FTS: &store.FTSDoc{Body: text},
		}
		recipient, err := p.store.EnsureParticipantByIdentifierContext(ctx, "line_user_id", UserIdentifier(p.channelID, p.botID), "")
		if err != nil {
			return 0, err
		}
		data.Recipients = append(data.Recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{recipient}})
		if e.Source.Type == "group" || e.Source.Type == "room" {
			identifier := participantIdentifier(p.channelID, key, e.Source.UserID)
			messageID, err = p.store.PersistLineGroupMessageContext(ctx, data, identifier)
		} else {
			messageID, err = p.store.PersistMessageContext(ctx, data)
		}
		if err != nil {
			return 0, err
		}
		p.recordMutation(ctx, 1, 0)
	}
	if err := p.reconcileMutations(ctx, e.Message.ID); err != nil {
		return 0, err
	}
	deleted, err := p.store.LineMessageDeletedContext(ctx, messageID)
	if err != nil {
		return 0, err
	}
	if deleted {
		return 0, p.media(ctx, messageID, e, true)
	}
	return messageID, nil
}

func (p *Processor) enrichMessage(ctx context.Context, e Event, messageID int64, previous store.MessageMetadataRecord, key, kind string) error {
	mediaErr := p.media(ctx, messageID, e, false)
	// Provider requests run after the whole batch has committed its local writes.
	profile, profileErr := p.profile(ctx, e.Source, e.Source.UserID)
	var senderSnapshotErr error
	if profileErr == nil && profile.DisplayName != "" {
		senderSnapshotErr = p.store.UpdateLineMessageSenderDisplayNameContext(ctx, p.sourceID, messageID, profile.DisplayName)
	}
	if previous.ID != 0 {
		p.recordMutation(ctx, 0, 1)
	}
	title := p.groupNames[key]
	var titleErr error
	if kind == "direct_chat" {
		title = profile.DisplayName
	} else if e.Source.Type == "group" && title == "" {
		var summary GroupSummary
		summary, titleErr = p.client.GroupSummary(ctx, e.Source.GroupID)
		if apiErr, ok := errors.AsType[*APIError](titleErr); ok && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound) {
			titleErr = nil
		}
		if titleErr == nil {
			title = summary.GroupName
			p.groupNames[key] = title
		}
	}
	if title != "" {
		if _, err := p.store.EnsureConversationWithTypeContext(ctx, p.sourceID, key, kind, title); err != nil {
			return err
		}
	}
	return errors.Join(mediaErr, profileErr, senderSnapshotErr, retryEvent(titleErr))
}

func (p *Processor) edit(ctx context.Context, e Event) error {
	if e.Source.Type != "group" || e.Message.Type != "text" {
		return invalidEvent("invalid LINE text edit")
	}
	key, _, err := e.Source.ConversationID()
	if err != nil {
		return invalidEvent("invalid LINE edit source")
	}
	prior, err := p.store.LineMessageContext(ctx, p.sourceID, e.Message.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return errTargetMissing
	}
	if err != nil {
		return err
	}
	if !prior.SourceConversationID.Valid || prior.SourceConversationID.String != key {
		return invalidEvent("LINE edit target belongs to a different chat")
	}
	var watermark struct {
		Timestamp int64  `json:"line_edited_at"`
		EventID   string `json:"line_edit_event_id"`
	}
	if prior.Metadata.Valid {
		if err = json.Unmarshal([]byte(prior.Metadata.String), &watermark); err != nil {
			return err
		}
	}
	if e.Timestamp < watermark.Timestamp || e.Timestamp == watermark.Timestamp && e.WebhookEventID <= watermark.EventID {
		return nil
	}
	metadata, err := metadataFor(e, prior.Metadata)
	if err != nil {
		return err
	}
	text := e.Message.Text
	err = p.store.ReplaceMessageTextContext(ctx, prior.ID, text, meetingarchive.Snippet(text), true, metadata)
	if err != nil {
		return err
	}
	p.recordMutation(ctx, 0, 1)
	return nil
}
