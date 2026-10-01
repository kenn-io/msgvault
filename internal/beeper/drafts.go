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

// DraftWriteError reports a failed draft write with its HTTP status, or 0
// when the request may have reached Beeper without a response.
type DraftWriteError struct {
	Status int
	Err    error
}

func (e *DraftWriteError) Error() string {
	return fmt.Sprintf("beeper draft update failed (status %d)", e.Status)
}

func (e *DraftWriteError) Unwrap() error { return e.Err }

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
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("wait for beeper rate limit: %w", err)
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v1/chats/"+url.PathEscape(chatID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &DraftWriteError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDraftResponseBytes))
	if err != nil {
		return nil, &DraftWriteError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &DraftWriteError{Status: resp.StatusCode, Err: errPermanentResponse}
	}
	var chat Chat
	if err := json.Unmarshal(data, &chat); err != nil {
		return nil, &DraftWriteError{Err: err}
	}
	return &chat, nil
}
