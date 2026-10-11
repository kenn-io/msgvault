// Package twilio archives existing calls, recordings, and retained transcripts
// without enabling provider features.
package twilio

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"time"
)

const SourceType = "twilio"
const RawFormat = "twilio_call_json"

type Options struct {
	AccountSID, APIKeySID, APIKeySecret, AuthToken string
	IntelligenceServiceSID                         string
	Region                                         string
	HTTPClient                                     *http.Client
	Endpoints                                      map[string]string
}

type Call struct {
	Raw           jsontext.Value `json:"provider_raw"`
	SID           string         `json:"sid"`
	AccountSID    string         `json:"account_sid"`
	ParentCallSID string         `json:"parent_call_sid,omitempty"`
	From          string         `json:"from"`
	To            string         `json:"to"`
	Direction     string         `json:"direction"`
	Status        string         `json:"status"`
	StartTime     string         `json:"start_time"`
	EndTime       string         `json:"end_time"`
	Duration      string         `json:"duration"`
	DateCreated   string         `json:"date_created,omitempty"`
	DateUpdated   string         `json:"date_updated,omitempty"`
}

type Recording struct {
	Raw         jsontext.Value `json:"provider_raw,omitempty"`
	SID         string         `json:"sid"`
	AccountSID  string         `json:"account_sid"`
	CallSID     string         `json:"call_sid"`
	Status      string         `json:"status"`
	DateCreated string         `json:"date_created"`
	DateUpdated string         `json:"date_updated"`
	StartTime   string         `json:"start_time"`
	Duration    string         `json:"duration"`
	Source      string         `json:"source"`
	Channels    int            `json:"channels"`
	// EncryptionDetails is set when the account encrypts recordings, whose
	// audio msgvault can't decrypt.
	EncryptionDetails map[string]any `json:"encryption_details,omitempty"`
}

// Encrypted reports whether Twilio stores the recording encrypted.
func (r Recording) Encrypted() bool { return len(r.EncryptionDetails) > 0 }

type Segment struct {
	Speaker       string   `json:"speaker,omitempty"`
	Text          string   `json:"text"`
	Scope         string   `json:"scope,omitempty"`
	OffsetSeconds *float64 `json:"offset_seconds,omitempty"`
}

type Transcript struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	Status   string `json:"status"`
	// DateCreated tells a retranscription from the transcript it replaced.
	DateCreated string         `json:"date_created,omitempty"`
	Complete    bool           `json:"complete"`
	Usable      bool           `json:"usable"`
	Segments    []Segment      `json:"segments,omitempty"`
	Raw         jsontext.Value `json:"raw,omitempty"`
}

type Evidence struct {
	Transcripts []Transcript `json:"transcripts,omitempty"`
	Diagnostics []string     `json:"diagnostics,omitempty"`
}

// ParseTime accepts the timestamp formats returned by Voice and Intelligence.
func ParseTime(value string) time.Time {
	for _, format := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, "2006-01-02T15:04:05Z0700"} {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// UnmarshalJSON keeps the original provider object alongside normalized fields.
func (c *Call) UnmarshalJSON(data []byte) error {
	type plain Call
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if string(value.Raw) == "null" {
		// An identity-only archived call has no upstream Call response.
		value.Raw = nil
	} else if len(value.Raw) == 0 {
		value.Raw = append(jsontext.Value(nil), data...)
	}
	*c = Call(value)
	return nil
}

func (r *Recording) UnmarshalJSON(data []byte) error {
	type plain Recording
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if len(value.Raw) == 0 {
		value.Raw = append(jsontext.Value(nil), data...)
	}
	*r = Recording(value)
	return nil
}
