package beeper

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const eventControlTimeout = 15 * time.Second

var errEventDiscovery = errors.New("beeper events discovery unavailable")
var errEventEndpoint = errors.New("beeper events endpoint must use the configured origin")

// resolveEventURL trusts discovery for a path, never a new credential destination.
func resolveEventURL(baseURL, discovered string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.User != nil || base.Fragment != "" || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errEventEndpoint
	}
	if strings.TrimSpace(discovered) == "" {
		return nil, errEventEndpoint
	}
	endpoint, err := url.Parse(discovered)
	if err != nil || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errEventEndpoint
	}
	scheme := "ws"
	if base.Scheme == "https" {
		scheme = "wss"
	}
	if endpoint.IsAbs() && endpoint.Scheme != scheme {
		return nil, errEventEndpoint
	}
	wsBase := *base
	wsBase.Scheme = scheme
	endpoint = wsBase.ResolveReference(endpoint)
	effectivePort := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" || u.Scheme == "wss" {
			return "443"
		}
		return "80"
	}
	if endpoint.Scheme != scheme || !strings.EqualFold(endpoint.Hostname(), base.Hostname()) || effectivePort(endpoint) != effectivePort(base) {
		return nil, errEventEndpoint
	}
	return endpoint, nil
}

func (c *Client) eventHTTPClient() *http.Client {
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (c *Client) discoverEvents(ctx context.Context, token string) (*url.URL, error) {
	// Validate the configured origin before sending any credentials.
	if _, err := resolveEventURL(c.baseURL, "/v1/ws"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, eventControlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/info", nil)
	if err != nil {
		return nil, errEventDiscovery
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.eventHTTPClient().Do(req)
	if err != nil {
		return nil, errEventDiscovery
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errEventDiscovery
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return nil, errEventDiscovery
	}
	var info struct {
		Endpoints struct {
			Events string `json:"ws_events"`
		} `json:"endpoints"`
	}
	if json.Unmarshal(body, &info) != nil {
		return nil, errEventDiscovery
	}
	return resolveEventURL(c.baseURL, info.Endpoints.Events)
}

const eventMessageLimit = 256 << 10

var errEventProtocol = errors.New("beeper events protocol unavailable")
var errEventConnection = errors.New("beeper events connection unavailable")
var errEventToken = errors.New("beeper events token unavailable")

type eventEnvelope struct {
	Type      string   `json:"type"`
	Version   int      `json:"version"`
	RequestID string   `json:"requestID"`
	ChatIDs   []string `json:"chatIDs"`
	Seq       *uint64  `json:"seq"`
	ChatID    string   `json:"chatID"`
	IDs       []string `json:"ids"`
}

func (e eventEnvelope) isHint() bool {
	switch e.Type {
	case "chat.upserted", "chat.deleted", "message.upserted", "message.deleted":
		return true
	}
	return false
}
func decodeEventEnvelope(b []byte) (eventEnvelope, error) {
	var e eventEnvelope
	if json.Unmarshal(b, &e) != nil || e.Type == "" {
		return e, errEventProtocol
	}
	if e.isHint() {
		if e.Seq == nil || e.ChatID == "" || len(e.IDs) == 0 {
			return e, errEventProtocol
		}
		if slices.Contains(e.IDs, "") {
			return e, errEventProtocol
		}
	}
	return e, nil
}

type eventWatchOptions struct {
	controlTimeout, readTimeout      time.Duration
	minRetry, maxRetry, healthyAfter time.Duration
	wait                             func(context.Context, time.Duration) error
}

func defaultEventWatchOptions() eventWatchOptions {
	return eventWatchOptions{controlTimeout: eventControlTimeout, readTimeout: 5 * time.Minute, minRetry: time.Second, maxRetry: time.Minute, healthyAfter: time.Minute, wait: waitEventRetry}
}
func waitEventRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func nextEventRetry(previous, healthy time.Duration, o eventWatchOptions) time.Duration {
	if healthy >= o.healthyAfter {
		return o.minRetry
	}
	return min(previous*2, o.maxRetry)
}

// jitterEventRetry keeps retry waits in the lower-inclusive, upper-exclusive
// half-delay window. Entropy failures retain its bounded minimum wait.
func jitterEventRetry(delay time.Duration, entropy io.Reader) time.Duration {
	wait := delay / 2
	offset, err := rand.Int(entropy, big.NewInt(max(int64(delay-wait), 1)))
	if err == nil {
		wait += time.Duration(offset.Int64())
	}
	return wait
}

// WatchEvents treats live events only as hints to run the HTTP importer. wake
// and report run synchronously and must return promptly. A nil report means
// the wildcard subscription was acknowledged, not that the archive is current.
// Discovery, bearer reload, and reconnect continue until ctx is canceled.
func (c *Client) WatchEvents(ctx context.Context, wake func(), report func(error)) error {
	return c.watchEvents(ctx, wake, report, defaultEventWatchOptions())
}
func (c *Client) watchEvents(ctx context.Context, wake func(), report func(error), o eventWatchOptions) error {
	delay := o.minRetry
	for ctx.Err() == nil {
		healthy, err := c.eventAttempt(ctx, wake, report, o)
		if ctx.Err() != nil {
			break
		}
		if report != nil {
			report(err)
		}
		if healthy >= o.healthyAfter {
			delay = o.minRetry
		}
		// Equal jitter bounds retries between half and all of the current delay.
		wait := jitterEventRetry(delay, rand.Reader)
		if err := o.wait(ctx, wait); err != nil {
			return err
		}
		delay = nextEventRetry(delay, healthy, o)
	}
	return ctx.Err()
}
func (c *Client) eventAttempt(ctx context.Context, wake func(), report func(error), o eventWatchOptions) (time.Duration, error) {
	token, err := c.token(ctx)
	if err != nil {
		return 0, errEventToken
	}
	endpoint, err := c.discoverEvents(ctx, token)
	if err != nil {
		return 0, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, o.controlTimeout)
	conn, resp, err := websocket.Dial(dialCtx, endpoint.String(), &websocket.DialOptions{HTTPClient: c.eventHTTPClient(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}, CompressionMode: websocket.CompressionDisabled})
	cancel()
	// Dial transfers a successful upgrade's body to conn and returns a nil
	// Body. A nonnil error-response body is only the library's bounded copy.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return 0, ErrUnauthorized
		}
		return 0, errEventConnection
	}
	defer func() { _ = conn.CloseNow() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stop()
	conn.SetReadLimit(eventMessageLimit)
	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, o.controlTimeout)
	defer handshakeCancel()
	read := func(readCtx context.Context) (eventEnvelope, error) {
		kind, b, err := conn.Read(readCtx)
		if err != nil {
			return eventEnvelope{}, errEventConnection
		}
		if kind != websocket.MessageText {
			return eventEnvelope{}, errEventProtocol
		}
		return decodeEventEnvelope(b)
	}
	ready, err := read(handshakeCtx)
	if err != nil {
		return 0, err
	}
	if ready.Type != "ready" || ready.Version != 1 {
		return 0, errEventProtocol
	}
	if conn.Write(handshakeCtx, websocket.MessageText, []byte(`{"type":"subscriptions.set","requestID":"msgvault","chatIDs":["*"]}`)) != nil {
		return 0, errEventConnection
	}
	for {
		ack, err := read(handshakeCtx)
		if err != nil {
			return 0, err
		}
		if ack.Type == "subscriptions.updated" {
			if ack.RequestID != "msgvault" || len(ack.ChatIDs) != 1 || ack.ChatIDs[0] != "*" {
				return 0, errEventProtocol
			}
			break
		}
		if ack.Type == "error" || ack.Type == "ready" {
			return 0, errEventProtocol
		}
		// Domain events observed before acknowledgement carry no admission authority.
	}
	handshakeCancel()
	subscribed := time.Now()
	if report != nil {
		report(nil)
	}
	if wake != nil {
		wake()
	}
	for {
		readCtx, cancel := context.WithTimeout(ctx, o.readTimeout)
		e, err := read(readCtx)
		cancel()
		if err == nil && (e.Type == "ready" || e.Type == "subscriptions.updated" || e.Type == "error") {
			err = errEventProtocol
		}
		if err != nil {
			if ctx.Err() == nil && wake != nil {
				wake()
			} // established connection lost: recover through HTTP
			return time.Since(subscribed), err
		}
		// Sequence gaps, repeats and regressions all receive the same recovery hint.
		// No sequence is a durable replay cursor, and entries never reach the Store.
		if e.isHint() && wake != nil {
			wake()
		}
	}
}
