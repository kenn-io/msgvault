package beeper

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
)

// Label is a provider observation; native label mutation is unsupported.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ChatCapabilities preserves unknown capability values separately from false.
type ChatCapabilities struct {
	Archive *bool `json:"archive"`
}

// ForTriage bounds explicit live reads without changing importer/draft defaults.
func (c *Client) ForTriage() *Client {
	scoped := *c
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	scoped.http = &client
	scoped.readAttempts = 1
	scoped.readMaxBytes = 1 << 20
	return &scoped
}

// ListLabels reads the installation-wide native catalog, if supported.
func (c *Client) ListLabels(ctx context.Context) ([]Label, error) {
	var labels []Label
	if err := c.ForTriage().getJSON(ctx, "/v1/labels", &labels); err != nil {
		return nil, err
	}
	if labels == nil {
		return nil, errors.New("native label catalog did not return a list")
	}
	return labels, nil
}

// ArchiveWriteError has status zero when the provider outcome is unknown.
// It is separate from DraftWriteError because archive recovery holds a
// conversation; draft recovery retains the existing pending-draft lifecycle.
// A response body never becomes a user-visible error or a retry instruction.
type ArchiveWriteError struct {
	Status int
	Err    error
}

func (e *ArchiveWriteError) Error() string {
	return fmt.Sprintf("beeper archive response status %d", e.Status)
}
func (e *ArchiveWriteError) Unwrap() error { return e.Err }

// Rejected reports a definite provider refusal, excluding an ambiguous timeout.
func (e *ArchiveWriteError) Rejected() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != 408
}

// ArchiveChat sends one request. In particular 307/308 and transient failures
// cannot replay the write. Existing draft behavior is intentionally separate.
func (c *Client) ArchiveChat(ctx context.Context, chatID string, archived bool) error {
	body, err := json.Marshal(map[string]bool{"archived": archived})
	if err != nil {
		return err
	}
	if err = c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("wait for archive rate limit: %w", err)
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chats/"+url.PathEscape(chatID)+"/archive", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.GetBody = nil // also prohibit net/http replay of a write on a broken connection
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return &ArchiveWriteError{Err: err}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &ArchiveWriteError{Status: response.StatusCode}
	}
	return nil
}

// requestBudget counts attempts at the private HTTP transport boundary. A
// shared scoped client cannot renew this allowance by capturing another scope.
type requestBudget struct {
	base  http.RoundTripper
	limit int64
	used  atomic.Int64
}

func (b *requestBudget) RoundTrip(req *http.Request) (*http.Response, error) {
	for {
		used := b.used.Load()
		if used >= b.limit {
			return nil, errors.New("beeper request limit")
		}
		if b.used.CompareAndSwap(used, used+1) {
			break
		}
	}
	return b.base.RoundTrip(req)
}

// ForBoundedOperations isolates triage reads and explicit writes from importer
// transport policy. Close the returned scope after the entire operation.
func (c *Client) ForBoundedOperations(maxRequests int) (*Client, func(), error) {
	if maxRequests < 1 || maxRequests > 128 {
		return nil, nil, errors.New("invalid beeper request limit")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, nil, errors.New("unsupported default context transport")
	}
	if existing, ok := c.http.Transport.(*http.Transport); ok {
		base = existing
	}
	transport := base.Clone()
	transport.DisableKeepAlives = true
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	budget := &requestBudget{base: transport, limit: int64(maxRequests)}
	scoped := *c
	client := *c.http
	client.Transport = budget
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	scoped.http, scoped.requestBudget = &client, budget
	scoped.readAttempts, scoped.readMaxBytes = 1, 1<<20
	return &scoped, transport.CloseIdleConnections, nil
}

// Requests reports physical attempts in this scope, including writes/readback.
func (c *Client) Requests() int {
	if c.requestBudget == nil {
		return 0
	}
	return int(c.requestBudget.used.Load())
}
