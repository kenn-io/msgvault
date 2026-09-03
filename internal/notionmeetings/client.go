package notionmeetings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

const (
	maxRetries      = 8
	maxResponseSize = int64(16 << 20)
	maxRetryAfter   = httpretry.ProviderMaxRetryAfter
	maxQueryResults = 50
	pageSize        = 100
)

var (
	ErrUnauthorized      = errors.New("notion integration token is invalid or expired")
	ErrMeetingAccess     = errors.New("notion AI Meeting Notes access is unavailable")
	ErrReadContent       = errors.New("notion integration lacks Read Content access")
	ErrUserInformation   = errors.New("notion integration lacks User Information access")
	ErrRateLimited       = errors.New("notion rate limit retry budget exhausted")
	ErrMalformedResponse = errors.New("notion returned a malformed response")
	ErrProvider          = errors.New("notion API request failed")
)

type APIError struct {
	Kind   error  `json:"-"`
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
}

func (e *APIError) Error() string {
	if e == nil {
		return ErrProvider.Error()
	}
	return fmt.Sprintf("%s (status %d, code %s)", e.Kind, e.Status, e.Code)
}

func (e *APIError) Unwrap() error {
	if e == nil || e.Kind == nil {
		return ErrProvider
	}
	return e.Kind
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	wait    func(context.Context, time.Duration) error
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
		wait:    waitContext,
	}
}

func (c *Client) QueryMeetingNotes(ctx context.Context, limit int) (*QueryResult, error) {
	if limit <= 0 || limit > maxQueryResults {
		limit = maxQueryResults
	}
	payload := struct {
		Sort []struct {
			Property  string `json:"property"`
			Direction string `json:"direction"`
		} `json:"sort"`
		Limit int `json:"limit"`
	}{Limit: limit}
	payload.Sort = append(payload.Sort, struct {
		Property  string `json:"property"`
		Direction string `json:"direction"`
	}{Property: "created_time", Direction: "descending"})
	var result QueryResult
	raw, err := c.doJSON(ctx, http.MethodPost, "/v1/blocks/meeting_notes/query", payload, &result, operationQuery)
	if err != nil {
		return nil, err
	}
	if err := validateListEnvelope(raw); err != nil {
		return nil, err
	}
	for index := range result.Results {
		if err := validateBlockResponse(result.Results[index].Raw, "", "meeting_notes"); err != nil {
			return nil, fmt.Errorf("validate meeting-note query result %d: %w", index, err)
		}
	}
	result.Raw = raw
	return &result, nil
}

func (c *Client) RetrieveBlock(ctx context.Context, blockID string) (*Block, error) {
	var result Block
	raw, err := c.doJSON(ctx, http.MethodGet, "/v1/blocks/"+url.PathEscape(blockID), nil, &result, operationContent)
	if err != nil {
		return nil, err
	}
	if err := validateBlockResponse(raw, blockID, ""); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) RetrieveBlockChildren(ctx context.Context, blockID, cursor string) (*BlockPage, error) {
	query := url.Values{"page_size": {strconv.Itoa(pageSize)}}
	if cursor != "" {
		query.Set("start_cursor", cursor)
	}
	path := "/v1/blocks/" + url.PathEscape(blockID) + "/children?" + query.Encode()
	var result BlockPage
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, &result, operationContent)
	if err != nil {
		return nil, err
	}
	if err := validateListEnvelope(raw); err != nil {
		return nil, err
	}
	for index := range result.Results {
		if err := validateBlockResponse(result.Results[index].Raw, "", ""); err != nil {
			return nil, fmt.Errorf("validate block child %d: %w", index, err)
		}
	}
	result.Raw = raw
	return &result, nil
}

func (c *Client) RetrievePageMarkdown(ctx context.Context, pageID string, includeTranscript bool) (*MarkdownPage, error) {
	query := url.Values{}
	if includeTranscript {
		query.Set("include_transcript", "true")
	}
	path := "/v1/pages/" + url.PathEscape(pageID) + "/markdown"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var result MarkdownPage
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil, &result, operationContent)
	if err != nil {
		return nil, err
	}
	if err := validateMarkdownResponse(raw, pageID); err != nil {
		return nil, err
	}
	result.Raw = raw
	return &result, nil
}

func (c *Client) ListUsers(ctx context.Context, cursor string) (*UserPage, error) {
	query := url.Values{"page_size": {strconv.Itoa(pageSize)}}
	if cursor != "" {
		query.Set("start_cursor", cursor)
	}
	var result UserPage
	raw, err := c.doJSON(ctx, http.MethodGet, "/v1/users?"+query.Encode(), nil, &result, operationUsers)
	if err != nil {
		return nil, err
	}
	if err := validateListEnvelope(raw); err != nil {
		return nil, err
	}
	result.Raw = raw
	return &result, nil
}

func validateListEnvelope(raw json.RawMessage) error {
	var envelope struct {
		Results *json.RawMessage `json:"results"`
		HasMore *bool            `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: decode list response: %w", ErrMalformedResponse, err)
	}
	if envelope.Results == nil || envelope.HasMore == nil {
		return fmt.Errorf("%w: list response is missing required fields", ErrMalformedResponse)
	}
	return nil
}

func validateBlockResponse(raw json.RawMessage, expectedID, expectedType string) error {
	var envelope struct {
		Object      *string `json:"object"`
		ID          *string `json:"id"`
		Type        *string `json:"type"`
		HasChildren *bool   `json:"has_children"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: decode block response: %w", ErrMalformedResponse, err)
	}
	if envelope.Object == nil || *envelope.Object != "block" || envelope.ID == nil ||
		strings.TrimSpace(*envelope.ID) == "" || envelope.Type == nil || strings.TrimSpace(*envelope.Type) == "" ||
		envelope.HasChildren == nil {
		return fmt.Errorf("%w: block response is missing required fields", ErrMalformedResponse)
	}
	if expectedID != "" && *envelope.ID != expectedID {
		return fmt.Errorf("%w: block response ID does not match the request", ErrMalformedResponse)
	}
	if expectedType != "" && *envelope.Type != expectedType {
		return fmt.Errorf("%w: block response has unexpected type", ErrMalformedResponse)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("%w: decode block fields: %w", ErrMalformedResponse, err)
	}
	payload, ok := fields[*envelope.Type]
	if !ok {
		return fmt.Errorf("%w: block response is missing its type payload", ErrMalformedResponse)
	}
	var typePayload map[string]json.RawMessage
	if err := json.Unmarshal(payload, &typePayload); err != nil || typePayload == nil {
		return fmt.Errorf("%w: block response has an invalid type payload", ErrMalformedResponse)
	}
	return nil
}

func validateMarkdownResponse(raw json.RawMessage, expectedID string) error {
	var envelope struct {
		Object          *string   `json:"object"`
		ID              *string   `json:"id"`
		Markdown        *string   `json:"markdown"`
		Truncated       *bool     `json:"truncated"`
		UnknownBlockIDs *[]string `json:"unknown_block_ids"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: decode page Markdown response: %w", ErrMalformedResponse, err)
	}
	if envelope.Object == nil || *envelope.Object != "page_markdown" || envelope.ID == nil ||
		strings.TrimSpace(*envelope.ID) == "" || envelope.Markdown == nil || envelope.Truncated == nil ||
		envelope.UnknownBlockIDs == nil {
		return fmt.Errorf("%w: page Markdown response is missing required fields", ErrMalformedResponse)
	}
	if *envelope.ID != expectedID {
		return fmt.Errorf("%w: page Markdown response ID does not match the request", ErrMalformedResponse)
	}
	return nil
}

type operation int

const (
	operationQuery operation = iota
	operationContent
	operationUsers
)

func (c *Client) doJSON(ctx context.Context, method, path string, payload any, target any, op operation) (json.RawMessage, error) {
	var encoded []byte
	var err error
	if payload != nil {
		encoded, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode Notion request: %w", err)
		}
	}
	for attempt := range maxRetries {
		var bodyReader io.Reader
		if encoded != nil {
			bodyReader = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("build Notion request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Notion-Version", APIVersion)
		req.Header.Set("Accept", "application/json")
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("perform Notion request: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read Notion response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close Notion response: %w", closeErr)
		}
		if int64(len(body)) > maxResponseSize {
			return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrMalformedResponse, maxResponseSize)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := json.Unmarshal(body, target); err != nil {
				return nil, fmt.Errorf("%w: decode response: %w", ErrMalformedResponse, err)
			}
			return append(json.RawMessage(nil), body...), nil
		}

		var providerErr struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &providerErr)
		if transientStatus(resp.StatusCode) {
			if attempt == maxRetries-1 {
				return nil, &APIError{Kind: ErrRateLimited, Status: resp.StatusCode, Code: providerErr.Code}
			}
			delay := httpretry.RetryAfter(resp.Header.Get("Retry-After"), attempt, maxRetryAfter)
			if err := c.wait(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		return nil, &APIError{Kind: classifyError(resp.StatusCode, op), Status: resp.StatusCode, Code: providerErr.Code}
	}
	return nil, ErrProvider
}

func transientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout || status == 529
}

func classifyError(status int, op operation) error {
	if status == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if status == http.StatusForbidden {
		switch op {
		case operationQuery:
			return ErrMeetingAccess
		case operationUsers:
			return ErrUserInformation
		default:
			return ErrReadContent
		}
	}
	return ErrProvider
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
