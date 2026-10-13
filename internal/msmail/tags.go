package msmail

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/msgraph"
)

type categorySnapshot struct {
	ID         string    `json:"id"`
	Categories *[]string `json:"categories"`
	ETag       string    `json:"@odata.etag"`
}

func (c *Client) readCategories(ctx context.Context, id string) (categorySnapshot, error) {
	var snapshot categorySnapshot
	err := c.GetJSON(ctx, "/me/messages/"+url.PathEscape(id)+"?$select=id,categories", &snapshot)
	if err != nil {
		return snapshot, err
	}
	if snapshot.ID != id {
		return snapshot, emailtags.Failure("stale_identity", "Microsoft returned a different message; sync and retry", nil, nil)
	}
	if snapshot.Categories == nil {
		return snapshot, errors.New("microsoft did not return a complete category collection")
	}
	return snapshot, nil
}

func observedCategories(result *emailtags.MessageTagResult, categories []string) {
	result.Tags = append([]string{}, categories...)
	result.AvailableTags = make([]emailtags.MessageTag, 0, len(categories))
	for _, name := range categories {
		result.AvailableTags = append(result.AvailableTags, emailtags.MessageTag{ID: name, Name: name})
	}
}

// MessageTags reads or changes Outlook categories on one immutable message ID.
// A conditional write preserves unrelated categories from the fresh snapshot.
func (c *Client) MessageTags(ctx context.Context, id string, input *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
	if id == "" {
		return nil, emailtags.Failure("stale_identity", "message has no Microsoft identity; sync the account", nil, nil)
	}
	var change emailtags.MessageTagChange
	if input != nil {
		change = *input
	}
	snapshot, err := c.readCategories(ctx, id)
	if err != nil {
		code := "provider_read_failed"
		if errors.Is(err, msgraph.ErrForbidden) {
			code = "insufficient_scope"
		} else if errors.Is(err, msgraph.ErrNotFound) {
			code = "stale_identity"
		}
		if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
			return nil, failure
		}
		return nil, emailtags.Failure(code, "cannot read Microsoft categories; check account access and sync", nil, err)
	}
	result := &emailtags.MessageTagResult{Provider: SourceType}
	observedCategories(result, *snapshot.Categories)
	result.Before = slices.Clone(result.Tags)
	if input == nil {
		result.Verified = true
		return result, nil
	}
	result.DryRun = change.DryRun
	if change.DryRun {
		result.Tags = emailtags.Project(result.Tags, change, true)
		return result, nil
	}
	add, remove := emailtags.Delta(result.Tags, change, true)
	if len(add)+len(remove) > 0 {
		if snapshot.ETag == "" {
			return result, emailtags.Failure("stale_identity", "Microsoft did not return a message version; read tags before retrying", result, nil)
		}
		body := struct {
			Categories []string `json:"categories"`
		}{Categories: emailtags.Project(result.Tags, change, true)}
		response, err := c.SendNoRetry(ctx, http.MethodPatch, "/me/messages/"+url.PathEscape(id), body, snapshot.ETag)
		if err != nil {
			if _, throttled := errors.AsType[*msgraph.ThrottledError](err); throttled {
				return result, emailtags.Failure("provider_write_failed", "Microsoft rejected the category update due to throttling; retry after the limit clears", result, err)
			}
			status, _ := errors.AsType[*msgraph.StatusError](err)
			code := "remote_unknown"
			switch {
			case errors.Is(err, msgraph.ErrPreconditionFailed), errors.Is(err, msgraph.ErrNotFound):
				code = "stale_identity"
			case errors.Is(err, msgraph.ErrForbidden):
				code = "insufficient_scope"
			case status != nil && status.StatusCode == http.StatusBadRequest:
				code = "invalid_tag"
			}
			return result, emailtags.Failure(code, "Microsoft category update failed; read current tags before retrying", result, err)
		}
		var observed categorySnapshot
		if json.Unmarshal(response, &observed) == nil && observed.ID == id && observed.Categories != nil {
			observedCategories(result, *observed.Categories)
		}
		after, err := c.readCategories(ctx, id)
		if err != nil {
			return result, emailtags.Failure("verification_failed", "Microsoft accepted the update but readback failed; read tags before retrying", result, err)
		}
		observedCategories(result, *after.Categories)
	}
	if !emailtags.Verify(result.Tags, change, true) {
		return result, emailtags.Failure("verification_failed", "Microsoft readback does not match the requested categories; read tags before retrying", result, nil)
	}
	result.Verified = true
	return result, nil
}
