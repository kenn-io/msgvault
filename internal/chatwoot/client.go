package chatwoot

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/netguard"
	"golang.org/x/time/rate"
)

const maxAPIBytes = 32 << 20

const (
	maxMediaRedirects         = 10
	maxMediaURLBytes          = 4096
	mediaBytesPerSecond int64 = 128 << 10
	minimumMediaTimeout       = 10 * time.Minute
)

var ErrNotFound = errors.New("chatwoot object not found")

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
	// range (MessageFinder#messages_between).
	messageRangeCap int
}

// mediaTimeout scales one attachment's deadline with its configured size cap.
// The minimum avoids starving small files; the transfer-rate floor gives large
// recordings room on slow links while keeping scheduled syncs bounded.
func mediaTimeout(maxBytes int64) time.Duration {
	return mediaTimeoutForRate(maxBytes, mediaBytesPerSecond, minimumMediaTimeout)
}

func mediaTimeoutForRate(maxBytes, bytesPerSecond int64, minimum time.Duration) time.Duration {
	if maxBytes <= 0 || bytesPerSecond <= 0 {
		return minimum
	}
	seconds := maxBytes / bytesPerSecond
	maxSeconds := int64(math.MaxInt64) / int64(time.Second)
	if seconds > maxSeconds {
		return time.Duration(math.MaxInt64)
	}
	scaled := time.Duration(seconds) * time.Second
	if scaled < minimum {
		return minimum
	}
	return scaled
}

func CanonicalURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Hostname() == "" ||
		(u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("chatwoot URL must be an absolute HTTP(S) instance URL without credentials, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" && !isLoopbackHost(host) {
		return "", errors.New("chatwoot remote URL must use HTTPS")
	}
	port := u.Port()
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

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		dialMedia: dialer.DialContext, mediaTransferTimeout: mediaTimeout, messageRangeCap: 1000,
	}, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := fmt.Sprintf("%s/api/v1/accounts/%d%s", c.baseURL, c.accountID, path)
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	for attempt := range 3 {
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("wait for Chatwoot API: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return errors.New("construct Chatwoot API request")
		}
		req.Header.Set("Api_access_token", c.token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.api.Do(req)
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
			return errors.New("chatwoot API response exceeds 32 MiB")
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(body, out); err != nil {
				// Parser errors may include source text; do not echo provider content.
				return errors.New("invalid Chatwoot API JSON response")
			}
			return nil
		}
		if resp.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		if attempt < 2 && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError) {
			delay := httpretry.RetryAfter(resp.Header.Get("Retry-After"), attempt, time.Minute)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return fmt.Errorf("chatwoot API HTTP %d", resp.StatusCode)
	}
	return errors.New("chatwoot API retries exhausted")
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
// Response size never proves a range complete; use empty-range observations.
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

func (c *Client) configuredMediaOrigin(u *url.URL) bool {
	if u == nil || c.baseOrigin == nil {
		return false
	}
	return strings.EqualFold(c.baseOrigin.Scheme, u.Scheme) &&
		strings.EqualFold(strings.TrimSuffix(c.baseOrigin.Hostname(), "."), strings.TrimSuffix(u.Hostname(), ".")) &&
		mediaEffectivePort(c.baseOrigin) == mediaEffectivePort(u)
}

func mediaEffectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func mediaTargetPort(u *url.URL) (uint16, error) {
	rawPort := u.Port()
	if rawPort == "" {
		if strings.EqualFold(u.Scheme, "https") {
			return 443, nil
		}
		return 80, nil
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("invalid Chatwoot media URL port")
	}
	return uint16(port), nil
}

func mediaAddressAllowed(addr netip.Addr, configuredOrigin, configuredLoopback bool) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if configuredLoopback {
		return addr.IsLoopback()
	}
	if !netguard.ProhibitedIP(addr) {
		return true
	}
	return configuredOrigin && netguard.ExplicitPrivateAddress(addr)
}

// validateMediaTarget resolves and pins one media URL hop. Public media hosts
// follow netguard policy; private destinations are allowed only for the exact
// configured Chatwoot origin, and only on loopback or explicitly private ranges.
func (c *Client) validateMediaTarget(ctx context.Context, u *url.URL) ([]netip.AddrPort, error) {
	if u == nil || len(u.String()) > maxMediaURLBytes || u.User != nil || u.Hostname() == "" {
		return nil, errors.New("invalid Chatwoot media URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("invalid Chatwoot media URL")
	}
	u.Scheme = scheme
	port, err := mediaTargetPort(u)
	if err != nil {
		return nil, errors.New("invalid Chatwoot media URL")
	}
	host := u.Hostname()
	configuredOrigin := c.configuredMediaOrigin(u)
	configuredLoopback := configuredOrigin && isLoopbackHost(c.baseOrigin.Hostname())
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		literal = literal.Unmap()
		if !mediaAddressAllowed(literal, configuredOrigin, configuredLoopback) {
			return nil, errors.New("chatwoot media destination is prohibited")
		}
		return []netip.AddrPort{netip.AddrPortFrom(literal, port)}, nil
	}
	if netguard.ProhibitedHostname(host) && !configuredOrigin {
		return nil, errors.New("chatwoot media destination is prohibited")
	}
	addresses, err := c.lookupMediaIP(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("chatwoot media host could not be resolved")
	}
	pinned := make([]netip.AddrPort, 0, len(addresses))
	seen := make(map[netip.AddrPort]bool, len(addresses))
	for _, address := range addresses {
		if !mediaAddressAllowed(address, configuredOrigin, configuredLoopback) {
			return nil, errors.New("chatwoot media destination is prohibited")
		}
		candidate := netip.AddrPortFrom(address.Unmap(), port)
		if !seen[candidate] {
			seen[candidate] = true
			pinned = append(pinned, candidate)
		}
	}
	return pinned, nil
}

func (c *Client) roundTripMedia(ctx context.Context, u *url.URL, pinned []netip.AddrPort) (*http.Response, error) {
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
			var failures []error
			for _, address := range pinned {
				conn, err := c.dialMedia(dialCtx, network, address.String())
				if err == nil {
					return conn, nil
				}
				failures = append(failures, err)
			}
			return nil, errors.Join(failures...)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("construct Chatwoot media request")
	}
	return transport.RoundTrip(req)
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
	timeout := mediaTimeout
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
	current := rawURL
	for redirects := 0; ; redirects++ {
		u, err := url.Parse(current)
		if err != nil {
			return nil, 0, "", errors.New("invalid Chatwoot media URL")
		}
		pinned, err := c.validateMediaTarget(mediaCtx, u)
		if err != nil {
			return nil, 0, "", err
		}
		resp, err := c.roundTripMedia(mediaCtx, u, pinned)
		if err != nil {
			return nil, 0, "", safeHTTPError(mediaCtx, "Chatwoot media request", err)
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if redirects >= maxMediaRedirects {
				return nil, 0, "", errors.New("too many Chatwoot media redirects")
			}
			if location == "" {
				return nil, 0, "", errors.New("invalid Chatwoot media redirect")
			}
			next, parseErr := u.Parse(location)
			if parseErr != nil {
				return nil, 0, "", errors.New("invalid Chatwoot media redirect")
			}
			current = next.String()
			continue
		case http.StatusOK:
			returnedBody = true
			return &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}, resp.ContentLength, resp.Header.Get("Content-Type"), nil
		default:
			_ = resp.Body.Close()
			return nil, 0, "", fmt.Errorf("chatwoot media HTTP %d", resp.StatusCode)
		}
	}
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
