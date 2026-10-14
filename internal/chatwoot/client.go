package chatwoot

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/netguard"
	"go.kenn.io/msgvault/internal/remoteimage"
	"golang.org/x/time/rate"
)

const maxAPIBytes = 32 << 20

const (
	maxMediaRedirects = 10
)

var ErrNotFound = errors.New("chatwoot object not found")
var ErrResponseTooLarge = errors.New("chatwoot API response exceeds 32 MiB; a single message must fit within this limit")

// Client has separate authenticated API and credential-free media transports.
type Client struct {
	baseURL              string
	baseOrigin           *url.URL
	accountID            int64
	token                string
	api                  *http.Client
	limiter              *rate.Limiter
	lookupMediaIP        func(context.Context, string) ([]netip.Addr, error)
	dialMedia            func(context.Context, string, string) (net.Conn, error)
	mediaTransferTimeout func(int64) time.Duration
	// messageRangeCap is the most messages Chatwoot returns for one bounded
	// range. MessageFinder#messages_between has used limit(1000), ordered by
	// created_at, since the combined range was added (chatwoot/chatwoot#6848).
	// Servers without the combined range fail the importer's bounds probe.
	messageRangeCap int
}

func CanonicalURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Hostname() == "" ||
		(u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("chatwoot URL must be an absolute HTTP(S) instance URL without credentials, query or fragment")
	}
	targetPort, err := netguard.TargetPort(u)
	if err != nil {
		return "", errors.New("chatwoot URL port must be between 1 and 65535")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" && !netguard.IsLoopbackHost(host) {
		return "", errors.New("chatwoot remote URL must use HTTPS")
	}
	port := strconv.Itoa(int(targetPort))
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// SourceIdentifier is stable across display-name and local profile-label changes.
// Callers validate baseURL first; invalid inputs yield an empty identifier.
func SourceIdentifier(baseURL string, accountID, inboxID int64) string {
	canonical, err := CanonicalURL(baseURL)
	if err != nil || accountID <= 0 || inboxID <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/accounts/%d/inboxes/%d", canonical, accountID, inboxID)
}

// ParseSourceIdentifier accepts only canonical registered inbox identifiers.
func ParseSourceIdentifier(identifier string) (baseURL string, accountID, inboxID int64, ok bool) {
	accountScope, inbox, found := strings.CutLast(identifier, "/inboxes/")
	if !found {
		return
	}
	inboxID, err := strconv.ParseInt(inbox, 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	baseURL, account, found := strings.CutLast(accountScope, "/accounts/")
	if !found {
		return "", 0, 0, false
	}
	accountID, err = strconv.ParseInt(account, 10, 64)
	if err != nil || identifier != SourceIdentifier(baseURL, accountID, inboxID) {
		return "", 0, 0, false
	}
	return baseURL, accountID, inboxID, true
}

func NewClient(baseURL string, accountID int64, token string) (*Client, error) {
	canonical, err := CanonicalURL(baseURL)
	if err != nil {
		return nil, err
	}
	if accountID <= 0 {
		return nil, errors.New("chatwoot account_id must be positive")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("chatwoot API token is missing or invalid")
	}
	parsedBase, err := url.Parse(canonical)
	if err != nil {
		return nil, errors.New("invalid Chatwoot instance URL")
	}
	baseOrigin := *parsedBase
	baseOrigin.Path = ""
	baseOrigin.RawPath = ""
	baseOrigin.RawQuery = ""
	baseOrigin.ForceQuery = false
	baseOrigin.Fragment = ""
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &Client{
		baseURL: canonical, baseOrigin: &baseOrigin, accountID: accountID, token: token,
		api: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		limiter: rate.NewLimiter(5, 1),
		lookupMediaIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dialMedia: dialer.DialContext, mediaTransferTimeout: attachmentpolicy.DownloadTimeout, messageRangeCap: 1000,
	}, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := fmt.Sprintf("%s/api/v1/accounts/%d%s", c.baseURL, c.accountID, path)
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	resp, err := httpretry.Do(ctx, 3, time.Minute, func() (*http.Response, error) {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for Chatwoot API: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, errors.New("construct Chatwoot API request")
		}
		req.Header.Set("Api_access_token", c.token)
		req.Header.Set("Accept", "application/json")
		return c.api.Do(req)
	}, func(resp *http.Response) bool {
		return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	})
	if err != nil {
		return safeHTTPError(ctx, "Chatwoot API request", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return safeHTTPError(ctx, "read Chatwoot API response", readErr)
	}
	if closeErr != nil {
		return safeHTTPError(ctx, "close Chatwoot API response", closeErr)
	}
	if len(body) > maxAPIBytes {
		return ErrResponseTooLarge
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chatwoot API HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("invalid Chatwoot API JSON response")
	}
	return nil
}

func (c *Client) ListInboxes(ctx context.Context) ([]Inbox, error) {
	var result struct {
		Payload *[]Inbox `json:"payload"`
	}
	if err := c.get(ctx, "/inboxes", nil, &result); err != nil {
		return nil, err
	}
	if result.Payload == nil {
		return nil, errors.New("chatwoot inbox response has no payload")
	}
	return *result.Payload, nil
}

func (c *Client) ListAgents(ctx context.Context) ([]Actor, error) {
	var result *[]Actor
	if err := c.get(ctx, "/agents", nil, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("chatwoot agent response has no array")
	}
	for i := range *result {
		(*result)[i].Type = actorUser
	}
	return *result, nil
}

const (
	sortByCreated  = "created_at_asc"
	sortByActivity = "last_activity_at_desc"
)

func (c *Client) ListConversations(ctx context.Context, page int, inboxID int64, sortBy string) ([]Conversation, error) {
	if page <= 0 || inboxID <= 0 || (sortBy != sortByCreated && sortBy != sortByActivity) {
		return nil, errors.New("chatwoot conversation page and inbox must be positive")
	}
	var result struct {
		Data struct {
			Payload *[]Conversation `json:"payload"`
		} `json:"data"`
	}
	query := url.Values{"status": {"all"}, "assignee_type": {"all"}, "sort_by": {sortBy}, "page": {strconv.Itoa(page)}}
	query.Set("inbox_id", strconv.FormatInt(inboxID, 10))
	if err := c.get(ctx, "/conversations", query, &result); err != nil {
		return nil, err
	}
	if result.Data.Payload == nil {
		return nil, errors.New("chatwoot conversation response has no payload")
	}
	return *result.Data.Payload, nil
}

// GetConversation retrieves current, safely projected context for saved work.
func (c *Client) GetConversation(ctx context.Context, conversationID int64) (Conversation, error) {
	if conversationID <= 0 {
		return Conversation{}, errors.New("invalid Chatwoot conversation ID")
	}
	var result Conversation
	if err := c.get(ctx, fmt.Sprintf("/conversations/%d", conversationID), nil, &result); err != nil {
		return result, err
	}
	if result.ID != conversationID {
		return result, errors.New("chatwoot conversation response ID mismatch")
	}
	return result, nil
}

// ListMessages uses provider ID bounds. With both bounds, after is inclusive;
// alone it is exclusive. Oversized before is unbounded on pinned Chatwoot.
// A fully handled response below the verified cap completes its range; capped responses retain unresolved ranges.
func (c *Client) ListMessages(ctx context.Context, conversationID, after, before int64) ([]Message, error) {
	if conversationID <= 0 || after < 0 || before < 0 {
		return nil, errors.New("invalid Chatwoot conversation or message bounds")
	}
	var result struct {
		Payload *[]Message `json:"payload"`
	}
	query := url.Values{}
	if after > 0 {
		query.Set("after", strconv.FormatInt(after, 10))
	}
	if before > 0 {
		query.Set("before", strconv.FormatInt(before, 10))
	}
	if err := c.get(ctx, fmt.Sprintf("/conversations/%d/messages", conversationID), query, &result); err != nil {
		return nil, err
	}
	if result.Payload == nil {
		return nil, errors.New("chatwoot message response has no payload")
	}
	return *result.Payload, nil
}

func (c *Client) mediaFetcher() *remoteimage.Fetcher {
	return &remoteimage.Fetcher{
		LookupNetIP: c.lookupMediaIP, DialContext: c.dialMedia,
		PrivateOrigin: c.baseOrigin, TryAllAddresses: true, MaxRedirects: maxMediaRedirects,
	}
}

type cancelReadCloser struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (body *cancelReadCloser) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

func (c *Client) OpenMedia(ctx context.Context, rawURL string, maxBytes int64) (io.ReadCloser, int64, string, error) {
	timeout := attachmentpolicy.DownloadTimeout
	if c.mediaTransferTimeout != nil {
		timeout = c.mediaTransferTimeout
	}
	mediaCtx, cancel := context.WithTimeout(ctx, timeout(maxBytes))
	returnedBody := false
	defer func() {
		if !returnedBody {
			cancel()
		}
	}()
	resp, fetchErr := c.mediaFetcher().Open(mediaCtx, rawURL)
	if fetchErr != nil {
		return nil, 0, "", safeHTTPError(mediaCtx, "Chatwoot media request", fetchErr)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, "", fmt.Errorf("chatwoot media HTTP %d", resp.StatusCode)
	}
	returnedBody = true
	return &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}, resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

// URL-bearing transport errors and provider bodies must not leak signed URLs,
// credentials or personal content into sync status. Preserve cancellation only.
func safeHTTPError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", operation, ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s failed", operation)
}
