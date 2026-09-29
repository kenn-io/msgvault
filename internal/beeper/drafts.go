package beeper

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DraftWriteState tells callers whether a failed PATCH may have reached the
// provider. The provider has no conditional clear token, so uncertain writes
// must stay pending in the local store.
type DraftWriteState string

const (
	DraftWriteRejected  DraftWriteState = "rejected"
	DraftWriteUncertain DraftWriteState = "uncertain"
)

// DraftWriteError exposes only a stable code and write state. It never copies
// provider response bodies or submitted draft text into an error.
type DraftWriteError struct {
	Code   string
	Status int
	State  DraftWriteState
	Err    error
}

func (e *DraftWriteError) Error() string {
	if e == nil || e.Code == "" {
		return "beeper draft update failed"
	}
	return e.Code
}

func (e *DraftWriteError) Unwrap() error { return e.Err }

const (
	DraftWriteCodeRejected  = "provider_rejected"
	DraftWriteCodeUnknown   = "remote_unknown"
	DraftWriteCodeRateLimit = "rate_limit"
	DraftWriteCodeToken     = "token_unavailable" // #nosec G101 -- this is an error code, not a credential.
)

// DraftObservation is the part of a Chat draft that msgvault can safely
// compare. Unknown fields and attachments make the observation occupied.
type DraftObservation struct {
	Present            bool
	ExplicitNull       bool
	Text               string
	AttachmentsPresent bool
	Unknown            bool
}

func (d DraftObservation) Empty() bool {
	return d.Present && d.ExplicitNull
}

// InspectDraft decodes the raw draft field while preserving absent versus
// explicit null. An object with fields outside text is unsafe to replace.
func (c Chat) InspectDraft() (DraftObservation, error) {
	if len(c.Draft) == 0 {
		return DraftObservation{}, nil
	}
	obs := DraftObservation{Present: true}
	if string(c.Draft) == "null" {
		obs.ExplicitNull = true
		return obs, nil
	}
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(c.Draft, &raw); err != nil {
		return DraftObservation{}, fmt.Errorf("decode Beeper draft: %w", err)
	}
	if raw == nil {
		return DraftObservation{}, errors.New("beeper draft is not an object")
	}
	for key, value := range raw {
		switch key {
		case "text":
			if err := json.Unmarshal(value, &obs.Text); err != nil {
				return DraftObservation{}, fmt.Errorf("decode Beeper draft text: %w", err)
			}
		case "attachments":
			obs.AttachmentsPresent = true
			obs.Unknown = true
		default:
			obs.Unknown = true
		}
	}
	return obs, nil
}

// UpdateDraft performs one native Beeper PATCH. A nil text sends JSON null
// and clears the composer slot. A non-empty value sets text and leaves
// attachments out of the request.
func (c *Client) UpdateDraft(ctx context.Context, chatID string, text *string) (*Chat, error) {
	if strings.TrimSpace(chatID) == "" {
		return nil, &DraftWriteError{Code: "invalid_chat", State: DraftWriteRejected, Err: errors.New("chat ID is blank")}
	}
	if text != nil && *text == "" {
		return nil, &DraftWriteError{Code: "invalid_body", State: DraftWriteRejected, Err: errors.New("draft body is empty")}
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, &DraftWriteError{Code: DraftWriteCodeRateLimit, State: DraftWriteRejected, Err: err}
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, &DraftWriteError{Code: DraftWriteCodeToken, State: DraftWriteRejected, Err: err}
	}
	body := []byte(`{"draft":null}`)
	if text != nil {
		encoded, marshalErr := json.Marshal(struct {
			Draft struct {
				Text string `json:"text"`
			} `json:"draft"`
		}{Draft: struct {
			Text string `json:"text"`
		}{Text: *text}})
		if marshalErr != nil {
			return nil, &DraftWriteError{Code: "invalid_body", State: DraftWriteRejected, Err: marshalErr}
		}
		body = encoded
	}
	path := "/v1/chats/" + url.PathEscape(chatID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, &DraftWriteError{Code: "invalid_request", State: DraftWriteRejected, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	httpClient := *c.http
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &DraftWriteError{Code: DraftWriteCodeUnknown, State: DraftWriteUncertain, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	reader := io.Reader(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reader = io.LimitReader(resp.Body, maxErrorBodyBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, &DraftWriteError{Code: DraftWriteCodeUnknown, Status: resp.StatusCode, State: DraftWriteUncertain, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		state := DraftWriteUncertain
		code := DraftWriteCodeUnknown
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			state = DraftWriteRejected
			code = DraftWriteCodeRejected
		}
		return nil, &DraftWriteError{Code: code, Status: resp.StatusCode, State: state, Err: fmt.Errorf("beeper PATCH returned status %d", resp.StatusCode)}
	}
	var chat Chat
	if err := json.Unmarshal(data, &chat); err != nil || strings.TrimSpace(chat.ID) == "" || strings.TrimSpace(chat.AccountID) == "" {
		if err == nil {
			err = errors.New("response omitted chat identity")
		}
		return nil, &DraftWriteError{Code: DraftWriteCodeUnknown, Status: resp.StatusCode, State: DraftWriteUncertain, Err: err}
	}
	return &chat, nil
}
