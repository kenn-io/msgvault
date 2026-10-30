package threads

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/netguard"
)

const (
	DefaultBaseURL     = "https://graph.threads.com/v1.0"
	commonFields       = "id,text,timestamp,username,permalink,media_type,media_url,shortcode,children,is_quote_post,quoted_post,has_replies"
	ownPostFields      = commonFields + ",owner,reposted_post"
	replyFields        = commonFields + ",root_post,replied_to,is_reply,is_reply_owned_by_me"
	conversationFields = replyFields + ",hide_status"
	maxResponseBytes   = 16 << 20
	defaultRetryWait   = 5 * time.Second
)

type ClientOptions struct {
	HTTPClient   *http.Client
	BaseURL      string
	AccessToken  string
	Budget       *RequestBudget
	MaxRetryWait time.Duration
}

// Format keeps bearer credentials out of diagnostic formatting, including %+v.
func (o ClientOptions) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "Threads client options (credentials redacted)")
}

type Client struct {
	base         *url.URL
	token        string
	http         *http.Client
	budget       *RequestBudget
	maxRetryWait time.Duration
}

var _ API = (*Client)(nil)

func (c *Client) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "Threads read client (credentials redacted)")
}

// NewClient fixes the API root for every request. Endpoint injection is for
// constructors/tests only; plain HTTP is restricted to loopback test servers.
func NewClient(o ClientOptions) (*Client, error) {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(o.BaseURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" || base.RawPath != "" {
		return nil, errors.New("invalid Threads API base URL")
	}
	if base.Scheme != "https" && (base.Scheme != "http" || !netguard.IsLoopbackHost(base.Hostname())) {
		return nil, errors.New("Threads API base URL must use HTTPS")
	}
	if o.AccessToken == "" || strings.ContainsAny(o.AccessToken, "\r\n") {
		return nil, errors.New("invalid Threads access token")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	if o.Budget == nil {
		if o.HTTPClient != nil {
			if transport, ok := o.HTTPClient.Transport.(*budgetTransport); ok {
				o.Budget = transport.budget
			}
		}
		if o.Budget == nil {
			o.Budget = NewRequestBudget(BudgetOptions{})
		}
	}
	if o.MaxRetryWait <= 0 {
		o.MaxRetryWait = defaultRetryWait
	}
	return &Client{base: base, token: o.AccessToken, http: NewBudgetedHTTPClient(o.HTTPClient, o.Budget), budget: o.Budget, maxRetryWait: o.MaxRetryWait}, nil
}

func (c *Client) Me(ctx context.Context) (Owner, error) {
	var owner Owner
	err := c.getJSON(ctx, "/me", url.Values{"fields": {"id,username,name"}}, &owner)
	if err != nil && !isPause(err) {
		return Owner{}, err
	}
	if owner.ID == "" && isPause(err) {
		return Owner{}, err
	}
	if !validID(owner.ID) {
		return Owner{}, ErrDecodeResponse
	}
	return owner, err
}

func (c *Client) OwnPosts(ctx context.Context, q ListQuery) (Page, error) {
	return c.ownList(ctx, "/me/threads", ownPostFields, q)
}
func (c *Client) OwnReplies(ctx context.Context, q ListQuery) (Page, error) {
	return c.ownList(ctx, "/me/replies", replyFields, q)
}

func (c *Client) ownList(ctx context.Context, path, fields string, q ListQuery) (Page, error) {
	// Bounds are sent as whole Unix seconds. Reject an interval that collapses
	// after conversion instead of silently widening coverage.
	since, until := q.Since.Unix(), q.Until.Unix()
	if q.Since.IsZero() || q.Until.IsZero() || since < 1688540400 || since >= until || q.Until.After(c.budget.now()) {
		return Page{}, errors.New("invalid Threads list time bounds")
	}
	params := url.Values{"fields": {fields}, "limit": {"100"}, "since": {strconv.FormatInt(since, 10)}, "until": {strconv.FormatInt(until, 10)}}
	return c.page(ctx, path, params, q.After, "")
}

func (c *Client) Conversation(ctx context.Context, root, after string) (Page, error) {
	if !validID(root) {
		return Page{}, errors.New("invalid Threads root ID")
	}
	return c.page(ctx, "/"+root+"/conversation", url.Values{"fields": {conversationFields}, "reverse": {"true"}}, after, root)
}

func (c *Client) Media(ctx context.Context, id string) (Media, error) {
	if !validID(id) {
		return Media{}, errors.New("invalid Threads media ID")
	}
	var media Media
	err := c.getJSON(ctx, "/"+id, url.Values{"fields": {replyFields}}, &media)
	if err != nil && !isPause(err) {
		return Media{}, err
	}
	if media.ID == "" && isPause(err) {
		return Media{}, err
	}
	if media.ID != id {
		return Media{}, ErrDecodeResponse
	}
	return media, err
}

func validCursor(cursor string) bool {
	if len(cursor) > 4096 {
		return false
	}
	for _, r := range cursor {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func (c *Client) page(ctx context.Context, path string, params url.Values, after, root string) (Page, error) {
	if !validCursor(after) {
		return Page{}, ErrPagination
	}
	if after != "" {
		params.Set("after", after)
	}
	var wire struct {
		Data   *[]Media `json:"data"`
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
		} `json:"paging"`
	}
	err := c.getJSON(ctx, path, params, &wire)
	if err != nil && !isPause(err) {
		return Page{}, err
	}
	if wire.Data == nil {
		if isPause(err) {
			return Page{}, err
		}
		return Page{}, ErrDecodeResponse
	}
	page := Page{Items: *wire.Data, After: wire.Paging.Cursors.After}
	if len(page.Items) == 0 {
		page.After = ""
		return page, err
	}
	if page.After == "" || page.After == after || !validCursor(page.After) {
		return Page{}, ErrPagination
	}
	seen := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		if seen[item.ID] {
			return Page{}, ErrPagination
		}
		seen[item.ID] = true
		if root != "" && item.RootPost != nil && item.RootPost.ID != root {
			return Page{}, ErrDecodeResponse
		}
	}
	return page, err
}

func isPause(err error) bool { var pause *PauseError; return errors.As(err, &pause) }

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	u := *c.base
	u.Path += path
	u.RawQuery = params.Encode()
	var retryErr error
	attempt := 0
	res, err := httpretry.Do(ctx, 3, c.maxRetryWait, func() (*http.Response, error) {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, errors.New("could not construct Threads request")
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		return c.http.Do(req)
	}, func(res *http.Response) bool {
		// Do invokes this only for retry candidates before its delay cap.
		// Retain the error body so final classification sees identical evidence.
		body, err := readBody(res)
		if err != nil {
			retryErr = err
			return false
		}
		res.Body = io.NopCloser(bytes.NewReader(body))
		api := decodeAPIError(res.StatusCode, body)
		if api.CredentialInvalid() {
			return false
		}
		if highUsage(res.Header) {
			retryErr = &PauseError{Reason: "high_usage"}
			return false
		}
		if !retryable(api) {
			return false
		}
		delay, ok := httpretry.ParseRetryAfter(res.Header.Get("Retry-After"), 0, c.budget.now())
		if !ok {
			delay = httpretry.RetryAfterAt("", attempt-1, c.maxRetryWait, c.budget.now())
		}
		if delay > c.maxRetryWait || delay >= c.budget.Remaining() {
			retryErr = &PauseError{Reason: "retry_after", RetryAfter: delay}
			return false
		}
		// Inject the actual wait here. Do's timer then receives zero, so tests
		// with real HTTP can advance a clock without real throttle sleeps. The
		// helper maximum is at least the original delay; it never shortens it.
		if err := c.budget.wait(ctx, delay); err != nil {
			retryErr = err
			return false
		}
		res.Header.Set("Retry-After", "0")
		return true
	})
	if err != nil {
		return safeRequestError(ctx, err)
	}
	defer func() { _ = res.Body.Close() }()
	if retryErr != nil {
		return retryErr
	}
	body, err := readBody(res)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		api := decodeAPIError(res.StatusCode, body)
		if api.CredentialInvalid() {
			return api
		}
		if highUsage(res.Header) {
			return &PauseError{Reason: "high_usage"}
		}
		return api
	}
	if err := json.Unmarshal(body, out); err != nil {
		return ErrDecodeResponse
	}
	if highUsage(res.Header) {
		return &PauseError{Reason: "high_usage"}
	}
	return nil
}

func safeRequestError(ctx context.Context, err error) error {
	if pause := workloadPause(ctx); pause != nil {
		return pause
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pause *PauseError
	if errors.As(err, &pause) {
		return pause
	}
	if errors.Is(err, ErrRedirect) {
		return ErrRedirect
	}
	return errors.New("threads HTTP request failed")
}

func readBody(res *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	_ = res.Body.Close()
	if err != nil {
		return nil, errors.New("could not read Threads API response")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("Threads API response exceeds size limit")
	}
	return body, nil
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Subcode int    `json:"error_subcode"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	// Retain only a narrow classification, never provider text in diagnostics.
	message := strings.ToLower(envelope.Error.Message)
	expired := envelope.Error.Code == 100 && (strings.Contains(message, "cursor has expired") || strings.Contains(message, "cursor is expired"))
	return &APIError{StatusCode: status, Code: envelope.Error.Code, Subcode: envelope.Error.Subcode, CursorExpired: expired}
}

func retryable(e *APIError) bool {
	if e.CredentialInvalid() {
		return false
	}
	switch e.Code {
	case 4, 17, 32, 613:
		return true
	}
	switch e.StatusCode {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func highUsage(h http.Header) bool {
	for _, header := range []string{"X-App-Usage", "X-Page-Usage"} {
		var app map[string]jsontext.Value
		if json.Unmarshal([]byte(h.Get(header)), &app) == nil && usageMetrics(app) {
			return true
		}
	}
	var business map[string][]map[string]jsontext.Value
	if json.Unmarshal([]byte(h.Get("X-Business-Use-Case-Usage")), &business) == nil {
		for _, uses := range business {
			for _, use := range uses {
				if usageMetrics(use) {
					return true
				}
			}
		}
	}
	return false
}

func usageMetrics(metrics map[string]jsontext.Value) bool {
	for _, key := range []string{"call_count", "total_cputime", "total_time"} {
		var percentage float64
		if value, ok := metrics[key]; ok && json.Unmarshal(value, &percentage) == nil && percentage >= 80 {
			return true
		}
	}
	return false
}
