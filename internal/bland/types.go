// Package bland retrieves existing call artifacts through Bland's read APIs.
package bland

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const SourceType = "bland"
const RawFormat = "bland_call_json"
const DefaultBaseURL = "https://api.bland.ai/v1"

// Call is one Bland call response: call details or a postcall payload after
// decodeDoc checked it, or a listing row, of which only the IDs are checked.
type Call struct {
	ID                string         `json:"call_id"`
	CID               string         `json:"c_id"`
	CreatedAt         string         `json:"created_at"`
	StartedAt         string         `json:"started_at"`
	EndAt             string         `json:"end_at"`
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

	created, start time.Time
	length         *float64
	// meta holds the call fields the response carries, for merging.
	meta map[string]jsontext.Value
	// The transcript renditions the response carries, decoded.
	transcripts, concatenated, corrected, postTransfer *rendition
	translation                                        jsontext.Value
	// hasTranscript means a transcript field was present, with speech or not.
	hasTranscript bool
}

var callMetadataKeys = []string{"call_id", "c_id", "created_at", "started_at", "end_at", "status", "queue_status", "completed", "from", "to", "inbound", "answered_by", "record", "recording_url", "corrected_duration", "call_length", "summary"}

// malformed names the field of a Bland response that failed its decode.
func malformed(field string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrInvalidPayload, field)
	}
	return fmt.Errorf("%w: %s: %w", ErrInvalidPayload, field, err)
}

// decodeDoc checks a Bland call response once, where it enters: every field
// has its type, times parse, numbers are numeric, call_id and c_id agree, and
// each transcript rendition decodes. Code past it trusts the result.
func decodeDoc(raw jsontext.Value) (*Call, error) {
	var c Call
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, malformed("call", err)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, malformed("call", err)
	}
	if c.ID == "" {
		c.ID = c.CID
	}
	if c.CID != "" && c.CID != c.ID {
		return nil, malformed("c_id", errors.New("differs from call_id"))
	}
	for _, at := range []struct {
		field string
		value string
		to    *time.Time
	}{{"created_at", c.CreatedAt, &c.created}, {"started_at", c.StartedAt, &c.start}, {"end_at", c.EndAt, nil}} {
		if at.value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, at.value)
		if err != nil {
			return nil, malformed(at.field, err)
		}
		if at.to != nil {
			*at.to = t
		}
	}
	if c.CorrectedDuration != "" {
		d, err := strconv.ParseFloat(strings.TrimSpace(c.CorrectedDuration), 64)
		if err != nil || !finiteNonnegative(d) {
			return nil, malformed("corrected_duration", err)
		}
		c.length = &d
	}
	if c.CallLength != nil {
		if !finiteNonnegative(*c.CallLength) {
			return nil, malformed("call_length", nil)
		}
		if c.length == nil {
			seconds := *c.CallLength * 60
			if !finiteNonnegative(seconds) {
				return nil, malformed("call_length", errors.New("out of range"))
			}
			c.length = &seconds
		}
	}
	c.meta = map[string]jsontext.Value{}
	for _, key := range callMetadataKeys {
		if value := fields[key]; value != nil {
			if err := value.Compact(); err != nil {
				return nil, malformed(key, err)
			}
			if s := string(value); s != "null" && s != `""` {
				c.meta[key] = value
			}
		}
	}
	var offset *float64
	if raw := fields["transfer_offset_seconds"]; raw != nil && string(raw) != "null" {
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil || !finiteNonnegative(value) {
			return nil, malformed("transfer_offset_seconds", err)
		}
		offset = &value
	}
	for _, r := range []struct {
		field string
		kind  renditionKind
		to    **rendition
	}{
		{"transcripts", ordinary, &c.transcripts},
		{"concatenated_transcript", ordinary, &c.concatenated},
		{"corrected_transcript", enhanced, &c.corrected},
		{"post_transfer_transcript", transferred, &c.postTransfer},
	} {
		value := fields[r.field]
		if value == nil || string(value) == "null" {
			continue
		}
		c.hasTranscript = true
		var at *float64
		if r.kind == transferred {
			at = offset
		}
		decoded, err := decodeRendition(value, r.kind, at)
		if err != nil {
			return nil, malformed(r.field, err)
		}
		*r.to = decoded
	}
	if value := fields["live_translation_transcript"]; value != nil && string(value) != "null" {
		c.translation = value
	}
	c.Raw = append(jsontext.Value(nil), raw...)
	return &c, nil
}

// decodeCall decodes call details: the call must be named, and match expected
// when set; dated means the response must carry a creation or start time.
func decodeCall(raw jsontext.Value, expected string, dated bool) (*Call, error) {
	c, err := decodeDoc(raw)
	if err != nil {
		return nil, err
	}
	if !validID(c.ID) || (expected != "" && c.ID != expected) {
		return nil, malformed("call_id", errors.New("doesn't name the call"))
	}
	if dated && c.started().IsZero() {
		return nil, malformed("created_at", errors.New("missing"))
	}
	return c, nil
}

// decodeListed checks only a listing row's IDs, so one bad field in a row
// can't stop every run; the row is decoded whole only if it stands in for
// details Bland refused.
func decodeListed(raw jsontext.Value) (*Call, error) {
	var c struct {
		ID  string `json:"call_id"`
		CID string `json:"c_id"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, malformed("call_id", err)
	}
	id := cmp.Or(c.ID, c.CID)
	if !validID(id) || c.CID != "" && c.CID != id {
		return nil, malformed("call_id", errors.New("doesn't name the call"))
	}
	return &Call{ID: id, Raw: append(jsontext.Value(nil), raw...)}, nil
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
	if !c.start.IsZero() {
		return c.start
	}
	return c.created
}

func (c *Call) duration() *float64 { return c.length }
