package zoomcalls

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
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client reads only the Zoom API. Tokens are supplied by a literal argv command;
// app enrollment and refresh remain under the user's control.
type Client struct {
	base   string
	http   *http.Client
	token  func(context.Context) (string, error)
	access string
}

func NewClient(command []string) *Client {
	argv := append([]string(nil), command...)
	return &Client{base: "https://api.zoom.us/v2", http: &http.Client{Timeout: 30 * time.Second}, token: func(ctx context.Context) (string, error) { return commandToken(ctx, argv) }}
}

type tokenOutput struct{ buffer bytes.Buffer }

func (b *tokenOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 64<<10 {
		return 0, errors.New("zoom token output exceeds limit")
	}
	n, err := b.buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffer zoom token output: %w", err)
	}
	return n, nil
}
func commandToken(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("zoom requires --token-command as a JSON argv array")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // Explicit user-configured literal argv; no shell.
	cmd.WaitDelay = time.Second
	var output tokenOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return "", errors.New("zoom token command failed (output suppressed)")
	}
	token := strings.TrimSpace(output.buffer.String())
	if token == "" || strings.ContainsAny(token, " \t\r\n\x00") {
		return "", errors.New("zoom token command must output one bearer token")
	}
	return token, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if c.access == "" {
		token, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		c.access = token
	}
	client := *c.http
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("zoom redirects are disabled") }
	waited := time.Duration(0)
	for attempt := range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, errors.New("invalid Zoom request")
		}
		req.Header.Set("Authorization", "Bearer "+c.access)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("zoom request canceled: %w", ctx.Err())
			}
			return nil, errors.New("zoom request failed")
		}
		status := resp.StatusCode
		if status == http.StatusOK {
			body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
			_ = resp.Body.Close()
			if err != nil {
				return nil, errors.New("read Zoom response")
			}
			if len(body) > 1<<20 {
				return nil, errors.New("zoom response exceeds 1 MiB")
			}
			return body, nil
		}
		retryAfter := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		if attempt == 2 || (status != 429 && status < 500) {
			return nil, fmt.Errorf("zoom returned HTTP %d; check app scopes, license and source access", status)
		}
		delay := time.Second
		if retryAfter != "" {
			if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 && seconds <= 30 {
				delay = time.Duration(seconds) * time.Second
			} else if at, err := http.ParseTime(retryAfter); err == nil {
				delay = max(time.Duration(0), time.Until(at))
			} else {
				return nil, errors.New("zoom retry delay exceeds budget")
			}
		}
		if delay > 30*time.Second-waited {
			return nil, errors.New("zoom retry delay exceeds budget")
		}
		waited += delay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("zoom retry limit")
}

func (c *Client) pages(ctx context.Context, path, key string, q url.Values, limit int) ([]jsontext.Value, error) {
	var pages []jsontext.Value
	seen := map[string]bool{}
	total, records := 0, 0
	expected := -1
	for range 100 {
		body, err := c.get(ctx, path, q)
		if err != nil {
			return nil, err
		}
		total += len(body)
		if total > 32<<20 {
			return nil, errors.New("zoom staging exceeds 32 MiB")
		}
		var page map[string]jsontext.Value
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, errors.New("invalid Zoom response JSON")
		}
		var items []jsontext.Value
		if err := json.Unmarshal(page[key], &items); err != nil || items == nil {
			return nil, errors.New("missing Zoom result array")
		}
		if value, ok := page["total_records"]; ok {
			var n int
			if err := json.Unmarshal(value, &n); err != nil || n < 0 || (expected >= 0 && expected != n) {
				return nil, errors.New("inconsistent Zoom total records")
			}
			expected = n
		}
		records += len(items)
		if records > limit {
			return nil, errors.New("zoom record limit exceeded; narrow the date window")
		}
		pages = append(pages, jsontext.Value(body))
		var next string
		if value, ok := page["next_page_token"]; ok {
			if err := json.Unmarshal(value, &next); err != nil {
				return nil, errors.New("invalid Zoom page token")
			}
		}
		if next == "" {
			if expected >= 0 && expected != records {
				return nil, errors.New("incomplete Zoom record total")
			}
			return pages, nil
		}
		if seen[next] {
			return nil, errors.New("repeated Zoom page token")
		}
		seen[next] = true
		q.Set("next_page_token", next)
	}
	return nil, errors.New("zoom page limit exceeded")
}

// Meeting retrieves every roster page for an exact past instance UUID.
func (c *Client) Meeting(ctx context.Context, uuid string) ([]byte, error) {
	if !isExactInstanceUUID(uuid) {
		// Zoom resolves a numeric meeting ID to its latest instance; it cannot
		// identify the exact historical occurrence required for archival identity.
		if isNumericZoomMeetingID(uuid) {
			return nil, errors.New("numeric Zoom meeting ID is not an exact instance UUID")
		}
		return nil, errors.New("zoom instance UUID required")
	}
	encoded := url.PathEscape(uuid)
	if strings.HasPrefix(uuid, "/") || strings.Contains(uuid, "//") {
		encoded = url.PathEscape(encoded)
	}
	pages, err := c.pages(ctx, "/past_meetings/"+encoded+"/participants", "participants", url.Values{"page_size": {"300"}}, 10000)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(pages)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxRosterBytes {
		return nil, errors.New("zoom roster exceeds archive raw limit")
	}
	return raw, nil
}
