package bland

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

// Evidence retains each rendition independently, so temporary omissions cannot
// destroy the corrected or transferred conversation already archived.
type Evidence struct {
	Version        int                    `json:"version"`
	Call           jsontext.Value         `json:"call"`
	EffectiveCall  jsontext.Value         `json:"effective_call,omitempty"`
	PostCall       jsontext.Value         `json:"postcall,omitempty"`
	Original       jsontext.Value         `json:"original,omitempty"`
	Corrected      jsontext.Value         `json:"corrected,omitempty"`
	PostTransfer   jsontext.Value         `json:"post_transfer,omitempty"`
	TransferOffset *float64               `json:"transfer_offset_seconds,omitempty"`
	Translation    jsontext.Value         `json:"live_translation,omitempty"`
	Content        meetingcontent.Content `json:"content"`
}

func marshalEvidence(e *Evidence) ([]byte, error) { return json.Marshal(e, json.Deterministic(true)) }

// fallbackCall stands the listed row in for a details response Bland refused,
// laid over the details archived before so the stored response keeps them.
func fallbackCall(listed *Call, previous *Evidence) (*Call, error) {
	fields := map[string]jsontext.Value{}
	if previous != nil && len(previous.Call) > 0 {
		if err := json.Unmarshal(previous.Call, &fields); err != nil {
			return nil, malformed("archived call", err)
		}
	}
	var row map[string]jsontext.Value
	if err := json.Unmarshal(listed.Raw, &row); err != nil {
		return nil, malformed("call", err)
	}
	maps.Copy(fields, row)
	raw, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return decodeCall(raw, listed.ID, true)
}

// retainedCallMetadata is the call metadata archived before: the postcall
// payload's, then the details' and the merged call's.
func retainedCallMetadata(previous *Evidence) map[string]jsontext.Value {
	fields := map[string]jsontext.Value{}
	if previous == nil {
		return fields
	}
	if len(previous.PostCall) > 0 {
		if hook, err := parsePostCall(previous.PostCall, ""); err == nil {
			maps.Copy(fields, hook.Call.meta)
		}
	}
	for _, raw := range []jsontext.Value{previous.Call, previous.EffectiveCall} {
		if len(raw) == 0 {
			continue
		}
		if c, err := decodeDoc(raw); err == nil {
			maps.Copy(fields, c.meta)
		}
	}
	return fields
}

// effectiveCall fills metadata the detail response omits from the current
// postcall payload, then from what was archived before. Raw stays the exact
// detail response; the merged fields are kept so a later sparse refresh keeps them.
// GetPostCall already checked that the payload belongs to c.
func effectiveCall(c, payload *Call, previous *Evidence) (*Call, jsontext.Value, error) {
	fields := retainedCallMetadata(previous)
	if payload != nil {
		maps.Copy(fields, payload.meta)
	}
	maps.Copy(fields, c.meta)
	merged, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return nil, nil, err
	}
	effective, err := decodeCall(merged, c.ID, false)
	if err != nil {
		return nil, nil, err
	}
	effective.Raw = c.Raw
	return effective, merged, nil
}

func finiteNonnegative(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }

// renditionKind is the shape a transcript rendition's entries have.
type renditionKind int

const (
	// ordinary is the transcripts list, with entry creation times.
	ordinary renditionKind = iota
	// enhanced is the corrected transcript, with offsets from the call start.
	enhanced
	// transferred is the post-transfer transcript, offset by the transfer.
	transferred
)

// rendition is one decoded transcript version.
type rendition struct {
	raw      jsontext.Value
	offset   *float64
	segments []meetingcontent.Segment
	text     string
}

func (r *rendition) speaks() bool { return r != nil && (len(r.segments) > 0 || r.text != "") }

// decodeRendition decodes one transcript rendition of kind: a JSON string is
// plain text, and every entry of a list is an object whose fields have their
// types, times and timing valid.
func decodeRendition(raw jsontext.Value, kind renditionKind, offset *float64) (*rendition, error) {
	r := &rendition{raw: raw, offset: offset}
	if raw.Kind() == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		r.text = strings.TrimSpace(text)
		return r, nil
	}
	var items []*struct {
		Text    string         `json:"text"`
		User    string         `json:"user"`
		Speaker jsontext.Value `json:"speaker"`
		Label   string         `json:"speaker_label"`
		Created string         `json:"created_at"`
		Start   *float64       `json:"start"`
		End     *float64       `json:"end"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	lines := []string{}
	for _, v := range items {
		if v == nil {
			return nil, errors.New("null entry")
		}
		speaker := cmp.Or(v.Label, v.User)
		switch v.Speaker.Kind() {
		case 0, 'n':
		case '"':
			var name string
			if err := json.Unmarshal(v.Speaker, &name); err != nil {
				return nil, err
			}
			speaker = cmp.Or(speaker, name)
		case '0':
			var n float64
			if err := json.Unmarshal(v.Speaker, &n); err != nil {
				return nil, err
			}
			speaker = cmp.Or(speaker, strconv.FormatFloat(n, 'f', -1, 64))
		default:
			return nil, errors.New("speaker is neither a name nor a number")
		}
		seg := meetingcontent.Segment{Speaker: speaker, Text: strings.TrimSpace(v.Text)}
		switch kind {
		case ordinary:
			if v.Created != "" {
				t, err := time.Parse(time.RFC3339Nano, v.Created)
				if err != nil {
					return nil, fmt.Errorf("entry created_at: %w", err)
				}
				seg.StartedAt = &t
			}
		default:
			if v.Start == nil {
				break
			}
			if !finiteNonnegative(*v.Start) || (v.End != nil && (!finiteNonnegative(*v.End) || *v.End < *v.Start)) {
				return nil, errors.New("invalid timing")
			}
			if kind == enhanced || offset != nil {
				at := *v.Start
				if offset != nil {
					at += *offset
				}
				if !finiteNonnegative(at) {
					return nil, errors.New("invalid timing")
				}
				seg.OffsetSeconds = &at
			}
		}
		if speaker == "agent-action" || seg.Text == "" {
			continue
		}
		r.segments = append(r.segments, seg)
		line := seg.Text
		if speaker != "" {
			line = speaker + ": " + line
		}
		lines = append(lines, line)
	}
	r.text = strings.Join(lines, "\n")
	return r, nil
}

// archived decodes a rendition kept in evidence; one that no longer decodes is
// skipped.
func archived(raw jsontext.Value, kind renditionKind, offset *float64) *rendition {
	if len(raw) == 0 || raw.Kind() == 'n' {
		return nil
	}
	r, err := decodeRendition(raw, kind, offset)
	if err != nil {
		return nil
	}
	return r
}

// firstSpeech returns the first rendition with spoken content.
func firstSpeech(renditions ...*rendition) ([]meetingcontent.Segment, string) {
	for _, r := range renditions {
		if r.speaks() {
			return r.segments, r.text
		}
	}
	return nil, ""
}

// pick returns the first rendition with speech; one without speech can't hide
// another.
func pick(renditions ...*rendition) *rendition {
	for _, r := range renditions {
		if r.speaks() {
			return r
		}
	}
	return nil
}

// buildEvidence normalizes the call once from its detail response, postcall
// payload and earlier archive, and returns the evidence and effective call.
// unread, when set, is the transcript reason for a details or postcall read
// that failed this run.
func buildEvidence(detail *Call, hook *PostCall, previous *Evidence, unread string) (*Evidence, *Call, error) {
	payload := &Call{}
	if hook != nil {
		payload = hook.Call
	}
	c, merged, err := effectiveCall(detail, payload, previous)
	if err != nil {
		return nil, nil, err
	}
	e := &Evidence{Version: 1}
	if previous != nil {
		*e = *previous
	}
	e.Call, e.EffectiveCall = c.Raw, merged
	if hook != nil {
		e.PostCall = hook.Raw
	}
	// The details' transcripts win, then the payload's, then either
	// concatenated text; an enhanced rendition in the payload wins over the
	// details'. The details' live translation wins over the payload's.
	original := pick(detail.transcripts, payload.transcripts, detail.concatenated, payload.concatenated)
	corrected := pick(payload.corrected, detail.corrected)
	transfer := pick(payload.postTransfer, detail.postTransfer)
	if original != nil {
		e.Original = original.raw
	}
	if corrected != nil {
		e.Corrected = corrected.raw
	}
	if transfer != nil {
		e.PostTransfer, e.TransferOffset = transfer.raw, transfer.offset
	}
	if translation := cmp.Or(string(detail.translation), string(payload.translation)); translation != "" {
		e.Translation = jsontext.Value(translation)
	}
	content := meetingcontent.Content{Actions: []meetingcontent.Action{}, ActionCoverage: meetingcontent.CoverageUnsupported, ActionReason: "no_structured_actions", Notes: meetingcontent.Section{State: meetingcontent.StateUnsupported}, Summary: meetingcontent.Section{State: meetingcontent.StateUnavailable, Reason: "missing_field"}, Transcript: meetingcontent.Transcript{State: meetingcontent.StateUnavailable, Reason: "not_retained"}}
	if previous != nil {
		content.Summary = previous.Content.Summary
	}
	if strings.TrimSpace(c.Summary) != "" {
		content.Summary = meetingcontent.Section{State: meetingcontent.StateAvailable, Text: c.Summary}
	}
	if d := c.duration(); d != nil {
		content.DurationSeconds = d
		content.DurationBasis = meetingcontent.DurationProvider
	} else if previous != nil {
		content.DurationSeconds = previous.Content.DurationSeconds
		content.DurationBasis = previous.Content.DurationBasis
	}
	// e holds each rendition's current speech, or the speech archived before
	// when Bland omits it now.
	segments, text := firstSpeech(archived(e.Corrected, enhanced, nil), archived(e.Original, ordinary, nil))
	transferredSegments, transferText := firstSpeech(archived(e.PostTransfer, transferred, e.TransferOffset))
	segments = append(segments, transferredSegments...)
	text = strings.TrimSpace(strings.Join([]string{text, transferText}, "\n"))
	switch {
	case len(segments) > 0 || text != "":
		content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateAvailable, Text: text, Segments: segments}
	case unread != "":
		// A failed details or postcall read may have hidden a transcript Bland has.
		content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateUnavailable, Reason: unread}
	case len(e.Corrected) > 0 || len(e.Original) > 0 || len(e.PostTransfer) > 0 || detail.hasTranscript || payload.hasTranscript:
		content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateEmpty}
	}
	phone := c.To
	if c.Inbound {
		phone = c.From
	}
	if phone != "" {
		content.SourceParticipants = []meetingcontent.Participant{{Phone: phone, Role: "endpoint"}}
	}
	e.Content = content
	return e, c, nil
}
