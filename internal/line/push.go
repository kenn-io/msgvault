package line

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// PushTextRequest is one plain text message. QuoteToken belongs to its parent.
// Durable callers must keep these values and their retry key unchanged.
type PushTextRequest struct {
	To         string
	Text       string
	QuoteToken string
}

// PushReceipt distinguishes acceptance evidence from a usable message ID.
// Acceptance does not prove delivery. RetryAfter is a provider retry deadline.
type PushReceipt struct {
	MessageID  string
	QuoteToken string
	RequestID  string
	StatusCode int
	Accepted   bool
	RetryAfter time.Time
}

// ErrInvalidReplyText identifies a local outbound text validation failure.
var ErrInvalidReplyText = errors.New("line: invalid reply text")

// ValidateReplyText applies LINE's 5,000 UTF-16 code-unit text limit.
func ValidateReplyText(text string) error {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) || strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: must be nonblank UTF-8 without NUL", ErrInvalidReplyText)
	}
	units := 0
	for _, r := range text {
		units++
		if r > 0xffff {
			units++
		}
		if units > 5000 {
			return fmt.Errorf("%w: exceeds 5000 UTF-16 code units", ErrInvalidReplyText)
		}
	}
	return nil
}

func validPushRecipient(id string) bool {
	if len(id) != 33 || !strings.ContainsRune("UCR", rune(id[0])) {
		return false
	}
	for _, b := range []byte(id[1:]) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
			return false
		}
	}
	return true
}

// PushText sends one frozen request with a caller-owned retry key. Only a 401
// token refresh is replayed here, with identical body and key. Other retries
// belong to the durable sender, within LINE's 24-hour deduplication window.
func (c *Client) PushText(ctx context.Context, retryKey string, input PushTextRequest) (PushReceipt, error) {
	var receipt PushReceipt
	if len(retryKey) != 36 || uuid.Validate(retryKey) != nil {
		return receipt, errors.New("line: invalid retry UUID")
	}
	if !validPushRecipient(input.To) {
		return receipt, errors.New("line: invalid push recipient")
	}
	if err := ValidateReplyText(input.Text); err != nil {
		return receipt, err
	}
	type textMessage struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		QuoteToken string `json:"quoteToken,omitempty"`
	}
	body, err := json.Marshal(struct {
		To       string        `json:"to"`
		Messages []textMessage `json:"messages"`
	}{To: input.To, Messages: []textMessage{{Type: "text", Text: input.Text, QuoteToken: input.QuoteToken}}})
	if err != nil {
		return receipt, errors.New("line: cannot encode push request")
	}
	response, err := c.authorizedWithHTTPClient(ctx, http.MethodPost, c.apiURL, "/v2/bot/message/push", body, c.http, retryKey)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = response.Body.Close() }()
	receipt.StatusCode = response.StatusCode
	receipt.RequestID = response.Header.Get("X-Line-Request-Id")
	receipt.Accepted = response.StatusCode >= 200 && response.StatusCode < 300
	if response.StatusCode == http.StatusConflict {
		acceptedID := response.Header.Get("X-Line-Accepted-Request-Id")
		if strings.TrimSpace(acceptedID) != "" {
			receipt.Accepted = true
			receipt.RequestID = acceptedID
		}
	}
	if !receipt.Accepted {
		apiErr := c.statusError(response)
		receipt.RetryAfter = apiErr.RetryAfter
		return receipt, apiErr
	}
	var result struct {
		SentMessages []struct {
			ID         string `json:"id"`
			QuoteToken string `json:"quoteToken"`
		} `json:"sentMessages"`
	}
	if err := decodeJSON(ctx, response.Body, &result); err != nil {
		return receipt, err
	}
	if len(result.SentMessages) != 1 || strings.TrimSpace(result.SentMessages[0].ID) == "" {
		return receipt, errors.New("line: accepted push has no usable single-message receipt")
	}
	receipt.MessageID = result.SentMessages[0].ID
	receipt.QuoteToken = result.SentMessages[0].QuoteToken
	return receipt, nil
}
