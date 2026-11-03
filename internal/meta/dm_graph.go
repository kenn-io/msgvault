package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strings"
)

const dmGraphFields = "fields"

type DMParticipant struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
}
type Conversation struct {
	ID, UpdatedTime string
	Participants    []DMParticipant
}
type ConversationPage struct {
	Data  []Conversation
	After string
}
type DMStory struct {
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}
type DMReplyTo struct {
	Mid   string   `json:"mid,omitempty"`
	Story *DMStory `json:"story,omitempty"`
}
type DMAttachment struct {
	Type    string `json:"type"`
	Payload struct {
		URL       string         `json:"url,omitempty"`
		StickerID jsontext.Value `json:"sticker_id,omitempty"`
	} `json:"payload,omitzero"`
}

// Graph message nodes use attachment data, unlike webhook type/payload pairs.
type dmGraphAttachment struct {
	MIMEType  string `json:"mime_type"`
	FileURL   string `json:"file_url"`
	ImageData *struct {
		RenderAsSticker bool `json:"render_as_sticker"`
	} `json:"image_data"`
	VideoData       jsontext.Value `json:"video_data"`
	GenericTemplate jsontext.Value `json:"generic_template"`
}

func (a dmGraphAttachment) attachment() DMAttachment {
	var out DMAttachment
	switch {
	case a.ImageData != nil:
		out.Type = "image"
		if a.ImageData.RenderAsSticker {
			out.Type = "sticker"
		}
	case len(a.VideoData) > 0 && string(a.VideoData) != "null":
		out.Type = "video"
	case strings.HasPrefix(a.MIMEType, "image/"):
		out.Type = "image"
	case strings.HasPrefix(a.MIMEType, "video/"):
		out.Type = "video"
	case strings.HasPrefix(a.MIMEType, "audio/"):
		out.Type = "audio"
	case len(a.GenericTemplate) > 0 && string(a.GenericTemplate) != "null":
		out.Type = "generic_template"
	default:
		out.Type = "file"
	}
	return out
}

type DMMessage struct {
	ID, CreatedTime string
	From            DMParticipant
	To              []DMParticipant
	Message         string
	MessagePresent  bool
	Attachments     []DMAttachment
	Shares          int
	Story           *DMStory
	ReplyTo         *DMReplyTo
	IsUnsupported   bool
	Raw             []byte
}

type DMAPI interface {
	VerifyPage(ctx context.Context, expectedPage string) (Page, error)
	LinkedInstagramAccount(ctx context.Context, pageID, expectedIGID string) (IGUser, error)
	Conversations(ctx context.Context, page, platform, after string) (ConversationPage, error)
	ConversationMessageIDs(ctx context.Context, thread string) ([]string, bool, error)
	Message(ctx context.Context, mid string) (DMMessage, error)
}

// Conversations follows only a bounded opaque cursor, never a provider URL.
func (c *Client) Conversations(ctx context.Context, page, platform, after string) (ConversationPage, error) {
	if !ValidAccountID(page) || (platform != "messenger" && platform != "instagram") {
		return ConversationPage{}, errors.New("invalid inbox Page or platform")
	}
	type wireConversation struct {
		ID           string `json:"id"`
		UpdatedTime  string `json:"updated_time"`
		Participants struct {
			Data []DMParticipant `json:"data"`
		} `json:"participants"`
	}
	wire, err := readObjectPage(ctx, c, page+"/conversations", after, url.Values{"platform": {platform}, dmGraphFields: {"id,updated_time,participants"}, "limit": {"25"}}, func(v wireConversation) string { return v.ID })
	if err != nil {
		return ConversationPage{}, err
	}
	out := ConversationPage{After: wire.After}
	for _, v := range wire.Data {
		out.Data = append(out.Data, Conversation{ID: v.ID, UpdatedTime: v.UpdatedTime, Participants: v.Participants.Data})
	}
	return out, nil
}

// ConversationMessageIDs deliberately reads no continuation: Meta exposes only
// the newest twenty message nodes for this archive's bounded backfill.
func (c *Client) ConversationMessageIDs(ctx context.Context, thread string) ([]string, bool, error) {
	if !safeObjectID(thread) {
		return nil, false, errors.New("invalid Graph thread ID")
	}
	var wire struct {
		Messages struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		} `json:"messages"`
	}
	if err := c.get(ctx, url.PathEscape(thread), url.Values{dmGraphFields: {"messages"}}, &wire); err != nil {
		return nil, false, err
	}
	if wire.Messages.Data == nil {
		return nil, false, errors.New("graph thread missing message IDs")
	}
	capped := len(wire.Messages.Data) > dmBackfillCap || wire.Messages.Paging.Next != ""
	ids := make([]string, 0, min(dmBackfillCap, len(wire.Messages.Data)))
	for _, v := range wire.Messages.Data[:min(dmBackfillCap, len(wire.Messages.Data))] {
		if !safeObjectID(v.ID) {
			return nil, false, errors.New("invalid Graph message ID")
		}
		ids = append(ids, v.ID)
	}
	return ids, capped, nil
}

func (c *Client) Message(ctx context.Context, mid string) (DMMessage, error) {
	if !safeObjectID(mid) {
		return DMMessage{}, errors.New("invalid Graph message ID")
	}
	var raw jsontext.Value
	if err := c.get(ctx, url.PathEscape(mid), url.Values{dmGraphFields: {"id,created_time,from,to,message,attachments,shares,story,reply_to,is_unsupported"}}, &raw); err != nil {
		return DMMessage{}, err
	}
	var wire struct {
		ID          string        `json:"id"`
		CreatedTime string        `json:"created_time"`
		From        DMParticipant `json:"from"`
		To          struct {
			Data []DMParticipant `json:"data"`
		} `json:"to"`
		Message     *string `json:"message"`
		Attachments struct {
			Data []dmGraphAttachment `json:"data"`
		} `json:"attachments"`
		Shares struct {
			Data []jsontext.Value `json:"data"`
		} `json:"shares"`
		Story         *DMStory   `json:"story"`
		ReplyTo       *DMReplyTo `json:"reply_to"`
		IsUnsupported bool       `json:"is_unsupported"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.ID != mid {
		return DMMessage{}, errors.New("invalid Graph message response")
	}
	attachments := make([]DMAttachment, 0, len(wire.Attachments.Data))
	for _, a := range wire.Attachments.Data {
		attachments = append(attachments, a.attachment())
	}
	message, messagePresent := "", wire.Message != nil
	if messagePresent {
		message = *wire.Message
	}
	return DMMessage{ID: wire.ID, CreatedTime: wire.CreatedTime, From: wire.From, To: wire.To.Data, Message: message, MessagePresent: messagePresent, Attachments: attachments, Shares: len(wire.Shares.Data), Story: wire.Story, ReplyTo: wire.ReplyTo, IsUnsupported: wire.IsUnsupported, Raw: append([]byte(nil), raw...)}, nil
}
