// Package youtube archives an explicitly selected owned channel through the Data API.
package youtube

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"time"
)

const SourceType = "youtube"

type VideoSnippet struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	ChannelID   string    `json:"channelId"`
	PublishedAt time.Time `json:"publishedAt"`
}

type Video struct {
	ID      string       `json:"id"`
	Snippet VideoSnippet `json:"snippet"`
	Status  struct {
		PrivacyStatus string `json:"privacyStatus"`
	} `json:"status"`
}

type CommentSnippet struct {
	VideoID           string `json:"videoId"`
	ParentID          string `json:"parentId"`
	TextOriginal      string `json:"textOriginal"`
	TextDisplay       string `json:"textDisplay"`
	AuthorDisplayName string `json:"authorDisplayName"`
	AuthorChannelID   struct {
		Value string `json:"value"`
	} `json:"authorChannelId"`
	PublishedAt      time.Time `json:"publishedAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	ModerationStatus string    `json:"moderationStatus"`
}

type Comment struct {
	ID       string         `json:"id"`
	Snippet  CommentSnippet `json:"snippet"`
	ThreadID string         `json:"-"`
	CanReply *bool          `json:"-"`
}

type thread struct {
	ID      string `json:"id"`
	Snippet struct {
		VideoID         string  `json:"videoId"`
		TopLevelComment Comment `json:"topLevelComment"`
		TotalReplyCount int     `json:"totalReplyCount"`
		CanReply        *bool   `json:"canReply"`
	} `json:"snippet"`
	Replies struct {
		Comments []Comment `json:"comments"`
	} `json:"replies"`
}

type page[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

func (p *page[T]) UnmarshalJSON(b []byte) error {
	var wire struct {
		Items         *[]T   `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	if wire.Items == nil {
		return errors.New("YouTube listing has no items array")
	}
	p.Items = *wire.Items
	p.NextPageToken = wire.NextPageToken
	return nil
}

func validID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func SourceIdentifier(channelID string) string {
	if !validID(channelID) {
		return ""
	}
	return "https://www.youtube.com/channel/" + channelID
}

// ParseSourceIdentifier accepts only an immutable canonical channel URL.
func ParseSourceIdentifier(identifier string) (channelID string, ok bool) {
	channelID, ok = strings.CutPrefix(identifier, "https://www.youtube.com/channel/")
	if !ok || SourceIdentifier(channelID) != identifier {
		return "", false
	}
	return channelID, true
}
