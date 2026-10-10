// Package omi archives Omi conversations through the read-only Developer API.
package omi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/httpretry"
)

var errCooldownSaveBudget = errors.New("omi cooldown save runtime budget exceeded")

const SourceType = "omi"
const RawFormat = "omi_json"
const DefaultBaseURL = "https://api.omi.me"
const PageSize = 200 // Upstream clamps the list endpoint to 200 records.

// TranscriptListsPerHour is Omi's per-key budget for transcript list requests.
const TranscriptListsPerHour = 25

// RequestInterval spaces requests evenly with no burst, so one client never
// exceeds the hourly budget.
var RequestInterval = time.Hour / TranscriptListsPerHour

// NormalizeBaseURL accepts a backend root, including a reverse-proxy prefix.
func NormalizeBaseURL(value string) (string, error) {
	if value == "" {
		value = DefaultBaseURL
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("omi base_url must be an HTTP(S) backend root without credentials, query, or fragment")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", errors.New("omi base_url must use HTTPS for non-loopback hosts")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1/dev") {
		return "", errors.New("omi base_url must be the backend root; omit /v1/dev")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// maxCooldown bounds a saved provider cooldown, so a wrong Retry-After from a
// proxy cannot block a source indefinitely.
const maxCooldown = 24 * time.Hour

// retryDelay makes Omi's 429 the authority: its transcript budget is a fixed
// hourly window, so honor Retry-After up to maxCooldown and otherwise wait out
// the whole hour.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp.StatusCode == http.StatusTooManyRequests {
		return httpretry.RetryAfterAtWithBase(resp.Header.Get("Retry-After"), 0, time.Hour, maxCooldown, time.Now())
	}
	return httpretry.RetryAfter(resp.Header.Get("Retry-After"), attempt, httpretry.ProviderMaxRetryAfter)
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	paceDir string
}

// NewPacedClient shares the provider budget across processes using the same backend and key.
func NewPacedClient(baseURL, apiKey, paceDir string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, paceDir: paceDir,
		http: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// CooldownError reports when the provider permits the next request.
type CooldownError struct{ Until time.Time }

func (e *CooldownError) Error() string {
	return "Omi provider cooldown until " + e.Until.UTC().Format(time.RFC3339)
}

func (c *Client) pacePath(baseURL string) string {
	sum := sha256.Sum256([]byte(baseURL + "\n" + c.apiKey))
	return filepath.Join(c.paceDir, "pace-"+hex.EncodeToString(sum[:8]))
}

func (c *Client) waitTurn(ctx context.Context, baseURL string) error {
	path := c.pacePath(baseURL)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait, err := claimPaceSlot(ctx, path, time.Now)
		if err != nil || wait <= 0 {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// claimPaceSlot records now as the last request time when RequestInterval has
// passed since the recorded one, and otherwise returns how long to wait. Only a
// request about to be sent claims the slot, so canceled waits cost nothing.
func claimPaceSlot(ctx context.Context, path string, clock func() time.Time) (wait time.Duration, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, fmt.Errorf("create Omi pacing directory: %w", err)
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !locked {
		return 0, fmt.Errorf("lock Omi pacing file: %w", errors.Join(err, ctx.Err()))
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	// Read the clock under the lock so claims land in timestamp order.
	now := clock()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read Omi pacing file: %w", err)
	}
	if cooldown, ok := strings.CutPrefix(string(data), "cooldown:"); ok {
		until, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(cooldown))
		if parseErr == nil && until.After(now) {
			if deadline, ok := ctx.Deadline(); ok && until.After(deadline) {
				return 0, &CooldownError{Until: until}
			}
			return until.Sub(now), nil
		}
	}
	// A last request in the future means the clock moved backward; claim now.
	if last, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); parseErr == nil && !last.After(now) {
		if wait := last.Add(RequestInterval).Sub(now); wait > 0 {
			return wait, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		return 0, fmt.Errorf("write Omi pacing file: %w", err)
	}
	return 0, nil
}

func (c *Client) saveCooldown(ctx context.Context, baseURL string, delay time.Duration) (until time.Time, err error) {
	until = time.Now().Add(delay)
	path := c.pacePath(baseURL)
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !locked {
		return until, fmt.Errorf("lock Omi cooldown: %w", errors.Join(err, ctx.Err()))
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return until, err
	}
	if prior, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(strings.TrimPrefix(string(data), "cooldown:"))); err == nil {
		if !strings.HasPrefix(string(data), "cooldown:") {
			prior = prior.Add(RequestInterval)
		}
		if prior.After(until) {
			until = prior
		}
	}
	return until, os.WriteFile(path, []byte("cooldown:"+until.UTC().Format(time.RFC3339Nano)), 0o600) // #nosec G703 -- Filename is a SHA-256 digest under the trusted configured pacing directory.
}

type ListParams struct {
	Limit         int
	Offset        int
	CreatedBefore time.Time
	CreatedAfter  time.Time
}

type Conversation struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	Structured struct {
		Title string `json:"title"`
	} `json:"structured"`
	Raw jsontext.Value `json:"-"`
}

// ListConversations preserves each full provider object, including unknown fields.
func (c *Client) ListConversations(ctx context.Context, p ListParams) ([]Conversation, error) {
	baseURL := c.baseURL
	if c.paceDir == "" {
		return nil, errors.New("omi pacing directory is required")
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("omi api_key is required (Developer API key with conversations:read)")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = PageSize
	}
	q := url.Values{"limit": {strconv.Itoa(min(limit, PageSize))}, "offset": {strconv.Itoa(p.Offset)}, "include_transcript": {"true"}}
	if !p.CreatedAfter.IsZero() {
		q.Set("start_date", p.CreatedAfter.UTC().Format(time.RFC3339Nano))
	}
	if !p.CreatedBefore.IsZero() {
		q.Set("end_date", p.CreatedBefore.UTC().Format(time.RFC3339Nano))
	}
	endpoint := baseURL + "/v1/dev/user/conversations?" + q.Encode()
	// A server failure stays attached to later waits, so an outage that
	// outlasts the pass fails the sync instead of looking like a pause.
	var serverErr error
	for attempt := range 8 {
		if err := c.waitTurn(ctx, baseURL); err != nil {
			return nil, errors.Join(serverErr, fmt.Errorf("wait for Omi API rate limit: %w", err))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, errors.Join(serverErr, fmt.Errorf("omi list conversations: %w", err))
		}
		// Record the status before reading the body, which can stall until
		// the pass deadline.
		serverErr = nil
		if resp.StatusCode >= 500 {
			serverErr = statusError(resp.StatusCode)
		}
		var cooldownUntil time.Time
		var cooldownErr error
		if resp.StatusCode == http.StatusTooManyRequests {
			// The headers establish the cooldown even if reading the body fails.
			saveCtx, cancel := context.WithTimeoutCause(context.WithoutCancel(ctx), 5*time.Second, errCooldownSaveBudget)
			cooldownUntil, cooldownErr = c.saveCooldown(saveCtx, baseURL, retryDelay(resp, attempt))
			if cooldownErr != nil {
				cooldownErr = fmt.Errorf("persist Omi cooldown: %w", errors.Join(cooldownErr, context.Cause(saveCtx)))
			}
			cancel()
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			readErr = fmt.Errorf("omi read response: %w", readErr)
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("omi close response: %w", closeErr)
		}
		responseErr := errors.Join(cooldownErr, readErr, closeErr)
		if len(body) > 64<<20 {
			responseErr = errors.Join(responseErr, errors.New("omi response exceeds 64 MiB"))
		}
		if responseErr != nil {
			return nil, errors.Join(serverErr, responseErr)
		}
		if deadline, ok := ctx.Deadline(); ok && cooldownUntil.After(deadline) {
			return nil, &CooldownError{Until: cooldownUntil}
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			var items []jsontext.Value
			if err := json.Unmarshal(body, &items); err != nil || items == nil {
				return nil, errors.New("omi list conversations: expected a JSON array")
			}
			result := make([]Conversation, 0, len(items))
			for _, raw := range items {
				var item Conversation
				if err := json.Unmarshal(raw, &item); err != nil {
					return nil, fmt.Errorf("omi decode conversation: %w", err)
				}
				if strings.TrimSpace(item.ID) == "" {
					return nil, errors.New("omi conversation has no ID")
				}
				item.Raw = raw
				result = append(result, item)
			}
			return result, nil
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, errors.New("omi rejected api_key (401): use a Developer API key (omi_dev_...), not an MCP key")
		case resp.StatusCode == http.StatusForbidden:
			return nil, errors.New("omi denied conversation access (403): check conversations:read scope and account access")
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			delay := retryDelay(resp, attempt)
			if attempt == 7 {
				break
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, errors.Join(serverErr, ctx.Err())
			case <-timer.C:
			}
		default:
			return nil, statusError(resp.StatusCode)
		}
	}
	return nil, errors.Join(errors.New("omi list conversations: exhausted retries"), serverErr)
}

// statusError omits the provider error body, which can contain private content.
func statusError(status int) error {
	return fmt.Errorf("omi list conversations: HTTP %d", status)
}
