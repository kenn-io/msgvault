package bland

import (
	"bufio"
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

	"go.kenn.io/msgvault/internal/callsync"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/httpretry"
)

var (
	ErrNotFound       = errors.New("bland artifact not found")
	ErrAuthentication = errors.New("bland authentication failed")
	ErrRateLimited    = errors.New("bland rate limited")
	ErrInvalidPayload = errors.New("invalid Bland payload")
)

// HTTPError is a Bland error response other than not-found.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("bland HTTP %d", e.StatusCode) }

// Retryable reports throttling or a server error, which a later run may not
// repeat.
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= http.StatusInternalServerError
}

const maxJSONBytes = 32 << 20

type Client struct {
	base         string
	key          string
	EncryptedKey string
	http         *http.Client
}

func NewClient(baseURL, key string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	apiHTTP := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("%w: bland redirect limit", callsync.ErrRedirectRefused)
		}
		if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host || req.URL.User != nil {
			return fmt.Errorf("%w: bland cross-origin redirect", callsync.ErrRedirectRefused)
		}
		return nil
	}}
	return &Client{base: strings.TrimRight(baseURL, "/"), key: key, http: apiHTTP}
}

// ListOptions pages calls in creation order, oldest first unless Newest.
// CreatedAfter (an ISO 8601 date or timestamp) is an inclusive creation bound.
type ListOptions struct {
	Offset, Limit int
	CreatedAfter  string
	Newest        bool
}
type Page struct {
	Calls []*Call
	Total *int
}

func (c *Client) request(ctx context.Context, client *http.Client, endpoint string, q url.Values, audio bool) (*http.Response, error) {
	u, e := url.Parse(c.base)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid Bland API base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	u.RawQuery = q.Encode()
	res, e := httpretry.Do(ctx, 3, 30*time.Second, func() (*http.Response, error) {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", c.key)
		if c.EncryptedKey != "" && (endpoint == "/calls" || strings.HasPrefix(endpoint, "/calls/")) {
			req.Header.Set("Encrypted_key", c.EncryptedKey)
		}
		if audio {
			req.Header.Set("Content-Type", "audio/mpeg")
			req.Header.Set("Accept", "audio/mpeg")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		res, e := client.Do(req)
		if e != nil && ctx.Err() == nil {
			if errors.Is(e, callsync.ErrRedirectRefused) {
				return nil, fmt.Errorf("bland request: %w", callsync.ErrRedirectRefused)
			}
			// net/http's error carries the URL, which may be a signed storage link.
			return nil, fmt.Errorf("bland request failed: %w", callsync.ErrTransport)
		}
		return res, e
	}, func(res *http.Response) bool {
		return (&HTTPError{StatusCode: res.StatusCode}).Retryable()
	})
	if e != nil {
		return nil, e
	}
	if res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices {
		return res, nil
	}
	var head []byte
	if audio {
		head, _ = io.ReadAll(io.LimitReader(res.Body, 512))
	}
	_ = res.Body.Close()
	// Bland answers a recording it hasn't produced yet with this code on any status.
	if errorCode(head) == recordingNotFound {
		return nil, ErrNotFound
	}
	// A recording Bland refuses or lacks is that recording's failure, not the account's.
	if audio && (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound) {
		return nil, &HTTPError{StatusCode: res.StatusCode}
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %w", ErrAuthentication, &HTTPError{StatusCode: res.StatusCode})
	case res.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case res.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: %w", ErrRateLimited, &HTTPError{StatusCode: res.StatusCode})
	case res.StatusCode >= http.StatusInternalServerError:
		return nil, fmt.Errorf("%w after retries", &HTTPError{StatusCode: res.StatusCode})
	}
	return nil, &HTTPError{StatusCode: res.StatusCode}
}
func (c *Client) json(ctx context.Context, endpoint string, q url.Values) (jsontext.Value, error) {
	res, e := c.request(ctx, c.http, endpoint, q, false)
	if e != nil {
		return nil, e
	}
	defer func() { _ = res.Body.Close() }()
	b, e := io.ReadAll(io.LimitReader(res.Body, maxJSONBytes+1))
	if e != nil {
		return nil, e
	}
	if len(b) > maxJSONBytes {
		return nil, ErrInvalidPayload
	}
	return b, nil
}

// ListCalls sorts by creation, which edits never change, so paging by offset
// can't skip a call.
func (c *Client) ListCalls(ctx context.Context, o ListOptions) (*Page, error) {
	if o.Limit <= 0 || o.Offset < 0 {
		return nil, errors.New("invalid Bland page bounds")
	}
	q := url.Values{"from": {strconv.Itoa(o.Offset)}, "to": {strconv.Itoa(o.Offset + o.Limit)}, "limit": {strconv.Itoa(o.Limit)}, "ascending": {strconv.FormatBool(!o.Newest)}, "sort_by": {"created_at"}}
	if o.CreatedAfter != "" {
		q.Set("start_date", o.CreatedAfter)
	}
	b, e := c.json(ctx, "/calls", q)
	if e != nil {
		return nil, e
	}
	var w struct {
		Calls []jsontext.Value `json:"calls"`
		Count *int             `json:"count"`
		Total *int             `json:"total_count"`
	}
	if e = json.Unmarshal(b, &w); e != nil || w.Calls == nil || w.Count == nil || *w.Count != len(w.Calls) || len(w.Calls) > o.Limit || (w.Total != nil && *w.Total < 0) {
		return nil, ErrInvalidPayload
	}
	p := &Page{Total: w.Total}
	for _, r := range w.Calls {
		call, e := decodeListed(r)
		if e != nil {
			return nil, e
		}
		p.Calls = append(p.Calls, call)
	}
	return p, nil
}
func (c *Client) GetCall(ctx context.Context, id string) (*Call, error) {
	if !validID(id) {
		return nil, errors.New("invalid Bland call ID")
	}
	b, e := c.json(ctx, "/calls/"+id, nil)
	if e != nil {
		return nil, e
	}
	return decodeCall(b, id, true)
}
func (c *Client) GetPostCall(ctx context.Context, id string) (*PostCall, error) {
	if !validID(id) {
		return nil, errors.New("invalid Bland call ID")
	}
	b, e := c.json(ctx, "/postcall/webhooks/"+id, nil)
	if e != nil {
		return nil, e
	}
	return parsePostCall(b, id)
}

// PostCall is a postcall webhook response, read once: the raw response the
// archive keeps and its decoded payload.
type PostCall struct {
	Raw  jsontext.Value
	Call *Call
}

// parsePostCall reads a postcall webhook response. Null data with an errors
// array is ErrNotFound, and no data without errors is malformed; id, when
// set, must match the payload's call.
func parsePostCall(b []byte, id string) (*PostCall, error) {
	var w struct {
		Data *struct {
			ID      string         `json:"call_id"`
			Payload jsontext.Value `json:"payload"`
		} `json:"data"`
		Errors []jsontext.Value `json:"errors"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, malformed("postcall", err)
	}
	// Bland reports a call without postcall webhook data as null data with
	// an errors array of coded errors, whatever the code.
	if w.Data == nil && len(w.Errors) > 0 {
		for _, raw := range w.Errors {
			var entry struct {
				Code string `json:"error"`
			}
			if raw.Kind() != '{' || json.Unmarshal(raw, &entry) != nil || entry.Code == "" {
				return nil, malformed("errors", errors.New("entry without an error code"))
			}
		}
		return nil, ErrNotFound
	}
	if w.Data == nil {
		return nil, malformed("data", errors.New("missing"))
	}
	if id != "" && w.Data.ID != "" && w.Data.ID != id {
		return nil, malformed("data.call_id", errors.New("names another call"))
	}
	if w.Data.Payload.Kind() != '{' {
		return nil, malformed("payload", errors.New("not an object"))
	}
	payload, err := decodeDoc(w.Data.Payload)
	if err != nil {
		return nil, err
	}
	if id != "" && payload.ID != "" && payload.ID != id {
		return nil, malformed("payload.call_id", errors.New("names another call"))
	}
	return &PostCall{Raw: append(jsontext.Value(nil), b...), Call: payload}, nil
}

// recordingNotFound is Bland's error code for a recording it doesn't have yet.
const recordingNotFound = "CALL_RECORDING_NOT_FOUND"

// errorCode returns the code of the first error in a Bland error envelope.
func errorCode(b []byte) string {
	var w struct {
		Errors []struct {
			Code string `json:"error"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &w) != nil || len(w.Errors) == 0 {
		return ""
	}
	return w.Errors[0].Code
}

type Recording struct {
	Body io.ReadCloser
	MIME string
}

// OpenRecording returns the call's MP3 or WAV audio. It refuses a declared size
// over maxBytes; the caller caps the stream itself.
func (c *Client) OpenRecording(ctx context.Context, id string, maxBytes int64) (*Recording, error) {
	if !validID(id) {
		return nil, errors.New("invalid Bland call ID")
	}
	if maxBytes <= 0 {
		return nil, errors.New("bland recording byte limit must be positive")
	}
	res, e := c.request(ctx, callsync.MediaClient(c.http, maxBytes), "/recordings/"+id, nil, true)
	if e != nil {
		return nil, e
	}
	if res.ContentLength > maxBytes {
		return nil, errors.Join(export.ErrAttachmentTooLarge, res.Body.Close())
	}
	b := bufio.NewReader(res.Body)
	prefix, e := b.Peek(512)
	if e != nil && !errors.Is(e, io.EOF) {
		_ = res.Body.Close()
		return nil, fmt.Errorf("read Bland recording header: %w", e)
	}
	if errorCode(prefix) == recordingNotFound {
		_ = res.Body.Close()
		return nil, ErrNotFound
	}
	mime := strings.ToLower(strings.Split(res.Header.Get("Content-Type"), ";")[0])
	// Validate audio signatures even when an error response claims an audio MIME.
	mp3 := bytes.HasPrefix(prefix, []byte("ID3")) || len(prefix) >= 2 && prefix[0] == 0xff && (prefix[1]&0xe0) == 0xe0
	wav := len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WAVE"
	if (!mp3 && !wav) || strings.Contains(mime, "json") || strings.Contains(mime, "html") {
		_ = res.Body.Close()
		return nil, errors.New("bland: recording response is not audio")
	}
	mime = "audio/mpeg"
	if wav {
		mime = "audio/wav"
	}
	return &Recording{Body: struct {
		io.Reader
		io.Closer
	}{b, res.Body}, MIME: mime}, nil
}
