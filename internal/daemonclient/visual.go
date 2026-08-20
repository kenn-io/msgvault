package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/vector/visual"
)

const visualSearchLimitField = "limit"

func (c *Client) SearchVisualAttachments(ctx context.Context, text string, image []byte, limit int) (*visual.SearchResponse, error) {
	return c.SearchVisualAttachmentsFiltered(ctx, VisualSearchOptions{Text: text, Image: image, Limit: limit})
}

type VisualSearchOptions struct {
	Text           string
	Image          []byte
	Limit          int
	Cursor         string
	SenderPersonID int64
	SourceID       int64
	MessageID      int64
	Filename       string
	MIMEPrefix     string
	After          *time.Time
	Before         *time.Time
}

func (c *Client) SearchVisualAttachmentsFiltered(ctx context.Context, options VisualSearchOptions) (*visual.SearchResponse, error) {
	var body bytes.Buffer
	contentType := "application/json"
	if len(options.Image) > 0 {
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("image", "query-image")
		if err != nil {
			return nil, fmt.Errorf("create visual image form part: %w", err)
		}
		if _, err := part.Write(options.Image); err != nil {
			return nil, err
		}
		if options.Limit > 0 {
			_ = writer.WriteField(visualSearchLimitField, strconv.Itoa(options.Limit))
		}
		if options.SenderPersonID > 0 {
			_ = writer.WriteField("sender_person_id", strconv.FormatInt(options.SenderPersonID, 10))
		}
		for key, value := range map[string]string{
			"cursor": options.Cursor, "filename": options.Filename, "mime_prefix": options.MIMEPrefix,
		} {
			if value != "" {
				_ = writer.WriteField(key, value)
			}
		}
		if options.SourceID > 0 {
			_ = writer.WriteField("source_id", strconv.FormatInt(options.SourceID, 10))
		}
		if options.MessageID > 0 {
			_ = writer.WriteField("message_id", strconv.FormatInt(options.MessageID, 10))
		}
		if options.After != nil {
			_ = writer.WriteField("after", options.After.Format("2006-01-02"))
		}
		if options.Before != nil {
			_ = writer.WriteField("before", options.Before.Format("2006-01-02"))
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("close visual search form: %w", err)
		}
		contentType = writer.FormDataContentType()
	} else {
		payload := map[string]any{
			"text": options.Text, visualSearchLimitField: options.Limit, "cursor": options.Cursor,
			"sender_person_id": options.SenderPersonID, "source_id": options.SourceID,
			"message_id": options.MessageID, "filename": options.Filename, "mime_prefix": options.MIMEPrefix,
		}
		if options.After != nil {
			payload["after"] = options.After.Format("2006-01-02")
		}
		if options.Before != nil {
			payload["before"] = options.Before.Format("2006-01-02")
		}
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/search/attachments/visual", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	resp, err := doRequestWithRootContext(c.requestContext(), c.httpClient, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("visual attachment search HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(message))
	}
	var result visual.SearchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode visual attachment search: %w", err)
	}
	return &result, nil
}

func (c *Client) VisualStatus(ctx context.Context) (*visual.Status, error) {
	return c.visualStatusRequest(ctx, http.MethodGet, "/api/v1/multimodal/status")
}

// VisualStatusWithCoverage additionally requests the per-format coverage
// scan, which re-reads every candidate blob; the daemon serializes it.
func (c *Client) VisualStatusWithCoverage(ctx context.Context) (*visual.Status, error) {
	return c.visualStatusRequest(ctx, http.MethodGet, "/api/v1/multimodal/status?coverage=1")
}

func (c *Client) RunVisualBuildPass(ctx context.Context) (*visual.Status, error) {
	return c.visualStatusRequest(ctx, http.MethodPost, "/api/v1/multimodal/run")
}

func (c *Client) ConsentVisualBuildPass(ctx context.Context) (*visual.Status, error) {
	return c.visualStatusJSONRequest(ctx, "/api/v1/multimodal/build", map[string]any{"consent": true})
}

func (c *Client) RetryVisualOwner(ctx context.Context, messageID int64, blobHash string) (*visual.Status, error) {
	return c.visualStatusJSONRequest(ctx, "/api/v1/multimodal/retry", map[string]any{
		"message_id": messageID, "blob_hash": blobHash,
	})
}

func (c *Client) RetireVisualGeneration(ctx context.Context, generationID int64) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(map[string]any{"generation_id": generationID}); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/multimodal/retire", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	resp, err := doRequestWithRootContext(c.requestContext(), c.httpClient, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("retire multimodal generation HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(message))
	}
	return nil
}

func (c *Client) visualStatusJSONRequest(ctx context.Context, path string, payload any) (*visual.Status, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	return c.decodeVisualStatusResponse(req)
}

func (c *Client) visualStatusRequest(ctx context.Context, method, path string) (*visual.Status, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	return c.decodeVisualStatusResponse(req)
}

func (c *Client) decodeVisualStatusResponse(req *http.Request) (*visual.Status, error) {
	resp, err := doRequestWithRootContext(c.requestContext(), c.httpClient, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("multimodal status HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(message))
	}
	var status visual.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}
