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
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"golang.org/x/time/rate"
)

const maxResponseBytes = 64 << 20
const pageSize = 100
const maxAttempts = 5

const recordingFields = `id title status createdAt updatedAt startedAt endedAt calendarEventId transcript summary { markdown } calendarEvent { ` + calendarFields + ` }`
const calendarFields = `id title startsAt endsAt`
const participantFields = `id calendarEventId displayName handle isOrganizer`
const recordingQuery = `query Recordings($after: String, $first: Int!, $filter: CallRecordingFilterInput) {
 callRecordings(first: $first, after: $after, filter: $filter, orderBy: [{updatedAt: AscNullsFirst}, {id: AscNullsFirst}]) {
 edges { node { ` + recordingFields + ` } } pageInfo { hasNextPage endCursor }
 } }`
const recordingIDQuery = `query RecordingIDs($after: String, $filter: CallRecordingFilterInput) {
 callRecordings(first: 1, after: $after, filter: $filter, orderBy: [{updatedAt: AscNullsFirst}, {id: AscNullsFirst}]) {
 edges { node { id updatedAt } } pageInfo { hasNextPage endCursor }
 } }`
const participantQuery = `query Participants($ids: [UUID!]!, $after: String) {
 calendarEventParticipants(filter: {calendarEventId: {in: $ids}}, first: 100, after: $after, orderBy: [{id: AscNullsLast}]) {
 edges { node { ` + participantFields + ` } } pageInfo { hasNextPage endCursor }
 } }`
const probeQuery = `query Probe {
 callRecordings(first: 1, orderBy: [{updatedAt: AscNullsFirst}, {id: AscNullsFirst}]) { edges { node { ` + recordingFields + ` } } pageInfo { hasNextPage endCursor } }
 calendarEvents(first: 1) { edges { node { ` + calendarFields + ` } } }
 calendarEventParticipants(first: 1) { edges { node { ` + participantFields + ` } } pageInfo { hasNextPage endCursor } }
 }`

var errResponseTooLarge = errors.New("twenty response exceeds 64 MiB")

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

// query retries throttling, server errors and network failures, the way the
// other meeting clients do, so one transient error does not fail a sync.
func (c *Client) query(ctx context.Context, query string, variables map[string]any) (map[string]jsontext.Value, error) {
	body, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{query, variables})
	if err != nil {
		return nil, errors.New("encode Twenty request")
	}
	for attempt := 0; ; attempt++ {
		data, retryAfter, err := c.send(ctx, body)
		if retryAfter == nil || attempt+1 >= maxAttempts {
			return data, err
		}
		timer := time.NewTimer(httpretry.RetryAfter(*retryAfter, attempt, httpretry.ProviderMaxRetryAfter))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// send makes one request. A non-nil retryAfter marks a transient failure and
// carries the provider's Retry-After value, which may be empty.
func (c *Client) send(ctx context.Context, body []byte) (map[string]jsontext.Value, *string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, nil, fmt.Errorf("wait for Twenty rate limit: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("create Twenty request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, new(string), errors.New("twenty request failed (network or timeout)")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		retryAfter := response.Header.Get("Retry-After")
		return nil, &retryAfter, fmt.Errorf("twenty API returned HTTP %d", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("twenty API returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, new(string), errors.New("read Twenty response")
	}
	if len(data) > maxResponseBytes {
		return nil, nil, errResponseTooLarge
	}
	var wire struct {
		Data   map[string]jsontext.Value `json:"data"`
		Errors []struct {
			Extensions struct {
				Code         string  `json:"code"`
				RetryAfterMS float64 `json:"retryAfterMs"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return nil, nil, errors.New("invalid Twenty response JSON")
	}
	if len(wire.Errors) > 0 {
		// Twenty reports its workspace request limit as a GraphQL error.
		for _, graphqlErr := range wire.Errors {
			if graphqlErr.Extensions.Code == "RATE_LIMITED" {
				retryAfter := ""
				if graphqlErr.Extensions.RetryAfterMS > 0 {
					retryAfter = strconv.FormatInt(int64(math.Ceil(graphqlErr.Extensions.RetryAfterMS/1000)), 10)
				}
				return nil, &retryAfter, errors.New("twenty API rate limit reached")
			}
		}
		return nil, nil, errors.New("twenty GraphQL request failed; check API key permissions and Call Recorder availability")
	}
	if wire.Data == nil {
		return nil, nil, errors.New("twenty response has no data")
	}
	return wire.Data, nil, nil
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

func (c *Client) ListRecordings(ctx context.Context, updatedSince, after string, first int) (*Page, error) {
	if first < 1 || first > pageSize {
		return nil, errors.New("twenty page size must be between 1 and 100")
	}
	var cursor any
	if after != "" {
		cursor = after
	}
	variables := map[string]any{"after": cursor, "first": first, "filter": map[string]any{"updatedAt": map[string]any{"gte": updatedSince}}}
	data, err := c.query(ctx, recordingQuery, variables)
	// Full word-level transcripts can make a catalog page exceed the body
	// bound. Retry the same cursor with fewer records, and step past a single
	// recording that alone exceeds it so the importer can skip it.
	if errors.Is(err, errResponseTooLarge) {
		if first > 1 {
			return c.ListRecordings(ctx, updatedSince, after, first/2)
		}
		return c.tooLargeRecording(ctx, variables, after)
	}
	if err != nil {
		return nil, err
	}
	wire, err := readConnection(data["callRecordings"], true, after)
	if err != nil {
		return nil, err
	}
	page := &Page{Records: make([]Recording, 0, len(wire.Edges)), HasMore: *wire.PageInfo.HasNextPage, NextCursor: wire.PageInfo.EndCursor}
	calendars := map[string]*Calendar{}
	for _, edge := range wire.Edges {
		var recording Recording
		var fields map[string]jsontext.Value
		if json.Unmarshal(edge.Node, &recording) != nil || json.Unmarshal(edge.Node, &fields) != nil {
			return nil, errors.New("invalid Twenty recording fields")
		}
		event := fields["calendarEvent"]
		delete(fields, "calendarEvent")
		if recording.Raw, err = json.Marshal(fields, json.Deterministic(true)); err != nil {
			return nil, errors.New("invalid Twenty recording fields")
		}
		if len(event) > 0 && string(event) != "null" {
			var linked struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event, &linked) != nil || linked.ID == "" || linked.ID != recording.CalendarEventID {
				return nil, errors.New("linked Twenty calendar event identity mismatch")
			}
			if calendars[linked.ID] == nil {
				calendars[linked.ID] = &Calendar{Raw: event.Clone(), Participants: []jsontext.Value{}}
			}
			recording.Calendar = calendars[linked.ID]
		}
		page.Records = append(page.Records, recording)
	}
	if err := c.readParticipants(ctx, calendars); err != nil {
		return nil, err
	}
	return page, nil
}

func (c *Client) tooLargeRecording(ctx context.Context, variables map[string]any, after string) (*Page, error) {
	data, err := c.query(ctx, recordingIDQuery, map[string]any{"after": variables["after"], "filter": variables["filter"]})
	if err != nil {
		return nil, err
	}
	wire, err := readConnection(data["callRecordings"], true, after)
	if err != nil {
		return nil, err
	}
	page := &Page{HasMore: *wire.PageInfo.HasNextPage, NextCursor: wire.PageInfo.EndCursor}
	for _, edge := range wire.Edges {
		recording := Recording{TooLarge: true}
		if json.Unmarshal(edge.Node, &recording) != nil {
			return nil, errors.New("invalid Twenty recording fields")
		}
		page.Records = append(page.Records, recording)
	}
	return page, nil
}

// readParticipants fills every calendar on a page with one paged query
// rather than a request per recording.
func (c *Client) readParticipants(ctx context.Context, calendars map[string]*Calendar) error {
	if len(calendars) == 0 {
		return nil
	}
	ids := make([]string, 0, len(calendars))
	for id := range calendars {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var after string
	for {
		var cursor any
		if after != "" {
			cursor = after
		}
		data, err := c.query(ctx, participantQuery, map[string]any{"ids": ids, "after": cursor})
		if err != nil {
			return err
		}
		page, err := readConnection(data["calendarEventParticipants"], true, after)
		if err != nil {
			return err
		}
		for _, edge := range page.Edges {
			var participant struct {
				CalendarEventID string `json:"calendarEventId"`
			}
			if json.Unmarshal(edge.Node, &participant) != nil || calendars[participant.CalendarEventID] == nil {
				return errors.New("invalid Twenty calendar participant fields")
			}
			calendar := calendars[participant.CalendarEventID]
			calendar.Participants = append(calendar.Participants, edge.Node.Clone())
		}
		if !*page.PageInfo.HasNextPage {
			return nil
		}
		after = page.PageInfo.EndCursor
	}
}
