package twilio

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/httpretry"
)

const maxJSONBytes = 16 << 20
const maxPages = 10000
const pageSizeQuery = "PageSize"

// includeSoftDeleted lists recordings Twilio deleted in the last 40 days too,
// so their calls are still revisited.
const includeSoftDeleted = "IncludeSoftDeleted"

var sidPattern = regexp.MustCompile(`^[A-Z]{2}[0-9a-fA-F]{32}$`)

// APIError contains content-free provider failure information.
type APIError struct {
	Service    string
	StatusCode int
}

func (e *APIError) Error() string { return fmt.Sprintf("twilio %s: HTTP %d", e.Service, e.StatusCode) }
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= http.StatusInternalServerError
}

// Client issues GET requests only. Service origins are pinned for all pagination.
type Client struct {
	options            Options
	http               *http.Client
	origins            map[string]*url.URL
	username, password string
}

func NewClient(options Options) (*Client, error) {
	// These checks own the [[twilio]] credential rules; errors never echo secrets.
	if !validSID(options.AccountSID, "AC") {
		return nil, errors.New("twilio: account_sid must be an AC SID")
	}
	if (options.APIKeySID == "") != (options.APIKeySecret == "") {
		return nil, errors.New("twilio: api_key_sid and api_key_secret must be configured together")
	}
	if options.APIKeySID != "" && options.AuthToken != "" {
		return nil, errors.New("twilio: API key credentials and auth_token are mutually exclusive")
	}
	if options.APIKeySID == "" && options.AuthToken == "" {
		return nil, errors.New("twilio: configure api_key_sid and api_key_secret, or auth_token")
	}
	if options.APIKeySID != "" && !validSID(options.APIKeySID, "SK") {
		return nil, errors.New("twilio: api_key_sid must be an SK SID")
	}
	if options.IntelligenceServiceSID != "" && !validSID(options.IntelligenceServiceSID, "GA") {
		return nil, errors.New("twilio: intelligence_service_sid must be a GA SID")
	}
	region := strings.ToLower(options.Region)
	if region == "" {
		region = "us1"
	}
	options.Region = region
	voice := "https://api.twilio.com"
	switch region {
	case "us1":
	case "ie1":
		voice = "https://api.dublin.ie1.twilio.com"
	case "au1":
		voice = "https://api.sydney.au1.twilio.com"
	default:
		return nil, errors.New("twilio: region must be us1, ie1, or au1")
	}
	defaults := map[string]string{"voice": voice, "intelligence": "https://intelligence.twilio.com"}
	c := &Client{options: options, origins: make(map[string]*url.URL), username: options.AccountSID, password: options.AuthToken}
	if options.APIKeySID != "" {
		c.username = options.APIKeySID
		c.password = options.APIKeySecret
	}
	for service, endpoint := range defaults {
		if override := options.Endpoints[service]; override != "" {
			endpoint = override
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("twilio: invalid %s endpoint", service)
		}
		u.Path = ""
		c.origins[service] = u
	}
	base := options.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 60 * time.Second}
	}
	copyClient := *base
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("%w: twilio API redirects are not permitted", callsync.ErrRedirectRefused)
	}
	c.http = &copyClient
	return c, nil
}
func validSID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && sidPattern.MatchString(value)
}
func (c *Client) accountPath() string { return "/2010-04-01/Accounts/" + c.options.AccountSID }
func (c *Client) endpoint(service, path string, query url.Values) *url.URL {
	u := *c.origins[service]
	u.Path = path
	u.RawQuery = query.Encode()
	return &u
}
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Host == b.Host && b.User == nil && b.Fragment == ""
}
func (c *Client) nextURL(service string, current *url.URL, next, path string) (*url.URL, error) {
	candidate, err := url.Parse(next)
	if err != nil {
		return nil, errors.New("twilio: malformed pagination URL")
	}
	candidate = current.ResolveReference(candidate)
	if !sameOrigin(c.origins[service], candidate) || candidate.Path != path {
		return nil, errors.New("twilio: pagination left its service collection")
	}
	return candidate, nil
}
func (c *Client) getJSON(ctx context.Context, service string, u *url.URL, target any) (retErr error) {
	response, err := c.get(ctx, c.http, service, u)
	if err != nil {
		return err
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("twilio %s: close response: %w", service, err))
		}
	}()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("twilio %s: read response: %w", service, err)
	}
	if len(data) > maxJSONBytes {
		return errors.New("twilio: JSON response exceeds byte limit")
	}
	if err = json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("twilio %s: invalid JSON payload", service)
	}
	return nil
}
func (c *Client) get(ctx context.Context, client *http.Client, service string, u *url.URL) (*http.Response, error) {
	response, err := httpretry.Do(ctx, 3, 10*time.Second, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, errors.New("twilio: invalid request URL")
		}
		req.SetBasicAuth(c.username, c.password)
		response, err := client.Do(req)
		if err != nil && ctx.Err() == nil {
			if errors.Is(err, callsync.ErrRedirectRefused) {
				return nil, fmt.Errorf("twilio %s: %w", service, callsync.ErrRedirectRefused)
			}
			return nil, fmt.Errorf("twilio %s: request failed: %w", service, callsync.ErrTransport)
		}
		return response, err
	}, func(response *http.Response) bool { return (&APIError{StatusCode: response.StatusCode}).Retryable() })
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	_ = response.Body.Close()
	return nil, &APIError{Service: service, StatusCode: response.StatusCode}
}

// collectionPage reads one page of a pinned collection. Filters survive every
// cursor; page size and page tokens may change as traversal resumes.
func (c *Client) collectionPage(ctx context.Context, service, path, key string, query url.Values, cursor string) ([]jsontext.Value, string, error) {
	current := c.endpoint(service, path, query)
	if cursor != "" {
		resolved, err := c.nextURL(service, current, cursor, path)
		if err != nil {
			return nil, "", err
		}
		current = resolved
	}
	var envelope map[string]jsontext.Value
	if err := c.getJSON(ctx, service, current, &envelope); err != nil {
		return nil, "", err
	}
	raw, ok := envelope[key]
	if !ok || string(raw) == "null" {
		return nil, "", fmt.Errorf("twilio %s: missing %s collection", service, key)
	}
	var values []jsontext.Value
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, "", errors.New("twilio: invalid collection payload")
	}
	var next string
	if raw := envelope["next_page_uri"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, "", errors.New("twilio: invalid next page URI")
		}
	}
	var meta struct {
		NextPageURL string `json:"next_page_url"`
	}
	if raw := envelope["meta"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, "", errors.New("twilio: invalid pagination metadata")
		}
	}
	if next == "" {
		next = meta.NextPageURL
	}
	if next == "" {
		return values, "", nil
	}
	u, err := c.nextURL(service, current, next, path)
	if err != nil {
		return nil, "", err
	}
	return values, u.String(), nil
}

// collection traverses only a pinned collection. Filters survive every token/page.
func (c *Client) collection(ctx context.Context, service, path, key string, query url.Values) ([]jsontext.Value, error) {
	cursor := c.endpoint(service, path, query).String()
	seen := map[string]bool{}
	var items []jsontext.Value
	for range maxPages {
		if seen[cursor] {
			return nil, errors.New("twilio: pagination loop")
		}
		seen[cursor] = true
		values, next, err := c.collectionPage(ctx, service, path, key, query, cursor)
		if err != nil {
			return nil, err
		}
		items = append(items, values...)
		if next == "" {
			return items, nil
		}
		cursor = next
	}
	return nil, errors.New("twilio: pagination exceeded page limit")
}
func (c *Client) validateAccount(value string) error {
	if value != "" && value != c.options.AccountSID {
		return errors.New("twilio: foreign account in response")
	}
	return nil
}

// decodeRecordings validates one recordings payload. callSID, when set, pins
// every recording to that call.
func (c *Client) decodeRecordings(items []jsontext.Value, callSID string) ([]Recording, error) {
	seen := map[string]bool{}
	var result []Recording
	for _, raw := range items {
		var value Recording
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("twilio: invalid recording payload")
		}
		if !validSID(value.SID, "RE") || value.CallSID != "" && !validSID(value.CallSID, "CA") {
			return nil, errors.New("twilio: invalid recording identity")
		}
		if err := c.validateAccount(value.AccountSID); err != nil {
			return nil, err
		}
		if callSID != "" && value.CallSID != callSID {
			return nil, errors.New("twilio: recording belongs to another call")
		}
		if !seen[value.SID] {
			seen[value.SID] = true
			result = append(result, value)
		}
	}
	return result, nil
}

// ListRecordingsPage reads one validated page of account-wide recordings.
func (c *Client) ListRecordingsPage(ctx context.Context, after time.Time, pageSize int, cursor string) ([]Recording, string, error) {
	q := url.Values{pageSizeQuery: {strconv.Itoa(pageSize)}, includeSoftDeleted: {"true"}}
	if !after.IsZero() {
		q.Set("DateCreated>", after.UTC().Format("2006-01-02"))
	}
	raw, next, err := c.collectionPage(ctx, "voice", c.accountPath()+"/Recordings.json", "recordings", q, cursor)
	if err != nil {
		return nil, "", err
	}
	recordings, err := c.decodeRecordings(raw, "")
	if err != nil {
		return nil, "", err
	}
	return recordings, next, nil
}

func (c *Client) CallRecordings(ctx context.Context, id string) ([]Recording, error) {
	if !validSID(id, "CA") {
		return nil, errors.New("twilio: invalid call SID")
	}
	items, err := c.collection(ctx, "voice", c.accountPath()+"/Calls/"+id+"/Recordings.json", "recordings", url.Values{pageSizeQuery: {"1000"}, includeSoftDeleted: {"true"}})
	if err != nil {
		return nil, err
	}
	return c.decodeRecordings(items, id)
}

func (c *Client) GetCall(ctx context.Context, id string) (Call, error) {
	var call Call
	if !validSID(id, "CA") {
		return call, errors.New("twilio: invalid call SID")
	}
	if err := c.getJSON(ctx, "voice", c.endpoint("voice", c.accountPath()+"/Calls/"+id+".json", nil), &call); err != nil {
		return Call{}, err
	}
	if call.SID != id {
		return Call{}, errors.New("twilio: call SID mismatch")
	}
	if err := c.validateAccount(call.AccountSID); err != nil {
		return Call{}, err
	}
	return call, nil
}
