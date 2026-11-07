package youtube

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// CheckReply refreshes ownership and public reply eligibility immediately before
// dispatch. An archived moderation observation alone cannot authorize a send.
func (c *Client) CheckReply(ctx context.Context, target ReplyTarget) error {
	var channels struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/channels", url.Values{"part": {"id"}, "mine": {"true"}}, &channels); err != nil {
		return err
	}
	owned := false
	for _, channel := range channels.Items {
		owned = owned || channel.ID == target.ChannelID
	}
	if !owned {
		return fmt.Errorf("%w: credential channel no longer matches archived channel", ErrReplyRejected)
	}
	// Moderation status is omitted for id-filtered comment requests. A
	// video-scoped listing supplies parent-specific evidence; the published
	// filter itself is never treated as that evidence (a sibling can match it).
	query := url.Values{"part": {"snippet"}, "videoId": {target.VideoID}, "moderationStatus": {"published"}, "textFormat": {"plainText"}, "maxResults": {"100"}, "order": {"time"}}
	seen := map[string]bool{}
	for {
		var result page[thread]
		if err := c.get(ctx, "/commentThreads", query, &result); err != nil {
			return err
		}
		for _, item := range result.Items {
			if item.ID != target.ThreadID {
				continue
			}
			current := item.Snippet
			if current.VideoID != target.VideoID || current.TopLevelComment.ID != target.ParentID || current.CanReply == nil || !*current.CanReply || current.TopLevelComment.Snippet.ModerationStatus != "published" {
				return fmt.Errorf("%w: current parent is not eligible for a public reply", ErrReplyRejected)
			}
			return nil
		}
		if result.NextPageToken == "" {
			return fmt.Errorf("%w: parent thread unavailable in current published listing", ErrReplyRejected)
		}
		if seen[result.NextPageToken] {
			return errors.New("YouTube repeated a results page; nothing sent")
		}
		seen[result.NextPageToken] = true
		query.Set("pageToken", result.NextPageToken)
	}
}

// InsertReply performs one insert attempt; callers must durably claim their
// intent first. No HTTP status or transport failure triggers an automatic retry.
func (c *Client) InsertReply(ctx context.Context, parentID, body string) (Comment, error) {
	if !validCommentID(parentID) {
		return Comment{}, errors.New("invalid YouTube reply parent")
	}
	payload := struct {
		Snippet struct {
			ParentID string `json:"parentId"`
			Text     string `json:"textOriginal"`
		} `json:"snippet"`
	}{}
	payload.Snippet.ParentID = parentID
	payload.Snippet.Text = body
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Comment{}, errors.New("encode YouTube reply")
	}
	response, err := c.request(ctx, http.MethodPost, "/comments", url.Values{"part": {"snippet"}}, bytes.NewReader(encoded))
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			return Comment{}, fmt.Errorf("%w: %w", ErrReplyRejected, err)
		}
		return Comment{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var comment Comment
	if err = json.UnmarshalRead(io.LimitReader(response.Body, 2<<20), &comment); err != nil {
		return Comment{}, errors.New("invalid YouTube reply response; delivery may have occurred")
	}
	return comment, nil
}

// GetReply retrieves explicit provider evidence without creating a comment.
func (c *Client) GetReply(ctx context.Context, commentID string) (Comment, error) {
	if !validCommentID(commentID) {
		return Comment{}, errors.New("invalid YouTube comment ID")
	}
	var result struct {
		Items []Comment `json:"items"`
	}
	if err := c.get(ctx, "/comments", url.Values{"part": {"snippet"}, "id": {commentID}, "textFormat": {"plainText"}}, &result); err != nil {
		return Comment{}, err
	}
	if len(result.Items) != 1 || result.Items[0].ID != commentID {
		return Comment{}, errors.New("provider comment not found; uncertain receipt remains blocked")
	}
	return result.Items[0], nil
}
