package notionmeetings

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type DiscoveryObject struct {
	Object string `json:"object"`
	ID     string `json:"id"`
}
type DiscoveryPage struct {
	Object        string            `json:"object"`
	Results       []DiscoveryObject `json:"results"`
	HasMore       bool              `json:"has_more"`
	NextCursor    string            `json:"next_cursor"`
	RequestStatus struct {
		Type             string `json:"type"`
		IncompleteReason string `json:"incomplete_reason"`
	} `json:"request_status"`
}
type Database struct {
	Object      string            `json:"object"`
	ID          string            `json:"id"`
	DataSources []DiscoveryObject `json:"data_sources"`
}

func (c *Client) RetrieveSelf(ctx context.Context) (*User, error) {
	var self User
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/users/me", nil, &self, operationContent)
	if err != nil {
		return nil, err
	}
	if self.Object != "user" || strings.TrimSpace(self.ID) == "" {
		return nil, ErrMalformedResponse
	}
	return &self, nil
}
func (c *Client) Search(ctx context.Context, cursor string) (*DiscoveryPage, error) {
	return c.discoveryPage(ctx, "/v1/search", cursor, discoveryPageSize(ctx))
}
func (c *Client) QueryDataSource(ctx context.Context, id, cursor string) (*DiscoveryPage, error) {
	return c.discoveryPage(ctx, "/v1/data_sources/"+url.PathEscape(id)+"/query", cursor, discoveryPageSize(ctx))
}
func (c *Client) discoveryPage(ctx context.Context, path, cursor string, size int) (*DiscoveryPage, error) {
	payload := map[string]any{"page_size": size}
	if cursor != "" {
		payload["start_cursor"] = cursor
	}
	var page DiscoveryPage
	raw, err := c.doJSON(ctx, http.MethodPost, path, payload, &page, operationContent)
	if err != nil {
		return nil, err
	}
	if err = validateListEnvelope(raw); err != nil {
		return nil, err
	}
	if page.Object != "list" || len(page.Results) > size || (page.HasMore && page.NextCursor == "") {
		return nil, ErrMalformedResponse
	}
	for _, o := range page.Results {
		if strings.TrimSpace(o.ID) == "" || (o.Object != "page" && o.Object != "data_source") {
			return nil, ErrMalformedResponse
		}
	}
	return &page, nil
}
func (c *Client) RetrieveDatabase(ctx context.Context, id string) (*Database, error) {
	var db Database
	raw, err := c.doJSON(ctx, http.MethodGet, "/v1/databases/"+url.PathEscape(id), nil, &db, operationContent)
	if err != nil {
		return nil, err
	}
	var required struct {
		DataSources *jsontext.Value `json:"data_sources"`
	}
	_ = json.Unmarshal(raw, &required)
	if db.Object != "database" || db.ID != id || required.DataSources == nil || len(db.DataSources) > pageSize {
		return nil, ErrMalformedResponse
	}
	for _, ds := range db.DataSources {
		if strings.TrimSpace(ds.ID) == "" {
			return nil, ErrMalformedResponse
		}
	}
	return &db, nil
}

type attemptBudgetKey struct{}
type attemptBudget struct {
	mu        sync.Mutex
	remaining int
}
type pageSizeKey struct{}

var ErrRequestBudget = errors.New("notion HTTP attempt budget exhausted")

// WithAttemptBudget shares one HTTP attempt allowance across clients and retries.
func WithAttemptBudget(ctx context.Context, attempts int) context.Context {
	return context.WithValue(ctx, attemptBudgetKey{}, &attemptBudget{remaining: attempts})
}
func consumeAttempt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(attemptBudgetKey{}).(*attemptBudget)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return ErrRequestBudget
	}
	b.remaining--
	return nil
}
func discoveryPageSize(ctx context.Context) int {
	if n, ok := ctx.Value(pageSizeKey{}).(int); ok && n > 0 && n < pageSize {
		return n
	}
	return pageSize
}
