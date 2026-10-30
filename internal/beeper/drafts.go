package beeper

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
	"time"
)

// maxDraftResponseBytes bounds a PATCH response, which carries the whole chat.
const maxDraftResponseBytes = 16 << 20

// ErrDraftUnsupported reports a chat response without a readable draft field.
var ErrDraftUnsupported = errors.New("beeper chat does not report a readable draft")

// DraftState is what a chat's composer holds. Other reports a draft with
// fields beyond text, such as attachments, which msgvault never overwrites.
type DraftState struct {
	Empty bool
	Text  string
	Other bool
}

// DraftState reads the composer draft from a chat response.
func (c Chat) DraftState() (DraftState, error) {
	if len(c.Draft) == 0 {
		return DraftState{}, ErrDraftUnsupported
	}
	if string(c.Draft) == "null" {
		return DraftState{Empty: true}, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(c.Draft, &fields); err != nil || fields == nil {
		return DraftState{}, ErrDraftUnsupported
	}
	var state DraftState
	for key, value := range fields {
		if key != "text" {
			state.Other = true
			continue
		}
		if err := json.Unmarshal(value, &state.Text); err != nil {
			return DraftState{}, ErrDraftUnsupported
		}
	}
	return state, nil
}

// WriteError reports a failed write. Status is the HTTP status Beeper
// returned, 400 for a 200 upload response that carries an error
// payload, or 0 when the request may have reached Beeper without a
// complete success response.
type WriteError struct {
	Status int
	Err    error
}

func (e *WriteError) Error() string {
	return fmt.Sprintf("beeper write failed (status %d)", e.Status)
}

func (e *WriteError) Unwrap() error { return e.Err }

// SetDraft replaces a chat's composer draft with text, or clears it when text
// is nil. Beeper rejects setting text over an occupied draft, so callers clear
// first. The write is sent once; response bodies never reach the error.
func (c *Client) SetDraft(ctx context.Context, chatID string, text *string) (*Chat, error) {
	body := []byte(`{"draft":null}`)
	if text != nil {
		encoded, err := json.Marshal(map[string]map[string]string{"draft": {"text": *text}})
		if err != nil {
			return nil, err
		}
		body = encoded
	}
	data, err := c.writeOnce(ctx, http.MethodPatch, "/v1/chats/"+url.PathEscape(chatID), "application/json", bytes.NewReader(body), c.http.Timeout, false)
	if err != nil {
		return nil, err
	}
	var chat Chat
	if err := json.Unmarshal(data, &chat); err != nil {
		return nil, &WriteError{Err: err}
	}
	return &chat, nil
}

func (c *Client) writeOnce(ctx context.Context, method, path, contentType string, body io.Reader, timeout time.Duration, refuseRedirects bool) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("wait for beeper rate limit: %w", err)
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	writeClient := *c.http
	writeClient.Timeout = timeout
	if refuseRedirects {
		writeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := writeClient.Do(req)
	if err != nil {
		return nil, &WriteError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDraftResponseBytes))
	if err != nil {
		failure := &WriteError{Err: err}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			failure.Status = resp.StatusCode
		}
		return nil, failure
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &WriteError{Status: resp.StatusCode, Err: errPermanentResponse}
	}
	return data, nil
}
