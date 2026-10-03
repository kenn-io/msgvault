package twenty

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const maxResponseBytes = 64 << 20
const pageSize = 100

const recordingFields = `id title status createdAt startedAt endedAt calendarEventId transcript summary { markdown }`
const calendarFields = `id title startsAt endsAt`
const participantFields = `id displayName handle isOrganizer`
const recordingQuery = `query Recordings($after: String, $first: Int!) {
 callRecordings(first: $first, after: $after, orderBy: [{id: AscNullsLast}]) {
 edges { node { ` + recordingFields + ` } } pageInfo { hasNextPage endCursor }
 } }`
const calendarQuery = `query Calendar($id: UUID!) {
 calendarEvents(filter: {id: {eq: $id}}, first: 1) { edges { node { ` + calendarFields + ` } } }
 }`
const participantQuery = `query Participants($id: UUID!, $after: String) {
 calendarEventParticipants(filter: {calendarEventId: {eq: $id}}, first: 100, after: $after, orderBy: [{id: AscNullsLast}]) {
 edges { node { ` + participantFields + ` } } pageInfo { hasNextPage endCursor }
 } }`
const probeQuery = `query Probe {
 callRecordings(first: 1, orderBy: [{id: AscNullsLast}]) { edges { node { ` + recordingFields + ` } } pageInfo { hasNextPage endCursor } }
 calendarEvents(first: 1) { edges { node { ` + calendarFields + ` } } }
 calendarEventParticipants(first: 1) { edges { node { ` + participantFields + ` } } pageInfo { hasNextPage endCursor } }
 }`

var errResponseTooLarge = errors.New("twenty response exceeds 64 MiB")
var errLinkedCalendarUnavailable = errors.New("linked Twenty calendar event is unavailable")

type rateLimitIdentity struct {
	origin     string
	credential [sha256.Size]byte
}

var sharedRateLimiters = struct {
	sync.Mutex

	byIdentity map[rateLimitIdentity]*rate.Limiter
}{byIdentity: make(map[rateLimitIdentity]*rate.Limiter)}

func rateLimiterFor(baseURL, apiKey string) *rate.Limiter {
	u, err := url.Parse(baseURL)
	if err != nil {
		return rate.NewLimiter(rate.Every(650*time.Millisecond), 1)
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	identity := rateLimitIdentity{origin: scheme + "://" + host, credential: sha256.Sum256([]byte(apiKey))}
	sharedRateLimiters.Lock()
	defer sharedRateLimiters.Unlock()
	if limiter := sharedRateLimiters.byIdentity[identity]; limiter != nil {
		return limiter
	}
	limiter := rate.NewLimiter(rate.Every(650*time.Millisecond), 1)
	sharedRateLimiters.byIdentity[identity] = limiter
	return limiter
}

type Client struct {
	endpoint string
	key      string
	http     *http.Client
	limiter  *rate.Limiter
}

// ValidateBaseURL accepts an API root, never an API path or a URL with
// credentials. Plain HTTP is limited to local development deployments.
func ValidateBaseURL(value string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u == nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", errors.New("twenty base_url must be an API root URL without credentials, query, fragment or path prefix")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		local := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
		if u.Scheme != "http" || !local {
			return "", errors.New("twenty base_url requires HTTPS except for loopback HTTP")
		}
	}
	u.Path = ""
	return u.String(), nil
}

func NewClient(baseURL, apiKey string) (*Client, error) {
	root, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(apiKey)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("twenty api_key is required and must not contain newlines")
	}
	// Twenty documents 100 requests/minute. Share pacing across clients that
	// use the same origin and credential so configured sources cannot each
	// spend a separate burst token.
	return &Client{endpoint: root + "/graphql", key: key, limiter: rateLimiterFor(root, key), http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) query(ctx context.Context, query string, variables map[string]any) (map[string]jsontext.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("wait for Twenty rate limit: %w", err)
	}
	body, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{query, variables})
	if err != nil {
		return nil, errors.New("encode Twenty request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create Twenty request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("twenty request failed (network or timeout)")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("twenty API returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("read Twenty response")
	}
	if len(data) > maxResponseBytes {
		return nil, errResponseTooLarge
	}
	var wire struct {
		Data   map[string]jsontext.Value `json:"data"`
		Errors jsontext.Value            `json:"errors"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return nil, errors.New("invalid Twenty response JSON")
	}
	if len(wire.Errors) > 0 && string(wire.Errors) != "null" && string(wire.Errors) != "[]" {
		return nil, errors.New("twenty GraphQL request failed; check API key permissions and Call Recorder availability")
	}
	if wire.Data == nil {
		return nil, errors.New("twenty response has no data")
	}
	return wire.Data, nil
}

type connection struct {
	Edges []struct {
		Node jsontext.Value `json:"node"`
	} `json:"edges"`
	PageInfo *struct {
		HasNextPage *bool  `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

func readConnection(raw jsontext.Value, paginated bool, current string) (*connection, error) {
	var c connection
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.Edges == nil {
		return nil, errors.New("twenty response has an invalid connection")
	}
	for _, edge := range c.Edges {
		var node struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(edge.Node, &node) != nil || strings.TrimSpace(node.ID) == "" {
			return nil, errors.New("twenty connection has an invalid node")
		}
	}
	if paginated {
		if c.PageInfo == nil || c.PageInfo.HasNextPage == nil {
			return nil, errors.New("twenty response has no valid pageInfo")
		}
		if *c.PageInfo.HasNextPage && (strings.TrimSpace(c.PageInfo.EndCursor) == "" || c.PageInfo.EndCursor == current) {
			return nil, errors.New("twenty pagination cursor did not advance")
		}
	}
	return &c, nil
}

func (c *Client) Probe(ctx context.Context) error {
	data, err := c.query(ctx, probeQuery, nil)
	if err != nil {
		return err
	}
	for _, collection := range []string{"callRecordings", "calendarEvents", "calendarEventParticipants"} {
		if _, err := readConnection(data[collection], collection != "calendarEvents", ""); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) ListRecordings(ctx context.Context, after string, first int) (*Page, error) {
	if first < 1 || first > pageSize {
		return nil, errors.New("twenty page size must be between 1 and 100")
	}
	var cursor any
	if after != "" {
		cursor = after
	}
	data, err := c.query(ctx, recordingQuery, map[string]any{"after": cursor, "first": first})
	// Full word-level transcripts can make a catalog page exceed the body
	// bound. Retry the same cursor with fewer records, retaining the bound
	// and the provider cursor rather than skipping oversized pages.
	if errors.Is(err, errResponseTooLarge) && first > 1 {
		return c.ListRecordings(ctx, after, first/2)
	}
	if err != nil {
		return nil, err
	}
	wire, err := readConnection(data["callRecordings"], true, after)
	if err != nil {
		return nil, err
	}
	page := &Page{Records: make([]Recording, 0, len(wire.Edges)), HasMore: *wire.PageInfo.HasNextPage, NextCursor: wire.PageInfo.EndCursor}
	for _, edge := range wire.Edges {
		var recording Recording
		if json.Unmarshal(edge.Node, &recording) != nil {
			return nil, errors.New("invalid Twenty recording fields")
		}
		recording.Raw = edge.Node.Clone()
		page.Records = append(page.Records, recording)
	}
	return page, nil
}

func (c *Client) GetCalendar(ctx context.Context, id string) (*Calendar, error) {
	data, err := c.query(ctx, calendarQuery, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	events, err := readConnection(data["calendarEvents"], false, "")
	if err != nil {
		return nil, err
	}
	if len(events.Edges) == 0 {
		return nil, errLinkedCalendarUnavailable
	}
	if len(events.Edges) != 1 {
		return nil, errors.New("linked Twenty calendar event is unavailable")
	}
	var event struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(events.Edges[0].Node, &event) != nil || event.ID != id {
		return nil, errors.New("linked Twenty calendar event identity mismatch")
	}
	calendar := &Calendar{Raw: events.Edges[0].Node.Clone(), Participants: []Participant{}}
	seen := map[string]bool{}
	var after string
	for {
		var cursor any
		if after != "" {
			cursor = after
		}
		data, err := c.query(ctx, participantQuery, map[string]any{"id": id, "after": cursor})
		if err != nil {
			return nil, err
		}
		page, err := readConnection(data["calendarEventParticipants"], true, after)
		if err != nil {
			return nil, err
		}
		for _, edge := range page.Edges {
			var participant Participant
			if json.Unmarshal(edge.Node, &participant) != nil {
				return nil, errors.New("invalid Twenty calendar participant fields")
			}
			participant.Raw = edge.Node.Clone()
			calendar.Participants = append(calendar.Participants, participant)
		}
		if !*page.PageInfo.HasNextPage {
			return calendar, nil
		}
		after = page.PageInfo.EndCursor
		if seen[after] {
			return nil, errors.New("twenty participant pagination repeated a cursor")
		}
		seen[after] = true
	}
}
