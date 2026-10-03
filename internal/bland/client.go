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
)

var (
	ErrNotFound       = errors.New("bland artifact not found")
	ErrAuthentication = errors.New("bland authentication failed")
	ErrRateLimited    = errors.New("bland rate limited")
	ErrInvalidPayload = errors.New("invalid Bland payload")
)

const maxJSONBytes = 32 << 20

type Client struct {
	base         string
	key          string
	EncryptedKey string
	http         *http.Client
	mediaHTTP    *http.Client
}

func NewClient(baseURL, key string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	apiHTTP := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("bland redirect limit")
		}
		if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host || req.URL.User != nil {
			return errors.New("bland cross-origin redirect refused")
		}
		return nil
	}}
	mediaHTTP := *apiHTTP
	mediaHTTP.Timeout = 0
	return &Client{base: strings.TrimRight(baseURL, "/"), key: key, http: apiHTTP, mediaHTTP: &mediaHTTP}
}

type ListOptions struct {
	Offset, Limit                        int
	UpdateStart, UpdateEnd, CreatedAfter string
}
type Page struct {
	Calls []*Call
	Total *int
}

func (c *Client) request(ctx context.Context, endpoint string, q url.Values, audio bool) (*http.Response, error) {
	u, e := url.Parse(c.base)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid Bland API base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	u.RawQuery = q.Encode()
	for attempt := range 3 {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", c.key)
		if c.EncryptedKey != "" && (endpoint == "/calls" || (strings.HasPrefix(endpoint, "/calls/") && !strings.HasSuffix(endpoint, "/correct"))) {
			req.Header.Set("Encrypted_key", c.EncryptedKey)
		}
		if audio {
			req.Header.Set("Content-Type", "audio/mpeg")
			req.Header.Set("Accept", "audio/mpeg")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		client := c.http
		if audio {
			client = c.mediaHTTP
		}
		res, e := client.Do(req)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("bland request transport failed")
		}
		if res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices {
			return res, nil
		}
		_ = res.Body.Close()
		switch res.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("%w (HTTP %d)", ErrAuthentication, res.StatusCode)
		case http.StatusNotFound:
			return nil, ErrNotFound
		}
		if res.StatusCode != http.StatusTooManyRequests && res.StatusCode < http.StatusInternalServerError {
			return nil, fmt.Errorf("bland HTTP %d", res.StatusCode)
		}
		if attempt == 2 {
			if res.StatusCode == http.StatusTooManyRequests {
				return nil, ErrRateLimited
			}
			return nil, fmt.Errorf("bland HTTP %d after retries", res.StatusCode)
		}
		delay := time.Duration(1<<attempt) * time.Second
		if s := res.Header.Get("Retry-After"); s != "" {
			if n, e := strconv.Atoi(s); e == nil && n >= 0 {
				delay = time.Duration(n) * time.Second
			} else if t, e := http.ParseTime(s); e == nil {
				delay = time.Until(t)
			}
		}
		if delay > 30*time.Second {
			if res.StatusCode == http.StatusTooManyRequests {
				return nil, fmt.Errorf("%w: retry later", ErrRateLimited)
			}
			delay = 30 * time.Second
		}
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("bland retry exhausted")
}
func (c *Client) json(ctx context.Context, endpoint string, q url.Values) (jsontext.Value, error) {
	res, e := c.request(ctx, endpoint, q, false)
	if e != nil {
		return nil, e
	}
	defer func() { _ = res.Body.Close() }()
	b, e := io.ReadAll(io.LimitReader(res.Body, maxJSONBytes+1))
	if e != nil {
		return nil, e
	}
	if len(b) > maxJSONBytes || !jsontext.Value(b).IsValid() {
		return nil, ErrInvalidPayload
	}
	return b, nil
}
func (c *Client) ListCalls(ctx context.Context, o ListOptions) (*Page, error) {
	if o.Limit <= 0 || o.Offset < 0 {
		return nil, errors.New("invalid Bland page bounds")
	}
	q := url.Values{"from": {strconv.Itoa(o.Offset)}, "limit": {strconv.Itoa(o.Limit)}, "ascending": {"true"}, "sort_by": {"updated_at"}}
	for k, v := range map[string]string{"update_start_date": o.UpdateStart, "update_end_date": o.UpdateEnd, "start_date": o.CreatedAfter} {
		if v != "" {
			q.Set(k, v)
		}
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
		call, e := parseCall(r, "")
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
	return parseCall(b, id)
}
func (c *Client) GetPostCall(ctx context.Context, id string) (jsontext.Value, error) {
	if !validID(id) {
		return nil, errors.New("invalid Bland call ID")
	}
	b, e := c.json(ctx, "/postcall/webhooks/"+id, nil)
	if e != nil {
		return nil, e
	}
	var w struct {
		Data *struct {
			ID      string         `json:"call_id"`
			Payload jsontext.Value `json:"payload"`
		} `json:"data"`
		Errors []jsontext.Value `json:"errors"`
	}
	if e = json.Unmarshal(b, &w); e != nil {
		return nil, ErrInvalidPayload
	}
	if len(w.Errors) > 0 {
		return nil, ErrInvalidPayload
	}
	if w.Data == nil {
		return nil, ErrNotFound
	}
	if w.Data.ID != "" && w.Data.ID != id {
		return nil, ErrInvalidPayload
	}
	var payload map[string]jsontext.Value
	if json.Unmarshal(w.Data.Payload, &payload) != nil || payload == nil {
		return nil, ErrInvalidPayload
	}
	for _, k := range []string{"call_id", "c_id"} {
		if raw, ok := payload[k]; ok {
			var pid string
			if json.Unmarshal(raw, &pid) != nil || pid != id {
				return nil, ErrInvalidPayload
			}
		}
	}
	return b, nil
}

// GetCorrectedTranscript is opt-in at the importer: provider docs do not
// establish whether this GET can trigger processing or billing.
func (c *Client) GetCorrectedTranscript(ctx context.Context, id string) (jsontext.Value, error) {
	if !validID(id) {
		return nil, errors.New("invalid Bland call ID")
	}
	raw, err := c.json(ctx, "/calls/"+id+"/correct", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Status    string         `json:"status"`
		Corrected jsontext.Value `json:"corrected"`
	}
	if json.Unmarshal(raw, &result) != nil || (result.Status != "" && result.Status != "success") || len(result.Corrected) == 0 || result.Corrected[0] != '[' {
		return nil, ErrInvalidPayload
	}
	if _, _, err := decodeSegments(result.Corrected, true, nil, false); err != nil {
		return nil, ErrInvalidPayload
	}
	return raw, nil
}

type Recording struct {
	Body io.ReadCloser
	MIME string
	Size int64
}
type bufferedBody struct {
	io.Reader
	io.Closer
}

func (c *Client) OpenRecording(ctx context.Context, id string) (*Recording, error) {
	if !validID(id) {
		return nil, errors.New("invalid Bland call ID")
	}
	res, e := c.request(ctx, "/recordings/"+id, nil, true)
	if e != nil {
		return nil, e
	}
	b := bufio.NewReader(res.Body)
	prefix, e := b.Peek(512)
	if e != nil && !errors.Is(e, io.EOF) {
		_ = res.Body.Close()
		return nil, fmt.Errorf("read Bland recording header: %w", e)
	}
	trimmed := bytes.TrimSpace(prefix)
	if bytes.Contains(prefix, []byte("CALL_RECORDING_NOT_FOUND")) {
		_ = res.Body.Close()
		return nil, ErrNotFound
	}
	mime := strings.ToLower(strings.Split(res.Header.Get("Content-Type"), ";")[0])
	// Validate audio signatures even when an error response claims an audio MIME.
	mp3 := bytes.HasPrefix(prefix, []byte("ID3")) || len(prefix) >= 2 && prefix[0] == 0xff && (prefix[1]&0xe0) == 0xe0
	wav := len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WAVE"
	if len(trimmed) == 0 || (!mp3 && !wav) || strings.Contains(mime, "json") || strings.Contains(mime, "html") {
		_ = res.Body.Close()
		return nil, ErrInvalidPayload
	}
	if wav {
		mime = "audio/wav"
	} else {
		mime = "audio/mpeg"
	}
	return &Recording{Body: bufferedBody{Reader: b, Closer: res.Body}, MIME: mime, Size: res.ContentLength}, nil
}
