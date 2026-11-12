package meta

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

// Client is GET-only and binds credentials to one fixed Graph origin.
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
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("graph request failed (HTTP %d)", e.Status) }

// APIErrorCode lets CLI diagnostics classify a refused provider read without
// interpreting discarded response bodies or inferring cursor expiry.
func (e *APIError) APIErrorCode() string { return "source_scope_unavailable" }

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+"/"+graphVersion+"/"+path+"?"+q.Encode(), nil)
	if err != nil {
		return errors.New("invalid Graph request")
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
		return &APIError{Status: resp.StatusCode}
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
