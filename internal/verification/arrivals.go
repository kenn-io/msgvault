package verification

import (
	"errors"
	"time"
)

// ListOptions bounds polling to a recent window; Reveal is owner configuration,
// never a substitute for a delegated source's verification.read permission.
type ListOptions struct {
	Since          time.Time
	Sender         string
	Kind           string
	IncludeExpired bool
	Limit          int
	Reveal         bool
}

func (o ListOptions) Normalize(now time.Time) (ListOptions, error) {
	if o.Since.IsZero() {
		o.Since = now.Add(-time.Hour)
	}
	if o.Since.Before(now.Add(-24 * time.Hour)) {
		o.Since = now.Add(-24 * time.Hour)
	}
	if o.Since.After(now) {
		return o, errors.New("since must not be in the future")
	}
	if o.Limit == 0 {
		o.Limit = 20
	}
	if o.Limit < 1 || o.Limit > 100 {
		return o, errors.New("limit must be between 1 and 100")
	}
	switch o.Kind {
	case "", "otp_code", "magic_link", "password_reset", "double_opt_in", "account_confirmation":
	default:
		return o, errors.New("unsupported verification kind")
	}
	if len(o.Sender) > 320 {
		return o, errors.New("sender is too long")
	}
	return o, nil
}

type RevealedValue struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Host  string `json:"host,omitempty"`
}

type Arrival struct {
	MessageID       int64           `json:"message_id"`
	SourceID        int64           `json:"source_id"`
	SourceMessageID string          `json:"source_message_id"`
	ConversationID  int64           `json:"conversation_id"`
	Verification    *Metadata       `json:"verification"`
	Values          []RevealedValue `json:"values,omitempty"`
}

type ArrivalPage struct {
	Data          []Arrival `json:"data"`
	Since         time.Time `json:"since"`
	Before        time.Time `json:"before"`
	Scanned       int       `json:"scanned"`
	ScanTruncated bool      `json:"scan_truncated"`
}
