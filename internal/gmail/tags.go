package gmail

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"go.kenn.io/msgvault/internal/emailtags"
)

// MessageTagClient is an optional capability of Gmail clients, outside the sync API.
type MessageTagClient interface {
	MessageTags(ctx context.Context, id string, change *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error)
	Close() error
}

func (c *Client) readTagIDs(ctx context.Context, id string) ([]string, error) {
	path := fmt.Sprintf("/users/%s/messages/%s?format=metadata&fields=id,labelIds", url.PathEscape(c.userID), url.PathEscape(id))
	data, err := c.request(ctx, OpMessagesGet, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return decodeTagIDs(data, id)
}

func decodeTagIDs(data []byte, id string) ([]string, error) {
	var message struct {
		ID       string   `json:"id"`
		LabelIDs []string `json:"labelIds"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return nil, err
	}
	if message.ID != id {
		return nil, errors.New("gmail returned an unexpected message identity")
	}
	if message.LabelIDs == nil {
		message.LabelIDs = []string{}
	}
	return message.LabelIDs, nil
}

// MessageTags reads or changes existing user labels on one Gmail message.
// System labels are returned but cannot be edited by this capability.
func (c *Client) MessageTags(ctx context.Context, id string, input *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
	if id == "" {
		return nil, emailtags.Failure("stale_identity", "message has no Gmail identity; sync the account", nil, nil)
	}
	var change emailtags.MessageTagChange
	if input != nil {
		change = *input
	}
	labels, err := c.ListLabels(ctx)
	if err != nil {
		return nil, emailtags.Failure("provider_read_failed", "cannot list Gmail labels; check account access and retry", nil, err)
	}
	result := &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{}, Before: []string{}, AvailableTags: []emailtags.MessageTag{}}
	for _, label := range labels {
		if label.Type == "user" {
			result.AvailableTags = append(result.AvailableTags, emailtags.MessageTag{ID: label.ID, Name: label.Name})
		}
	}
	for _, id := range append(slices.Clone(change.Add), change.Remove...) {
		if !slices.ContainsFunc(result.AvailableTags, func(tag emailtags.MessageTag) bool { return tag.ID == id }) {
			return nil, emailtags.Failure("unavailable_tag", "use an existing user label ID from available_tags", nil, nil)
		}
	}
	tags, err := c.readTagIDs(ctx, id)
	if err != nil {
		return nil, emailtags.Failure("provider_read_failed", "cannot read Gmail message tags; sync or check account access", nil, err)
	}
	result.Tags = slices.Clone(tags)
	result.Before = slices.Clone(tags)
	if input == nil {
		result.Verified = true
		return result, nil
	}
	result.DryRun = change.DryRun
	if change.DryRun {
		result.Tags = emailtags.Project(tags, change, false)
		return result, nil
	}
	add, remove := emailtags.Delta(tags, change, false)
	if len(add)+len(remove) > 0 {
		body, marshalErr := json.Marshal(struct {
			Add    []string `json:"addLabelIds,omitempty"`
			Remove []string `json:"removeLabelIds,omitempty"`
		}{add, remove})
		if marshalErr != nil {
			return result, marshalErr
		}
		response, err := c.request(ctx, OpMessagesModify, http.MethodPost, fmt.Sprintf("/users/%s/messages/%s/modify", url.PathEscape(c.userID), url.PathEscape(id)), body)
		if err != nil {
			code, message := "provider_write_failed", "Gmail rejected tag changes; check permissions and read current tags before retrying"
			if errors.Is(err, errWriteOutcomeUnknown) {
				code, message = "remote_unknown", "Gmail write outcome is unknown; read current tags before retrying"
			}
			return result, emailtags.Failure(code, message, result, err)
		}
		if observed, err := decodeTagIDs(response, id); err == nil {
			result.Tags = observed
		}
		after, readErr := c.readTagIDs(ctx, id)
		if readErr != nil {
			return result, emailtags.Failure("remote_unknown", "Gmail accepted the write but readback failed; read current tags before retrying", result, readErr)
		}
		result.Tags = after
	}
	if !emailtags.Verify(result.Tags, change, false) {
		return result, emailtags.Failure("verification_failed", "Gmail readback does not match requested changes; inspect tags and retry deliberately", result, nil)
	}
	result.Verified = true
	return result, nil
}
