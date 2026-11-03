package meta

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
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const graphVersion = "v26.0"
const maxResponseBytes = 8 << 20
const pageLimit = 100

var decimalID = regexp.MustCompile(`^[1-9][0-9]*$`)

// ValidAccountID validates configured app/Page IDs, not native object IDs.
func ValidAccountID(id string) bool { return decimalID.MatchString(id) }

// safeObjectID refuses path separators and URL syntax. IDs otherwise remain
// opaque: there is no numeric conversion or undocumented native-ID length cap.
func safeObjectID(id string) bool {
	return id != "" && id != "." && id != ".." && utf8.ValidString(id) &&
		!strings.ContainsAny(id, "/\\?#%") && strings.IndexFunc(id, unicode.IsControl) < 0
}

// Client binds credentials to one fixed Graph origin.
type Client struct {
	token, origin string
	http          *http.Client
}

func NewClient(token string) (*Client, error) {
	return newClient(token, "https://graph.facebook.com", http.DefaultClient)
}

// The origin override is package-local for synthetic HTTP fixtures.
func newClient(token, origin string, h *http.Client) (*Client, error) {
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid Facebook Page token")
	}
	if h == nil {
		return nil, errors.New("missing Graph HTTP client")
	}
	clone := *h
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("graph redirects are not allowed") }
	clone.Timeout = 30 * time.Second
	return &Client{token: token, origin: origin, http: &clone}, nil
}

// APIError retains no provider text, URLs, request headers or token.
type APIError struct {
	Status, Code, Subcode int
	RemainingSeconds      int
	Kind                  string
}

func (e *APIError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("graph request failed (HTTP %d)", e.Status)
	}
	if e.Subcode == 0 {
		return fmt.Sprintf("graph request failed (HTTP %d, code %d)", e.Status, e.Code)
	}
	return fmt.Sprintf("graph request failed (HTTP %d, code %d, subcode %d)", e.Status, e.Code, e.Subcode)
}

func (e *APIError) Permission() bool { return e.Code == 10 || (e.Code >= 200 && e.Code <= 299) }
func (e *APIError) Throttled() bool {
	return e.Code == 4 || e.Code == 17 || e.Code == 32 || e.Code == 613 || (e.Code >= 80000 && e.Code <= 80014)
}
func (e *APIError) TokenExpired() bool { return e.Code == 190 }
func (e *APIError) NotFound() bool     { return e.Code == 100 }

// APIErrorCode lets CLI diagnostics classify a refused provider read without
// interpreting discarded response bodies or inferring cursor expiry.
func (e *APIError) APIErrorCode() string { return "source_scope_unavailable" }

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, "", out)
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", out)
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("invalid Graph request body")
	}
	return c.do(ctx, http.MethodPost, path, nil, bytes.NewReader(data), "application/json", out)
}

func (c *Client) delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil, "", out)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string, out any) error {
	target := c.origin + "/" + graphVersion + "/" + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New("invalid Graph request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// net/http errors may embed the entire URL or a transport's credentials.
		return errors.New("graph request could not complete")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{Status: resp.StatusCode}
		const errorLimit = 64 << 10
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, errorLimit+1))
		if readErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		var wire struct {
			Error struct {
				Code    int `json:"code"`
				Subcode int `json:"error_subcode"`
			} `json:"error"`
		}
		if readErr == nil && len(data) <= errorLimit && json.Unmarshal(data, &wire) == nil {
			apiErr.Code, apiErr.Subcode = wire.Error.Code, wire.Error.Subcode
		}
		if readErr == nil && len(data) <= errorLimit {
			apiErr.Kind, apiErr.RemainingSeconds = marketingErrorFields(data)
		}
		return apiErr
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(data) > maxResponseBytes {
		return errors.New("graph response unreadable or too large")
	}
	var envelope struct {
		Error jsontext.Value `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Error) > 0 {
		return errors.New("invalid Graph response")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("invalid Graph response")
	}
	return nil
}

type graphPage[T any] struct {
	Data  []T
	After string
}

// readObjectPage shares bounded paging while callers choose explicit wire types.
// The ID projection applies the same producer validation to every platform.
func readObjectPage[T any](ctx context.Context, c *Client, path, after string, q url.Values, id func(T) string) (graphPage[T], error) {
	if !c.safeCursor(after) {
		return graphPage[T]{}, errors.New("invalid Graph cursor")
	}
	if after != "" {
		q.Set("after", after)
	}
	var wire struct {
		Data   []T `json:"data"`
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
			Next string `json:"next"`
		} `json:"paging"`
	}
	if err := c.get(ctx, path, q, &wire); err != nil {
		return graphPage[T]{}, err
	}
	if wire.Data == nil || len(wire.Data) > pageLimit {
		return graphPage[T]{}, errors.New("graph response missing or oversized data page")
	}
	next := ""
	if wire.Paging.Next != "" {
		next = wire.Paging.Cursors.After
		if next == "" || next == after || !c.safeCursor(next) {
			return graphPage[T]{}, errors.New("invalid Graph pagination progress")
		}
	}
	for _, object := range wire.Data {
		if !safeObjectID(id(object)) {
			return graphPage[T]{}, errors.New("invalid Graph object identity")
		}
	}
	return graphPage[T]{Data: wire.Data, After: next}, nil
}
