// Package twilio reads existing call evidence without enabling provider features.
package twilio

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"
)

const SourceType = "twilio"
const RawFormat = "twilio_call_json"

type Options struct {
	AccountSID, APIKeySID, APIKeySecret, AuthToken string
	IntelligenceServiceSID                         string
	RelayDiscovery                                 bool
	Region                                         string
	HTTPClient                                     *http.Client
	Endpoints                                      map[string]string
	RecordingKeys                                  map[string]string
	ExternalMedia                                  map[string]string
	ExternalMediaHosts                             []string
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
	Raw               jsontext.Value     `json:"provider_raw,omitempty"`
	SID               string             `json:"sid"`
	AccountSID        string             `json:"account_sid"`
	CallSID           string             `json:"call_sid"`
	ConferenceSID     string             `json:"conference_sid,omitempty"`
	Status            string             `json:"status"`
	DateCreated       string             `json:"date_created"`
	DateUpdated       string             `json:"date_updated"`
	StartTime         string             `json:"start_time"`
	Duration          string             `json:"duration"`
	Source            string             `json:"source"`
	Channels          int                `json:"channels"`
	MediaURL          string             `json:"media_url,omitempty"`
	EncryptionDetails *EncryptionDetails `json:"encryption_details,omitempty"`
}

type EncryptionDetails struct {
	Type         string `json:"type"`
	PublicKeySID string `json:"public_key_sid"`
	EncryptedCEK string `json:"encrypted_cek"`
	IV           string `json:"iv"`
}

type Segment struct {
	Speaker       string     `json:"speaker,omitempty"`
	Text          string     `json:"text"`
	Scope         string     `json:"scope,omitempty"`
	OffsetSeconds *float64   `json:"offset_seconds,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
}

type Transcript struct {
	Kind           string          `json:"kind"`
	ID             string          `json:"id"`
	SourceID       string          `json:"source_id"`
	Status         string          `json:"status"`
	Complete       bool            `json:"complete"`
	Usable         bool            `json:"usable"`
	Segments       []Segment       `json:"segments,omitempty"`
	Raw            jsontext.Value  `json:"raw,omitempty"`
	Communications []Communication `json:"communications,omitempty"`
}

type Evidence struct {
	RelayEvents   []jsontext.Value `json:"relay_events,omitempty"`
	Conversations []jsontext.Value `json:"conversations,omitempty"`
	Transcripts   []Transcript     `json:"transcripts,omitempty"`
	Diagnostics   []string         `json:"diagnostics,omitempty"`
}

type Communication struct {
	ID             string `json:"id"`
	AccountID      string `json:"accountId"`
	ConversationID string `json:"conversationId"`
	ChannelID      string `json:"channelId"`
	ResourceID     string `json:"resourceId"`
	Content        struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Author struct {
		Address       string `json:"address"`
		Channel       string `json:"channel"`
		ParticipantID string `json:"participantId"`
	} `json:"author"`
	OccurredAt string `json:"occurredAt"`
}

// ParseTime accepts the timestamp formats returned by Voice and Orchestrator.
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
		// An identity-only archived call has no upstream Call response. Keep
		// that explicit null distinct from a freshly decoded provider object.
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

// UnmarshalJSON accepts the documented REST and callback encryption field names.
// Contradictory aliases are rejected rather than selecting an arbitrary key.
func (e *EncryptionDetails) UnmarshalJSON(data []byte) error {
	type plain EncryptionDetails
	var value struct {
		plain

		RESTKey string `json:"encryption_public_key_sid"`
		RESTCEK string `json:"encryption_cek"`
		RESTIV  string `json:"encryption_iv"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	merge := func(a, b string) (string, error) {
		if a != "" && b != "" && a != b {
			return "", errors.New("twilio: contradictory recording encryption aliases")
		}
		if a != "" {
			return a, nil
		}
		return b, nil
	}
	key, err := merge(value.PublicKeySID, value.RESTKey)
	if err != nil {
		return err
	}
	cek, err := merge(value.EncryptedCEK, value.RESTCEK)
	if err != nil {
		return err
	}
	iv, err := merge(value.IV, value.RESTIV)
	if err != nil {
		return err
	}
	kind := value.Type
	if kind == "" && (value.RESTKey != "" || value.RESTCEK != "" || value.RESTIV != "") {
		if value.RESTKey == "" || value.RESTCEK == "" || value.RESTIV == "" {
			return errors.New("twilio: incomplete REST recording encryption envelope")
		}
		kind = "rsa-aes"
	}
	*e = EncryptionDetails{Type: kind, PublicKeySID: key, EncryptedCEK: cek, IV: iv}
	return nil
}
