package daemonclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/msgvault/internal/store"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// EmailFamilyContext resolves archive families through the owning daemon.
func (c *Client) EmailFamilyContext(ctx context.Context, id int64, options store.EmailFamilyOptions) (*store.EmailFamily, error) {
	path := fmt.Sprintf("/api/v1/messages/%d/family", id)
	if options.SourceIDs != nil {
		values := url.Values{}
		if len(options.SourceIDs) == 0 {
			values.Add("source_id", "")
		}
		for _, source := range options.SourceIDs {
			values.Add("source_id", strconv.FormatInt(source, 10))
		}
		path += "?" + values.Encode()
	}
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodGet, path, nil, c.httpClient, 1<<20)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil, store.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get email family (HTTP %d)", response.StatusCode)
	}
	var family store.EmailFamily
	// Families are bounded by member and key counts, but message fields have no
	// response-byte cap. Decode the successful response directly so valid
	// families are not truncated at an unrelated client-side limit.
	if err := json.UnmarshalRead(response.Body, &family); err != nil {
		return nil, fmt.Errorf("decode email family: %w", err)
	}
	return &family, nil
}

type emailFamilyRequestOptions struct{ body jsontext.Value }

func (o emailFamilyRequestOptions) GetPathParams() (map[string]any, error) {
	return map[string]any{}, nil
}
func (o emailFamilyRequestOptions) GetQuery() (map[string]any, error) { return map[string]any{}, nil }
func (o emailFamilyRequestOptions) GetBody() any                      { return o.body }
func (o emailFamilyRequestOptions) GetHeader() (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json"}, nil
}

func (c *Client) emailFamilyJSON(ctx context.Context, method, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, method, path, emailFamilyRequestOptions{body: body}, c.httpClient, maxErrorBodyBytes)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		if err := json.UnmarshalRead(response.Body, output); err != nil {
			return fmt.Errorf("decode email family decision: %w", err)
		}
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
	if err != nil {
		return err
	}
	_, code, _ := apiErrorMessage(data)
	switch code {
	case "not_found":
		return store.ErrNotFound
	case "family_changed":
		return store.ErrEmailFamilyChanged
	case "family_truncated":
		return store.ErrEmailFamilyTruncated
	case "request_conflict":
		return store.ErrEmailFamilyRequestConflict
	}
	return handleErrorBody(response.StatusCode, data)
}

func (c *Client) PreviewEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionPreview, error) {
	var result store.EmailFamilyDecisionPreview
	err := c.emailFamilyJSON(ctx, http.MethodPost, "/api/v1/messages/family/decisions/preview", input, &result)
	return &result, err
}
func (c *Client) ApplyEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionResult, error) {
	var result store.EmailFamilyDecisionResult
	err := c.emailFamilyJSON(ctx, http.MethodPost, "/api/v1/messages/family/decisions", input, &result)
	return &result, err
}
func (c *Client) UndoEmailFamilyDecisionContext(ctx context.Context, id, revision int64, familyRevision string, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error) {
	input := struct {
		ExpectedRevision       int64   `json:"expected_revision"`
		ExpectedFamilyRevision string  `json:"expected_family_revision"`
		SourceIDs              []int64 `json:"source_ids,omitzero"`
	}{revision, familyRevision, options.SourceIDs}
	var result store.EmailFamilyDecisionResult
	err := c.emailFamilyJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/messages/family/decisions/%d/undo", id), input, &result)
	return &result, err
}
func (c *Client) GetEmailFamilyDecisionContext(ctx context.Context, id int64, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error) {
	path := fmt.Sprintf("/api/v1/messages/family/decisions/%d", id)
	if options.SourceIDs != nil {
		values := url.Values{}
		if len(options.SourceIDs) == 0 {
			values.Add("source_id", "")
		}
		for _, source := range options.SourceIDs {
			values.Add("source_id", strconv.FormatInt(source, 10))
		}
		path += "?" + values.Encode()
	}
	var result store.EmailFamilyDecisionResult
	err := c.emailFamilyJSON(ctx, http.MethodGet, path, nil, &result)
	return &result, err
}
func (c *Client) ListEmailFamilyDecisionsContext(ctx context.Context, id int64, options store.EmailFamilyOptions) ([]store.EmailFamilyDecision, error) {
	var result []store.EmailFamilyDecision
	path := fmt.Sprintf("/api/v1/messages/family/decisions?message_id=%d", id)
	if options.SourceIDs != nil {
		values := url.Values{}
		if len(options.SourceIDs) == 0 {
			values.Add("source_id", "")
		}
		for _, id := range options.SourceIDs {
			values.Add("source_id", strconv.FormatInt(id, 10))
		}
		path += "&" + values.Encode()
	}
	err := c.emailFamilyJSON(ctx, http.MethodGet, path, nil, &result)
	return result, err
}
