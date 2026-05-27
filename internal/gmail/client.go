package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

const (
	baseURL        = "https://gmail.googleapis.com/gmail/v1"
	maxRetries     = 12  // Covers ~10 minutes of network outages
	maxBackoff     = 600 // Max backoff in seconds
	defaultTimeout = 30 * time.Second
)

// Client implements the Gmail API interface.
type Client struct {
	httpClient  *http.Client
	rateLimiter *RateLimiter
	logger      *slog.Logger
	userID      string // "me" for authenticated user
	concurrency int    // Max parallel requests for batch operations
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithLogger sets the logger for the client.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(c *Client) {
		c.logger = logger
	}
}

// WithConcurrency sets the max concurrent requests for batch operations.
func WithConcurrency(n int) ClientOption {
	return func(c *Client) {
		c.concurrency = n
	}
}

// WithRateLimiter sets a custom rate limiter.
func WithRateLimiter(rl *RateLimiter) ClientOption {
	return func(c *Client) {
		c.rateLimiter = rl
	}
}

// NewClient creates a new Gmail API client.
func NewClient(tokenSource oauth2.TokenSource, opts ...ClientOption) *Client {
	c := &Client{
		httpClient:  oauth2.NewClient(context.Background(), tokenSource),
		userID:      "me",
		concurrency: 10,
		logger:      slog.Default(),
	}

	// Apply options
	for _, opt := range opts {
		opt(c)
	}

	// Default rate limiter if not set
	if c.rateLimiter == nil {
		c.rateLimiter = NewRateLimiter(5.0)
	}

	return c
}

// Close releases resources held by the client.
func (c *Client) Close() error {
	// HTTP client doesn't need explicit closing
	return nil
}

// request makes an HTTP request with rate limiting and retry logic.
// bodyBytes can be nil for requests without a body.
func (c *Client) request(ctx context.Context, op Operation, method, path string, bodyBytes []byte) ([]byte, error) {
	// Acquire rate limit tokens
	if err := c.rateLimiter.Acquire(ctx, op); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	reqURL := baseURL + path

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.calculateBackoff(attempt)
			c.logger.Debug("retrying request", "attempt", attempt, "backoff", backoff, "path", path)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		// Create a new reader for each attempt to ensure body can be re-read on retry
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue // Retry on network errors
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response: %w", err)
			continue
		}

		// Check for success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		// Handle specific error codes
		switch resp.StatusCode {
		case http.StatusTooManyRequests: // Rate limited
			// Log at Debug level since rate limiting is expected during high-volume syncs
			// and the retry logic handles it automatically
			c.logger.Debug("rate limited, backing off 30s", "path", path, "attempt", attempt)
			// Throttle the rate limiter to back off
			c.rateLimiter.Throttle(30 * time.Second)
			lastErr = errors.New("rate limited (429)")
			continue

		case http.StatusForbidden: // Could be rate limit or permission error
			// Gmail returns 403 for quota exceeded with "rateLimitExceeded" reason
			if isRateLimitError(respBody) {
				// Log at Debug level since quota throttling is expected during high-volume syncs
				// and the retry logic handles it automatically
				c.logger.Debug("quota exceeded, backing off 60s", "path", path, "attempt", attempt)
				// Throttle the rate limiter - quota errors need longer backoff
				c.rateLimiter.Throttle(60 * time.Second)
				lastErr = errors.New("quota exceeded (403)")
				continue // Retry with backoff
			}
			// Actual permission error - don't retry
			return nil, fmt.Errorf("forbidden (403): %s", string(respBody))

		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // Server errors
			lastErr = fmt.Errorf("server error (%d)", resp.StatusCode)
			continue

		case http.StatusUnauthorized: // Unauthorized - token might be expired
			// oauth2.Client should auto-refresh, but if it fails, don't retry
			return nil, errors.New("unauthorized (401): token may be invalid")

		case http.StatusNotFound: // Not found
			return nil, &NotFoundError{Path: path}

		default: // Other client errors - don't retry
			return nil, fmt.Errorf("request failed (%d): %s", resp.StatusCode, string(respBody))
		}
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// calculateBackoff returns the backoff duration for a retry attempt.
// Uses exponential backoff with full jitter.
func (c *Client) calculateBackoff(attempt int) time.Duration {
	// Exponential: 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 600, 600...
	base := float64(uint(1) << uint(attempt))
	if base > maxBackoff {
		base = maxBackoff
	}

	// Full jitter: random value between 0 and base. math/rand is fine here —
	// the jitter is only for retry-backoff spread, not authentication.
	jittered := rand.Float64() * base //nolint:gosec // not security-sensitive
	return time.Duration(jittered * float64(time.Second))
}

// NotFoundError indicates a 404 response.
type NotFoundError struct {
	Path string
}

func (e *NotFoundError) Error() string {
	return "not found: " + e.Path
}

// Gmail API JSON response types (unexported, used only for JSON unmarshaling).

type profileResponse struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	ThreadsTotal  int64  `json:"threadsTotal"`
	HistoryID     string `json:"historyId"`
}

type gmailLabel struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	MessagesTotal         int64  `json:"messagesTotal"`
	MessagesUnread        int64  `json:"messagesUnread"`
	MessageListVisibility string `json:"messageListVisibility"`
	LabelListVisibility   string `json:"labelListVisibility"`
}

type listLabelsResponse struct {
	Labels []gmailLabel `json:"labels"`
}

type gmailMessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type listMessagesResponse struct {
	Messages           []gmailMessageRef `json:"messages"`
	NextPageToken      string            `json:"nextPageToken"`
	ResultSizeEstimate int64             `json:"resultSizeEstimate"`
}

type rawMessageResponse struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	HistoryID    string   `json:"historyId"`
	InternalDate string   `json:"internalDate"`
	SizeEstimate int64    `json:"sizeEstimate"`
	Raw          string   `json:"raw"` // base64url encoded (unpadded)
}

// decodeBase64URL decodes a base64url-encoded string, tolerating optional padding.
// Gmail typically returns unpadded base64url, but this function handles both cases.
// If padding is present, it validates that padding is correct (rejects malformed padding).
func decodeBase64URL(s string) ([]byte, error) {
	if strings.ContainsRune(s, '=') {
		// Input has padding - use URLEncoding which validates padding correctness
		return base64.URLEncoding.DecodeString(s)
	}
	// No padding - use RawURLEncoding for unpadded base64url
	return base64.RawURLEncoding.DecodeString(s)
}

type historyMessageChange struct {
	Message gmailMessageRef `json:"message"`
}

type historyLabelChangeJSON struct {
	Message  gmailMessageRef `json:"message"`
	LabelIDs []string        `json:"labelIds"`
}

type historyEntry struct {
	ID              string                   `json:"id"`
	MessagesAdded   []historyMessageChange   `json:"messagesAdded"`
	MessagesDeleted []historyMessageChange   `json:"messagesDeleted"`
	LabelsAdded     []historyLabelChangeJSON `json:"labelsAdded"`
	LabelsRemoved   []historyLabelChangeJSON `json:"labelsRemoved"`
}

type listHistoryResponse struct {
	History       []historyEntry `json:"history"`
	NextPageToken string         `json:"nextPageToken"`
	HistoryID     string         `json:"historyId"`
}

// GetProfile returns the authenticated user's profile.
func (c *Client) GetProfile(ctx context.Context) (*Profile, error) {
	path := fmt.Sprintf("/users/%s/profile", c.userID)
	data, err := c.request(ctx, OpProfile, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp profileResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}

	historyID, _ := strconv.ParseUint(resp.HistoryID, 10, 64)

	return &Profile{
		EmailAddress:  resp.EmailAddress,
		MessagesTotal: resp.MessagesTotal,
		ThreadsTotal:  resp.ThreadsTotal,
		HistoryID:     historyID,
	}, nil
}

// ListLabels returns all labels for the account.
func (c *Client) ListLabels(ctx context.Context) ([]*Label, error) {
	path := fmt.Sprintf("/users/%s/labels", c.userID)
	data, err := c.request(ctx, OpLabelsList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listLabelsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse labels: %w", err)
	}

	labels := make([]*Label, len(resp.Labels))
	for i, l := range resp.Labels {
		labels[i] = &Label{
			ID:                    l.ID,
			Name:                  l.Name,
			Type:                  l.Type,
			MessagesTotal:         l.MessagesTotal,
			MessagesUnread:        l.MessagesUnread,
			MessageListVisibility: l.MessageListVisibility,
			LabelListVisibility:   l.LabelListVisibility,
		}
	}
	return labels, nil
}

// ListMessages returns message IDs matching the query.
func (c *Client) ListMessages(ctx context.Context, query string, pageToken string) (*MessageListResponse, error) {
	params := url.Values{}
	params.Set("maxResults", "500")
	if query != "" {
		params.Set("q", query)
	}
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}

	path := fmt.Sprintf("/users/%s/messages?%s", c.userID, params.Encode())
	data, err := c.request(ctx, OpMessagesList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listMessagesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}

	messages := make([]MessageID, len(resp.Messages))
	for i, m := range resp.Messages {
		messages[i] = MessageID(m)
	}

	return &MessageListResponse{
		Messages:           messages,
		NextPageToken:      resp.NextPageToken,
		ResultSizeEstimate: resp.ResultSizeEstimate,
	}, nil
}

// GetMessageRaw fetches a single message with raw MIME data.
func (c *Client) GetMessageRaw(ctx context.Context, messageID string) (*RawMessage, error) {
	path := fmt.Sprintf("/users/%s/messages/%s?format=raw", c.userID, messageID)
	data, err := c.request(ctx, OpMessagesGetRaw, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp rawMessageResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	// Decode raw MIME from base64url
	rawBytes, err := decodeBase64URL(resp.Raw)
	if err != nil {
		return nil, fmt.Errorf("decode raw MIME: %w", err)
	}

	historyID, _ := strconv.ParseUint(resp.HistoryID, 10, 64)
	internalDate, _ := strconv.ParseInt(resp.InternalDate, 10, 64)

	return &RawMessage{
		ID:           resp.ID,
		ThreadID:     resp.ThreadID,
		LabelIDs:     resp.LabelIDs,
		Snippet:      resp.Snippet,
		HistoryID:    historyID,
		InternalDate: internalDate,
		SizeEstimate: resp.SizeEstimate,
		Raw:          rawBytes,
	}, nil
}

// isRateLimitError checks if a 403 response is actually a rate limit error.
// Gmail returns 403 with "rateLimitExceeded" for quota exceeded instead of 429.
func isRateLimitError(body []byte) bool {
	// Check for common rate limit indicators in the response
	return bytes.Contains(body, []byte("rateLimitExceeded")) ||
		bytes.Contains(body, []byte("RATE_LIMIT_EXCEEDED")) ||
		bytes.Contains(body, []byte("Quota exceeded")) ||
		// Also check for userRateLimitExceeded which is another variant
		bytes.Contains(body, []byte("userRateLimitExceeded"))
}

// GetMessagesRawBatch fetches multiple messages in parallel with rate limiting.
func (c *Client) GetMessagesRawBatch(ctx context.Context, messageIDs []string) ([]*RawMessage, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	results := make([]*RawMessage, len(messageIDs))
	sem := make(chan struct{}, c.concurrency)

	g, ctx := errgroup.WithContext(ctx)

	for i, id := range messageIDs {
		g.Go(func() error {
			// Acquire semaphore
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return ctx.Err()
			}

			msg, err := c.GetMessageRaw(ctx, id)
			if err != nil {
				// Log but don't fail the batch - allow partial results.
				// 404s are expected (message deleted between history scan and fetch),
				// so log at debug level to avoid noise during incremental sync.
				var nfe *NotFoundError
				if errors.As(err, &nfe) {
					c.logger.Debug("message deleted before fetch", "id", id)
				} else {
					c.logger.Warn("failed to fetch message", "id", id, "error", err)
				}
				return nil
			}

			results[i] = msg
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return results, nil
}

// ListHistory returns changes since the given history ID.
func (c *Client) ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*HistoryResponse, error) {
	params := url.Values{}
	params.Set("startHistoryId", strconv.FormatUint(startHistoryID, 10))
	params.Set("maxResults", "500")
	for _, ht := range []string{"messageAdded", "messageDeleted", "labelAdded", "labelRemoved"} {
		params.Add("historyTypes", ht)
	}
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}

	path := fmt.Sprintf("/users/%s/history?%s", c.userID, params.Encode())
	data, err := c.request(ctx, OpHistoryList, "GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp listHistoryResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse history: %w", err)
	}

	historyID, _ := strconv.ParseUint(resp.HistoryID, 10, 64)

	return &HistoryResponse{
		History:       mapHistoryEntries(resp.History),
		NextPageToken: resp.NextPageToken,
		HistoryID:     historyID,
	}, nil
}

// mapHistoryEntries converts JSON history entries to domain types.
func mapHistoryEntries(entries []historyEntry) []HistoryRecord {
	records := make([]HistoryRecord, len(entries))
	for i, h := range entries {
		id, _ := strconv.ParseUint(h.ID, 10, 64)
		records[i] = HistoryRecord{
			ID:              id,
			MessagesAdded:   mapMessageChanges(h.MessagesAdded),
			MessagesDeleted: mapMessageChanges(h.MessagesDeleted),
			LabelsAdded:     mapLabelChanges(h.LabelsAdded),
			LabelsRemoved:   mapLabelChanges(h.LabelsRemoved),
		}
	}
	return records
}

func mapMessageChanges(changes []historyMessageChange) []HistoryMessage {
	out := make([]HistoryMessage, len(changes))
	for i, c := range changes {
		out[i] = HistoryMessage{
			Message: MessageID(c.Message),
		}
	}
	return out
}

func mapLabelChanges(changes []historyLabelChangeJSON) []HistoryLabelChange {
	out := make([]HistoryLabelChange, len(changes))
	for i, c := range changes {
		out[i] = HistoryLabelChange{
			Message:  MessageID(c.Message),
			LabelIDs: c.LabelIDs,
		}
	}
	return out
}

// TrashMessage moves a message to trash.
func (c *Client) TrashMessage(ctx context.Context, messageID string) error {
	path := fmt.Sprintf("/users/%s/messages/%s/trash", c.userID, messageID)
	_, err := c.request(ctx, OpMessagesTrash, "POST", path, nil)
	return err
}

// DeleteMessage permanently deletes a message.
func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	path := fmt.Sprintf("/users/%s/messages/%s", c.userID, messageID)
	_, err := c.request(ctx, OpMessagesDelete, "DELETE", path, nil)
	return err
}

// BatchDeleteMessages permanently deletes multiple messages.
func (c *Client) BatchDeleteMessages(ctx context.Context, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	if len(messageIDs) > 1000 {
		return fmt.Errorf("batch delete limited to 1000 messages, got %d", len(messageIDs))
	}

	body := struct {
		IDs []string `json:"ids"`
	}{IDs: messageIDs}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	path := fmt.Sprintf("/users/%s/messages/batchDelete", c.userID)
	_, err = c.request(ctx, OpMessagesBatchDelete, "POST", path, bodyBytes)
	return err
}

// Ensure Client implements API interface.
var _ API = (*Client)(nil)
