// Package remote provides an HTTP client for accessing a remote msgvault server.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Store provides remote API access to a msgvault server.
type Store struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Config holds configuration for creating a remote store.
type Config struct {
	URL           string
	APIKey        string
	AllowInsecure bool
	Timeout       time.Duration
}

// New creates a new remote store.
func New(cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("remote URL is required")
	}

	parsedURL, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	// Enforce HTTPS unless AllowInsecure is set
	if parsedURL.Scheme == "http" && !cfg.AllowInsecure {
		return nil, fmt.Errorf("HTTPS required for remote connections\n\n" +
			"Options:\n" +
			"  1. Use HTTPS: [remote] url = \"https://nas:8080\"\n" +
			"  2. For trusted networks: add 'allow_insecure = true' to [remote] in config.toml")
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme must be http or https, got: %s", parsedURL.Scheme)
	}

	if parsedURL.Host == "" {
		return nil, fmt.Errorf("remote URL must include a host (e.g., http://nas:8080)")
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Store{
		baseURL: strings.TrimSuffix(cfg.URL, "/"),
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// Close is a no-op for HTTP client.
func (s *Store) Close() error {
	return nil
}

// doRequest performs an authenticated HTTP request.
func (s *Store) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	return s.doRequestWithContext(context.Background(), method, path, body)
}

// doRequestWithContext performs an authenticated HTTP request with context support.
func (s *Store) doRequestWithContext(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	reqURL := s.baseURL + path

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if s.apiKey != "" {
		req.Header.Set("X-API-Key", s.apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// apiError represents an error response from the API.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// handleErrorResponse reads an error response and returns an appropriate error.
func handleErrorResponse(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("API error (%d): could not read response body: %w", resp.StatusCode, err)
	}

	var apiErr apiError
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Message != "" {
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr.Message)
	}

	return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
}

// statsResponse matches the API stats response format.
type statsResponse struct {
	TotalMessages int64 `json:"total_messages"`
	TotalThreads  int64 `json:"total_threads"`
	TotalAccounts int64 `json:"total_accounts"`
	TotalLabels   int64 `json:"total_labels"`
	TotalAttach   int64 `json:"total_attachments"`
	DatabaseSize  int64 `json:"database_size_bytes"`
}

// GetStats fetches stats from the remote server.
func (s *Store) GetStats() (*store.Stats, error) {
	resp, err := s.doRequest("GET", "/api/v1/stats", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, handleErrorResponse(resp)
	}

	var sr statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("decode stats response: %w", err)
	}

	return &store.Stats{
		MessageCount:    sr.TotalMessages,
		ThreadCount:     sr.TotalThreads,
		SourceCount:     sr.TotalAccounts,
		LabelCount:      sr.TotalLabels,
		AttachmentCount: sr.TotalAttach,
		DatabaseSize:    sr.DatabaseSize,
	}, nil
}

// messageResponse matches the API message summary format.
type messageResponse struct {
	ID             int64    `json:"id"`
	ConversationID int64    `json:"conversation_id,omitempty"`
	Subject        string   `json:"subject"`
	From           string   `json:"from"`
	To             []string `json:"to"`
	SentAt         string   `json:"sent_at"`
	DeletedAt      string   `json:"deleted_at,omitempty"`
	Snippet        string   `json:"snippet"`
	Labels         []string `json:"labels"`
	HasAttach      bool     `json:"has_attachments"`
	SizeBytes      int64    `json:"size_bytes"`
}

// messageDetailResponse includes body and attachments.
type messageDetailResponse struct {
	messageResponse
	Body        string               `json:"body"`
	Attachments []attachmentResponse `json:"attachments"`
}

// attachmentResponse matches the API attachment format.
type attachmentResponse struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size_bytes"`
}

// listMessagesResponse matches the API list messages response.
type listMessagesResponse struct {
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Messages []messageResponse `json:"messages"`
}

// parseTime parses RFC3339 time string.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// toAPIMessage converts a messageResponse to store.APIMessage.
func toAPIMessage(m messageResponse) store.APIMessage {
	var deletedAt *time.Time
	if m.DeletedAt != "" {
		t := parseTime(m.DeletedAt)
		if !t.IsZero() {
			deletedAt = &t
		}
	}
	return store.APIMessage{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Subject:        m.Subject,
		From:           m.From,
		To:             m.To,
		SentAt:         parseTime(m.SentAt),
		DeletedAt:      deletedAt,
		Snippet:        m.Snippet,
		Labels:         m.Labels,
		HasAttachments: m.HasAttach,
		SizeEstimate:   m.SizeBytes,
	}
}

// ListMessages fetches a paginated list of messages.
// Callers (API layer) always provide page-aligned offsets.
func (s *Store) ListMessages(offset, limit int) ([]store.APIMessage, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	path := fmt.Sprintf("/api/v1/messages?page=%d&page_size=%d", page, limit)
	resp, err := s.doRequest("GET", path, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, handleErrorResponse(resp)
	}

	var lr listMessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, 0, fmt.Errorf("decode messages response: %w", err)
	}

	messages := make([]store.APIMessage, len(lr.Messages))
	for i, m := range lr.Messages {
		messages[i] = toAPIMessage(m)
	}

	return messages, lr.Total, nil
}

// GetMessage fetches a single message by ID.
func (s *Store) GetMessage(id int64) (*store.APIMessage, error) {
	path := "/api/v1/messages/" + strconv.FormatInt(id, 10)
	resp, err := s.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, handleErrorResponse(resp)
	}

	var mr messageDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode message response: %w", err)
	}

	msg := toAPIMessage(mr.messageResponse)
	msg.Body = mr.Body

	attachments := make([]store.APIAttachment, len(mr.Attachments))
	for i, a := range mr.Attachments {
		attachments[i] = store.APIAttachment{
			Filename: a.Filename,
			MimeType: a.MimeType,
			Size:     a.Size,
		}
	}
	msg.Attachments = attachments

	return &msg, nil
}

// searchResponse matches the API search response format.
type searchResponse struct {
	Query    string            `json:"query"`
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Messages []messageResponse `json:"messages"`
}

// SearchMessages searches messages on the remote server.
// SearchMessages searches messages via the remote API.
// Callers (API layer) always provide page-aligned offsets.
func (s *Store) SearchMessages(query string, offset, limit int) ([]store.APIMessage, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	path := fmt.Sprintf("/api/v1/search?q=%s&page=%d&page_size=%d",
		url.QueryEscape(query), page, limit)

	resp, err := s.doRequest("GET", path, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, handleErrorResponse(resp)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, 0, fmt.Errorf("decode search response: %w", err)
	}

	messages := make([]store.APIMessage, len(sr.Messages))
	for i, m := range sr.Messages {
		messages[i] = toAPIMessage(m)
	}

	return messages, sr.Total, nil
}

// AccountInfo represents an account in list responses.
type AccountInfo struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	LastSyncAt  string `json:"last_sync_at,omitempty"`
	NextSyncAt  string `json:"next_sync_at,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// accountsResponse matches the API accounts list response.
type accountsResponse struct {
	Accounts []AccountInfo `json:"accounts"`
}

// ListAccounts fetches configured accounts from the remote server.
func (s *Store) ListAccounts() ([]AccountInfo, error) {
	resp, err := s.doRequest("GET", "/api/v1/accounts", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, handleErrorResponse(resp)
	}

	var ar accountsResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, fmt.Errorf("decode accounts response: %w", err)
	}

	return ar.Accounts, nil
}
