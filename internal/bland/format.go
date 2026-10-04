package bland

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

// Evidence retains each rendition independently, so temporary omissions cannot
// destroy the corrected or transferred conversation already archived.
type Evidence struct {
	RenditionError   string                 `json:"rendition_error,omitempty"`
	ProxyCallIDs     []string               `json:"proxy_call_ids,omitempty"`
	Version          int                    `json:"version"`
	Call             jsontext.Value         `json:"call"`
	EffectiveCall    jsontext.Value         `json:"effective_call,omitempty"`
	DirectCorrection jsontext.Value         `json:"direct_correction,omitempty"`
	PostCall         jsontext.Value         `json:"postcall,omitempty"`
	Original         jsontext.Value         `json:"original,omitempty"`
	Corrected        jsontext.Value         `json:"corrected,omitempty"`
	PostTransfer     jsontext.Value         `json:"post_transfer,omitempty"`
	TransferOffset   *float64               `json:"transfer_offset_seconds,omitempty"`
	Translation      jsontext.Value         `json:"live_translation,omitempty"`
	Content          meetingcontent.Content `json:"content"`
	Recording        StoredRecording        `json:"recording"`
	ParentCallID     string                 `json:"parent_call_id,omitempty"`
}

func marshalEvidence(e *Evidence) ([]byte, error) { return json.Marshal(e, json.Deterministic(true)) }
func rawPresent(raw jsontext.Value) bool {
	return len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" && string(raw) != "\"\""
}

var callMetadataKeys = []string{"call_id", "c_id", "created_at", "started_at", "status", "queue_status", "completed", "from", "to", "inbound", "answered_by", "record", "recording_url", "corrected_duration", "call_length", "summary"}

func callMetadataFields(raw jsontext.Value) (map[string]jsontext.Value, error) {
	if !rawPresent(raw) {
		return map[string]jsontext.Value{}, nil
	}
	var values map[string]jsontext.Value
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return filteredCallMetadata(values), nil
}

func filteredCallMetadata(values map[string]jsontext.Value) map[string]jsontext.Value {
	fields := make(map[string]jsontext.Value)
	for _, key := range callMetadataKeys {
		if rawPresent(values[key]) {
			fields[key] = values[key]
		}
	}
	return fields
}

func postCallPayload(raw jsontext.Value) (map[string]jsontext.Value, error) {
	if !rawPresent(raw) {
		return map[string]jsontext.Value{}, nil
	}
	var wrapper struct {
		Data struct {
			Payload jsontext.Value `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	var payload map[string]jsontext.Value
	if err := json.Unmarshal(wrapper.Data.Payload, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, ErrInvalidPayload
	}
	return payload, nil
}

func retainedCallMetadata(previous *Evidence) map[string]jsontext.Value {
	fields := map[string]jsontext.Value{}
	if previous == nil {
		return fields
	}
	if payload, err := postCallPayload(previous.PostCall); err == nil {
		maps.Copy(fields, filteredCallMetadata(payload))
	}
	if call, err := callMetadataFields(previous.Call); err == nil {
		maps.Copy(fields, call)
	}
	if effective, err := callMetadataFields(previous.EffectiveCall); err == nil {
		maps.Copy(fields, effective)
	}
	return fields
}

func effectiveCallMetadata(call, hook jsontext.Value, previous *Evidence) (jsontext.Value, error) {
	fields := retainedCallMetadata(previous)
	payload, err := postCallPayload(hook)
	if err != nil {
		return nil, fmt.Errorf("decode current Bland metadata hook: %w", err)
	}
	for _, key := range callMetadataKeys {
		if rawPresent(payload[key]) {
			fields[key] = payload[key]
		}
	}
	detail, err := callMetadataFields(call)
	if err != nil {
		return nil, fmt.Errorf("decode current Bland call metadata: %w", err)
	}
	maps.Copy(fields, detail)
	if len(fields) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshal effective Bland call metadata: %w", err)
	}
	return jsontext.Value(raw), nil
}

func finiteNonnegative(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }
func buildEvidence(c *Call, hook jsontext.Value, previous *Evidence, correction ...jsontext.Value) (*Evidence, error) {
	var enrichErr error
	c, enrichErr = enrichedCall(c, hook)
	if enrichErr != nil {
		return nil, enrichErr
	}
	e := &Evidence{Version: 1, Call: c.Raw}
	if previous != nil {
		*e = *previous
		e.Call = c.Raw
	}
	effectiveCall, err := effectiveCallMetadata(c.Raw, hook, previous)
	if err != nil {
		return nil, err
	}
	e.EffectiveCall = effectiveCall
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(c.Raw, &fields); err != nil {
		return nil, err
	}
	if rawPresent(fields["transcripts"]) {
		e.Original = fields["transcripts"]
	} else if rawPresent(fields["concatenated_transcript"]) {
		e.Original = fields["concatenated_transcript"]
	}
	var payload map[string]jsontext.Value
	if len(hook) > 0 {
		var w struct {
			Data struct {
				Payload jsontext.Value `json:"payload"`
			} `json:"data"`
		}
		if json.Unmarshal(hook, &w) != nil || json.Unmarshal(w.Data.Payload, &payload) != nil || payload == nil {
			return nil, ErrInvalidPayload
		}
		e.PostCall = hook
	}
	if rawPresent(payload["transcripts"]) && !rawPresent(fields["transcripts"]) {
		e.Original = payload["transcripts"]
	} else if !rawPresent(e.Original) && rawPresent(payload["concatenated_transcript"]) {
		e.Original = payload["concatenated_transcript"]
	}
	hasCurrentCorrected := false
	for _, source := range []map[string]jsontext.Value{fields, payload} {
		if rawPresent(source["corrected_transcript"]) {
			e.Corrected = source["corrected_transcript"]
			hasCurrentCorrected = true
		}
		if rawPresent(source["post_transfer_transcript"]) {
			e.PostTransfer = source["post_transfer_transcript"]
			// Offset belongs to this rendition, not a previous transfer snapshot.
			e.TransferOffset = nil
			var offset float64
			if raw := source["transfer_offset_seconds"]; len(raw) > 0 && json.Unmarshal(raw, &offset) == nil && finiteNonnegative(offset) {
				e.TransferOffset = &offset
			}
		}
		if rawPresent(source["live_translation_transcript"]) {
			e.Translation = source["live_translation_transcript"]
		}
	}
	if len(correction) > 0 && len(correction[0]) > 0 {
		var direct struct {
			Corrected jsontext.Value `json:"corrected"`
		}
		if json.Unmarshal(correction[0], &direct) != nil {
			return nil, ErrInvalidPayload
		}
		e.DirectCorrection = correction[0]
		if rawPresent(direct.Corrected) {
			e.Corrected = direct.Corrected
		}
	} else if !hasCurrentCorrected && rawPresent(e.DirectCorrection) {
		var direct struct {
			Corrected jsontext.Value `json:"corrected"`
		}
		if json.Unmarshal(e.DirectCorrection, &direct) == nil && rawPresent(direct.Corrected) {
			e.Corrected = direct.Corrected
		}
	}
	proxySet := map[string]bool{}
	for _, id := range e.ProxyCallIDs {
		proxySet[id] = true
	}
	for _, id := range append(proxyCalls(c.Raw), proxyCallsFromHook(hook)...) {
		if validID(id) && id != c.ID {
			proxySet[id] = true
		}
	}
	e.ProxyCallIDs = nil
	for id := range proxySet {
		e.ProxyCallIDs = append(e.ProxyCallIDs, id)
	}
	sort.Strings(e.ProxyCallIDs)
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
	pre := e.Corrected
	enhanced := rawPresent(pre)
	if !enhanced {
		pre = e.Original
	}
	var renditionErr error
	segments, text, err := decodeSegments(pre, enhanced, nil, false)
	if err != nil {
		renditionErr = fmt.Errorf("decode Bland pre-transfer transcript: %w", err)
		segments, text = nil, ""
		recovered := false
		if previous != nil {
			prior := previous.Original
			if enhanced {
				prior = previous.Corrected
			}
			if rawPresent(prior) {
				oldSegments, oldText, oldErr := decodeSegments(prior, enhanced, nil, false)
				if oldErr == nil {
					segments, text, recovered = oldSegments, oldText, true
					if enhanced {
						e.Corrected = prior
					} else {
						e.Original = prior
					}
				}
			}
		}
		if enhanced && !recovered {
			segments, text, err = decodeSegments(e.Original, false, nil, false)
			renditionErr = errors.Join(renditionErr, err)
			if err != nil && previous != nil && rawPresent(previous.Original) {
				oldSegments, oldText, oldErr := decodeSegments(previous.Original, false, nil, false)
				if oldErr == nil {
					segments, text = oldSegments, oldText
					e.Original = previous.Original
				}
			}
		}
	}
	if len(segments) == 0 && text == "" && previous != nil {
		priorRenditions := []struct {
			raw      jsontext.Value
			enhanced bool
		}{}
		if enhanced {
			priorRenditions = append(priorRenditions, struct {
				raw      jsontext.Value
				enhanced bool
			}{previous.Corrected, true})
		}
		priorRenditions = append(priorRenditions, struct {
			raw      jsontext.Value
			enhanced bool
		}{previous.Original, false})
		for _, prior := range priorRenditions {
			if !rawPresent(prior.raw) {
				continue
			}
			oldSegments, oldText, oldErr := decodeSegments(prior.raw, prior.enhanced, nil, false)
			if oldErr != nil || (len(oldSegments) == 0 && oldText == "") {
				continue
			}
			segments, text = oldSegments, oldText
			if prior.enhanced {
				e.Corrected = prior.raw
			} else {
				e.Original = prior.raw
			}
			break
		}
	}
	transferred, transferText, err := decodeSegments(e.PostTransfer, true, e.TransferOffset, true)
	if err != nil {
		renditionErr = errors.Join(renditionErr, fmt.Errorf("decode Bland post-transfer transcript: %w", err))
		transferred, transferText = nil, ""
		if previous != nil && rawPresent(previous.PostTransfer) {
			oldSegments, oldText, oldErr := decodeSegments(previous.PostTransfer, true, previous.TransferOffset, true)
			if oldErr == nil {
				transferred, transferText = oldSegments, oldText
				e.PostTransfer, e.TransferOffset = previous.PostTransfer, previous.TransferOffset
			}
		}
	}
	if len(transferred) == 0 && transferText == "" && previous != nil && rawPresent(previous.PostTransfer) {
		oldSegments, oldText, oldErr := decodeSegments(previous.PostTransfer, true, previous.TransferOffset, true)
		if oldErr == nil && (len(oldSegments) > 0 || oldText != "") {
			transferred, transferText = oldSegments, oldText
			e.PostTransfer, e.TransferOffset = previous.PostTransfer, previous.TransferOffset
		}
	}
	segments = append(segments, transferred...)
	text = strings.TrimSpace(strings.Join([]string{text, transferText}, "\n"))
	if len(segments) > 0 || text != "" {
		content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateAvailable, Text: text, Segments: segments}
	} else if renditionErr != nil {
		content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateUnavailable, Reason: "invalid_retained_payload"}
	} else if rawPresent(pre) || rawPresent(e.PostTransfer) || hasEmptyTranscript(fields) || hasEmptyTranscript(payload) {
		content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateEmpty}
	}
	phone := c.To
	if c.Inbound {
		phone = c.From
	}
	if phone != "" {
		content.SourceParticipants = []meetingcontent.Participant{{Phone: phone, Role: "endpoint"}}
	}
	e.RenditionError = ""
	if renditionErr != nil {
		e.RenditionError = "invalid_transcript_rendition"
	}
	e.Content = content
	return e, renditionErr
}
func decodeSegments(raw jsontext.Value, enhanced bool, transferOffset *float64, isTransfer bool) ([]meetingcontent.Segment, string, error) {
	if !rawPresent(raw) {
		return nil, "", nil
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return nil, strings.TrimSpace(plain), nil
	}
	// Some retained payloads wrap correction output in the GET result envelope.
	var wrapper map[string]jsontext.Value
	if json.Unmarshal(raw, &wrapper) == nil && wrapper != nil {
		raw = wrapper["corrected"]
		if !rawPresent(raw) {
			return nil, "", ErrInvalidPayload
		}
	}
	var items []struct {
		Text    string         `json:"text"`
		User    string         `json:"user"`
		Speaker jsontext.Value `json:"speaker"`
		Label   string         `json:"speaker_label"`
		Created string         `json:"created_at"`
		Start   *float64       `json:"start"`
		End     *float64       `json:"end"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil, "", ErrInvalidPayload
	}
	out := []meetingcontent.Segment{}
	lines := []string{}
	for _, v := range items {
		speaker := v.Label
		if speaker == "" {
			speaker = v.User
		}
		if speaker == "" && len(v.Speaker) > 0 {
			if json.Unmarshal(v.Speaker, &speaker) != nil {
				var n float64
				if json.Unmarshal(v.Speaker, &n) == nil {
					speaker = strconv.FormatFloat(n, 'f', -1, 64)
				}
			}
		}
		if speaker == "agent-action" || strings.TrimSpace(v.Text) == "" {
			continue
		}
		seg := meetingcontent.Segment{Speaker: speaker, Text: strings.TrimSpace(v.Text)}
		if enhanced && v.Start != nil {
			if !finiteNonnegative(*v.Start) || (v.End != nil && (!finiteNonnegative(*v.End) || *v.End < *v.Start)) {
				return nil, "", fmt.Errorf("%w: invalid transcript timing", ErrInvalidPayload)
			}
			if !isTransfer || transferOffset != nil {
				offset := *v.Start
				if transferOffset != nil {
					offset += *transferOffset
				}
				if !finiteNonnegative(offset) {
					return nil, "", ErrInvalidPayload
				}
				seg.OffsetSeconds = &offset
			}
		} else if !enhanced {
			if t, err := time.Parse(time.RFC3339Nano, v.Created); err == nil {
				seg.StartedAt = &t
			}
		}
		out = append(out, seg)
		line := seg.Text
		if speaker != "" {
			line = speaker + ": " + line
		}
		lines = append(lines, line)
	}
	return out, strings.Join(lines, "\n"), nil
}

func hasEmptyTranscript(fields map[string]jsontext.Value) bool {
	for _, key := range []string{"transcripts", "concatenated_transcript", "corrected_transcript", "post_transfer_transcript"} {
		if raw, ok := fields[key]; ok && string(raw) != "null" && len(raw) > 0 {
			return true
		}
	}
	return false
}
