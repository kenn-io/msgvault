// Package bland retrieves existing call artifacts through Bland's read APIs.
package bland

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const SourceType = "bland"
const RawFormat = "bland_call_json"
const DefaultBaseURL = "https://api.bland.ai/v1"

type Call struct {
	ID                string         `json:"call_id"`
	CID               string         `json:"c_id"`
	CreatedAt         string         `json:"created_at"`
	StartedAt         string         `json:"started_at"`
	UpdatedAt         string         `json:"updated_at"`
	Completed         bool           `json:"completed"`
	Status            string         `json:"status"`
	QueueStatus       string         `json:"queue_status"`
	From              string         `json:"from"`
	To                string         `json:"to"`
	Inbound           bool           `json:"inbound"`
	AnsweredBy        string         `json:"answered_by"`
	Record            bool           `json:"record"`
	RecordingURL      string         `json:"recording_url"`
	CorrectedDuration string         `json:"corrected_duration"`
	CallLength        *float64       `json:"call_length"`
	Summary           string         `json:"summary"`
	Raw               jsontext.Value `json:"-"`
}

func parseCall(raw []byte, expected string) (*Call, error) {
	var c Call
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalidPayload
	}
	if c.ID == "" {
		c.ID = c.CID
	}
	if !validID(c.ID) || (expected != "" && c.ID != expected) {
		return nil, fmt.Errorf("%w: call payload identity mismatch", ErrInvalidPayload)
	}
	c.Raw = append(jsontext.Value(nil), raw...)
	return &c, nil
}
func validID(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
func (c *Call) started() time.Time {
	for _, s := range []string{c.StartedAt, c.CreatedAt} {
		if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
			return t
		}
	}
	return time.Time{}
}
func (c *Call) duration() *float64 {
	d, err := strconv.ParseFloat(strings.TrimSpace(c.CorrectedDuration), 64)
	if err != nil || !finiteNonnegative(d) {
		if c.CallLength == nil {
			return nil
		}
		d = *c.CallLength * 60
	}
	if math.IsNaN(d) || math.IsInf(d, 0) || d < 0 || d > float64(math.MaxInt64)/float64(time.Second) {
		return nil
	}
	return &d
}
func (c *Call) ended() bool {
	if c.Completed {
		return true
	}
	switch c.Status {
	case "completed", "failed", "busy", "no-answer", "canceled", "cancelled":
		return true
	case "started", "in-progress", "queued", "new", "allocated":
		return false
	}
	switch c.QueueStatus {
	case "complete", "completed", "complete_error", "pre_queue_error", "queue_error", "call_error", "failed", "busy", "no-answer", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

// enrichedCall fills omitted detail fields with previously sent provider data.
// Raw remains the exact detail response; retained payloads have their own evidence.
func enrichedCall(c *Call, hook jsontext.Value) (*Call, error) {
	if len(hook) == 0 {
		return c, nil
	}
	var w struct {
		Data struct {
			Payload jsontext.Value `json:"payload"`
		} `json:"data"`
	}
	if json.Unmarshal(hook, &w) != nil {
		return nil, ErrInvalidPayload
	}
	var detail, payload map[string]jsontext.Value
	if json.Unmarshal(c.Raw, &detail) != nil || json.Unmarshal(w.Data.Payload, &payload) != nil || payload == nil {
		return nil, ErrInvalidPayload
	}
	for _, key := range []string{"call_id", "c_id"} {
		if raw, ok := payload[key]; ok {
			var id string
			if json.Unmarshal(raw, &id) != nil || id != c.ID {
				return nil, ErrInvalidPayload
			}
		}
	}
	for _, key := range []string{"created_at", "started_at", "status", "queue_status", "completed", "from", "to", "inbound", "answered_by", "record", "recording_url", "corrected_duration", "call_length", "summary"} {
		raw, ok := detail[key]
		if !ok || string(raw) == "null" || string(raw) == `""` {
			if v, ok := payload[key]; ok {
				detail[key] = v
			}
		}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	enriched, err := parseCall(b, c.ID)
	if err != nil {
		return nil, err
	}
	enriched.Raw = c.Raw
	return enriched, nil
}
