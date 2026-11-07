package tiktok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *Client) Conversations(ctx context.Context, account, kind, cursor string, limit int) (ConversationPage, error) {
	var page ConversationPage
	if (kind != "SINGLE" && kind != "STRANGER") || limit < 1 || limit > 100 {
		return page, errors.New("invalid TikTok conversation query")
	}
	err := c.request(ctx, "/business/message/conversation/list/", url.Values{"business_id": {account}, "conversation_type": {kind}, "cursor": {initialCursor(cursor)}, "limit": {strconv.Itoa(limit)}}, nil, &page, true, true)
	if err == nil && len(page.Conversations) > limit {
		err = errors.New("TikTok conversation page exceeds requested size")
	}
	return page, err
}
func (c *Client) Messages(ctx context.Context, account, conversation string) (MessagePage, error) {
	var page MessagePage
	err := c.request(ctx, "/business/message/content/list/", url.Values{"business_id": {account}, "conversation_id": {conversation}}, nil, &page, true, true)
	if err == nil && len(page.Messages) > 20 {
		err = errors.New("TikTok message page exceeds the 20-message history window")
	}
	if err == nil {
		for i := range page.Messages {
			m := &page.Messages[i]
			if m.ConversationID == "" {
				m.ConversationID = ID(conversation)
			}
			if string(m.ConversationID) != conversation {
				err = errors.New("TikTok message conversation mismatch")
				break
			}
		}
	}
	return page, err
}

type MessageMediaResponse struct {
	Body        io.ReadCloser
	ContentType string
}

func safeMediaURL(target *url.URL) bool {
	if target.Scheme != "https" || target.User != nil || target.Port() != "" || target.Fragment != "" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	for _, domain := range []string{"tiktokcdn.com", "tiktokcdn-us.com", "tiktokv.com", "byteoversea.com", "ibytedtos.com", "byteimg.com", "ibyteimg.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// OpenMessageMedia mints a fresh 24-hour URL for image/video media and reads
// sticker/emoji URLs immediately. Credentials never leave trusted media hosts.
func (c *Client) OpenMessageMedia(ctx context.Context, account string, message BusinessMessage, maxBytes int64) (*MessageMediaResponse, error) {
	var mediaURL string
	kind := message.Kind()
	switch kind {
	case "image", "video":
		media := message.Image
		if kind == "video" {
			media = message.Video
		}
		if media == nil || media.ID == "" {
			return nil, errors.New("TikTok media identifier is missing")
		}
		var result struct {
			DownloadURL string `json:"download_url"`
		}
		err := c.request(ctx, "/business/message/media/download/", nil, map[string]string{"business_id": account, "conversation_id": string(message.ConversationID), "message_id": string(message.ID), "media_id": string(media.ID), "media_type": strings.ToUpper(kind)}, &result, true, true)
		if err != nil {
			return nil, err
		}
		mediaURL = result.DownloadURL
	case "sticker":
		if message.Sticker != nil {
			mediaURL = message.Sticker.URL
		}
	case "emoji":
		if message.Emoji != nil {
			mediaURL = message.Emoji.URL
		}
	default:
		return nil, errors.New("TikTok message has no downloadable media")
	}
	target, err := url.Parse(mediaURL)
	if err != nil || !safeMediaURL(target) {
		return nil, errors.New("TikTok media URL is missing or outside trusted media hosts")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("invalid TikTok media request")
	}
	req.Header.Set("X-User", token)
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("TikTok media download failed")
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, errors.New("TikTok media download was rejected")
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		_ = resp.Body.Close()
		return nil, ErrMediaTooLarge
	}
	return &MessageMediaResponse{Body: resp.Body, ContentType: resp.Header.Get("Content-Type")}, nil
}

var ErrMediaTooLarge = errors.New("TikTok media exceeds the configured size limit")
