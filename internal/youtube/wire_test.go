package youtube

import (
	"context"
	"encoding/json/v2"
	"go.kenn.io/msgvault/internal/store"
	yt "google.golang.org/api/youtube/v3"
	"time"
)

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
	ID      string         `json:"id"`
	Snippet CommentSnippet `json:"snippet"`
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

func persistVideo(ctx context.Context, st *store.Store, sourceID int64, v Video) (int64, messageChange, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, messageUnchanged, err
	}
	var item yt.Video
	if err = json.Unmarshal(b, &item); err != nil {
		return 0, messageUnchanged, err
	}
	return persistVideoSDK(ctx, st, sourceID, "UCfixture", &item)
}
func persistComment(ctx context.Context, st *store.Store, sourceID int64, v Video, c Comment) (int64, messageChange, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return 0, messageUnchanged, err
	}
	var item yt.Comment
	if err = json.Unmarshal(b, &item); err != nil {
		return 0, messageUnchanged, err
	}
	return persistCommentSDK(ctx, st, sourceID, "UCfixture", v.ID, &item)
}
