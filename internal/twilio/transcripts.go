package twilio

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func completeStatus(status string) bool { return strings.EqualFold(status, "completed") }
func rawEvidence(value any) (jsontext.Value, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("twilio: encode transcript evidence: %w", err)
	}
	return raw, nil
}
func finishTranscript(value *Transcript) {
	segments := value.Segments[:0]
	for _, segment := range value.Segments {
		if strings.TrimSpace(segment.Text) != "" {
			segments = append(segments, segment)
		}
	}
	value.Segments = segments
	value.Usable = value.Complete && len(value.Segments) > 0
}

// Transcripts collects independent, complete representations. Failed collections
// never expose a partially acquired transcript as complete or usable.
func (c *Client) Transcripts(ctx context.Context, call Call, recordings []Recording) (Evidence, error) {
	var evidence Evidence
	var failures []error
	if !validSID(call.SID, "CA") {
		return evidence, errors.New("twilio: invalid call SID")
	}
	if err := c.validateAccount(call.AccountSID); err != nil {
		return evidence, err
	}
	addFailure := func(product string, err error, configured bool) {
		if err == nil {
			return
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound) {
			evidence.Diagnostics = append(evidence.Diagnostics, fmt.Sprintf("%s coverage unavailable (HTTP %d)", product, apiErr.StatusCode))
			if apiErr.StatusCode != http.StatusNotFound && configured {
				failures = append(failures, err)
			}
			return
		}
		evidence.Diagnostics = append(evidence.Diagnostics, product+" acquisition failed")
		failures = append(failures, err)
	}
	seenRecordings := map[string]bool{}
	for _, recording := range recordings {
		if !validSID(recording.SID, "RE") || recording.CallSID != call.SID {
			return evidence, errors.New("twilio: transcript recording call mismatch")
		}
		if err := c.validateAccount(recording.AccountSID); err != nil {
			return evidence, err
		}
		if seenRecordings[recording.SID] {
			continue
		}
		seenRecordings[recording.SID] = true
		legacy, err := c.legacy(ctx, recording.SID)
		addFailure("legacy transcription", err, false)
		evidence.Transcripts = append(evidence.Transcripts, legacy...)
		if c.options.Region != "us1" {
			continue
		}
		classic, err := c.classic(ctx, recording.SID)
		addFailure("classic intelligence", err, c.options.IntelligenceServiceSID != "")
		evidence.Transcripts = append(evidence.Transcripts, classic...)
		batch, diagnostics, err := c.batch(ctx, call.SID, recording.SID)
		addFailure("Batch transcription", err, false)
		evidence.Diagnostics = append(evidence.Diagnostics, diagnostics...)
		evidence.Transcripts = append(evidence.Transcripts, batch...)
	}
	if c.options.Region != "us1" {
		evidence.Diagnostics = append(evidence.Diagnostics, "classic intelligence, Relay Insights, Batch and Orchestrator coverage is not established for region "+c.options.Region+"; US credentials were not used")
		return evidence, errors.Join(failures...)
	}
	if c.options.RelayDiscovery {
		sessions, events, err := c.relaySessions(ctx, call.SID)
		evidence.RelayEvents = events
		addFailure("Relay Insights", err, false)
		for _, session := range sessions {
			classic, err := c.classic(ctx, session)
			addFailure("Relay classic intelligence", err, c.options.IntelligenceServiceSID != "")
			evidence.Transcripts = append(evidence.Transcripts, classic...)
		}
	}
	conversations, rawConversations, err := c.conversations(ctx, call.SID)
	evidence.Conversations = rawConversations
	addFailure("Orchestrator conversation discovery", err, false)
	for _, conversation := range conversations {
		tr, err := c.communications(ctx, call.SID, conversation, "orchestrator", call.SID)
		addFailure("Orchestrator communications", err, false)
		if err == nil {
			evidence.Transcripts = append(evidence.Transcripts, tr)
		}
	}
	// A provider may repeat IDs across pages or discovery paths.
	seen := map[string]bool{}
	unique := evidence.Transcripts[:0]
	for _, tr := range evidence.Transcripts {
		key := tr.Kind + ":" + tr.ID + ":" + tr.SourceID
		if !seen[key] {
			seen[key] = true
			unique = append(unique, tr)
		}
	}
	evidence.Transcripts = unique
	return evidence, errors.Join(failures...)
}

type legacyTranscript struct {
	SID          string  `json:"sid"`
	AccountSID   string  `json:"account_sid"`
	RecordingSID string  `json:"recording_sid"`
	Status       string  `json:"status"`
	Text         *string `json:"transcription_text"`
}

func (c *Client) legacy(ctx context.Context, recordingID string) ([]Transcript, error) {
	items, err := c.collection(ctx, "voice", c.accountPath()+"/Recordings/"+recordingID+"/Transcriptions.json", "transcriptions", url.Values{pageSizeQuery: {"1000"}})
	if err != nil {
		return nil, err
	}
	var result []Transcript
	for _, raw := range items {
		var item legacyTranscript
		if err := json.Unmarshal(raw, &item); err != nil || !validSID(item.SID, "TR") {
			return nil, errors.New("twilio: invalid legacy transcription")
		}
		if err := c.validateAccount(item.AccountSID); err != nil {
			return nil, err
		}
		if item.RecordingSID != "" && item.RecordingSID != recordingID {
			return nil, errors.New("twilio: legacy transcription recording mismatch")
		}
		if completeStatus(item.Status) && item.Text == nil {
			id := item.SID
			var detail jsontext.Value
			if err := c.getJSON(ctx, "voice", c.endpoint("voice", c.accountPath()+"/Transcriptions/"+id+".json", nil), &detail); err != nil {
				return nil, err
			}
			var hydrated legacyTranscript
			if err := json.Unmarshal(detail, &hydrated); err != nil || hydrated.SID != id {
				return nil, errors.New("twilio: legacy transcription SID mismatch")
			}
			item = hydrated
			raw, err = rawEvidence(struct {
				Listing jsontext.Value `json:"listing"`
				Detail  jsontext.Value `json:"detail"`
			}{raw, detail})
			if err != nil {
				return nil, err
			}
		}
		if item.RecordingSID != recordingID {
			return nil, errors.New("twilio: legacy transcription recording mismatch")
		}
		if err := c.validateAccount(item.AccountSID); err != nil {
			return nil, err
		}
		tr := Transcript{Kind: "legacy", ID: item.SID, SourceID: recordingID, Status: item.Status, Complete: completeStatus(item.Status), Raw: raw}
		if tr.Complete {
			if item.Text == nil {
				return nil, errors.New("twilio: completed legacy transcription has no text field")
			}
			if *item.Text != "" {
				tr.Segments = []Segment{{Text: *item.Text, Scope: recordingID}}
			}
		}
		finishTranscript(&tr)
		result = append(result, tr)
	}
	return result, nil
}

type classicTranscript struct {
	SID        string `json:"sid"`
	AccountSID string `json:"account_sid"`
	SourceSID  string `json:"source_sid"`
	ServiceSID string `json:"service_sid"`
	Status     string `json:"status"`
	Channel    struct {
		MediaProperties struct {
			SourceSID string `json:"source_sid"`
			Source    string `json:"source"`
		} `json:"media_properties"`
		Participants []struct {
			Channel int    `json:"channel_participant"`
			Role    string `json:"role"`
		} `json:"participants"`
	} `json:"channel"`
}
type sentence struct {
	SID     string   `json:"sid"`
	Index   int      `json:"sentence_index"`
	Channel *int     `json:"media_channel"`
	Start   *float64 `json:"start_time"`
	End     *float64 `json:"end_time"`
	Text    *string  `json:"transcript"`
}

func (s *sentence) UnmarshalJSON(data []byte) error {
	var value struct {
		SID     string         `json:"sid"`
		Index   int            `json:"sentence_index"`
		Channel jsontext.Value `json:"media_channel"`
		Start   jsontext.Value `json:"start_time"`
		End     jsontext.Value `json:"end_time"`
		Text    *string        `json:"transcript"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parse := func(raw jsontext.Value) (float64, bool, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return 0, false, nil
		}
		text := string(raw)
		if raw[0] == '"' {
			if err := json.Unmarshal(raw, &text); err != nil {
				return 0, false, err
			}
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0, false, errors.New("invalid sentence offset")
		}
		return v, true, nil
	}
	startValue, hasStart, err := parse(value.Start)
	if err != nil {
		return err
	}
	endValue, hasEnd, err := parse(value.End)
	if err != nil {
		return err
	}
	channelValue, hasChannel, err := parse(value.Channel)
	if err != nil {
		return err
	}
	var channel *int
	if hasChannel {
		if math.Trunc(channelValue) != channelValue || channelValue > 2 {
			return errors.New("invalid media channel")
		}
		v := int(channelValue)
		channel = &v
	}
	var start, end *float64
	if hasStart {
		start = &startValue
	}
	if hasEnd {
		end = &endValue
	}
	*s = sentence{SID: value.SID, Index: value.Index, Channel: channel, Start: start, End: end, Text: value.Text}
	return nil
}
func (c *Client) classic(ctx context.Context, sourceID string) ([]Transcript, error) {
	q := url.Values{"SourceSid": {sourceID}, pageSizeQuery: {"1000"}}
	if c.options.IntelligenceServiceSID != "" {
		q.Set("ServiceSid", c.options.IntelligenceServiceSID)
	}
	items, err := c.collection(ctx, "intelligence", "/v2/Transcripts", "transcripts", q)
	if err != nil {
		return nil, err
	}
	var result []Transcript
	seen := map[string]bool{}
	for _, raw := range items {
		var item classicTranscript
		if err := json.Unmarshal(raw, &item); err != nil || !validSID(item.SID, "GT") {
			return nil, errors.New("twilio: invalid classic transcript")
		}
		id := item.SID
		if seen[id] {
			continue
		}
		seen[id] = true
		// Fetch metadata to obtain the final status rather than assuming list freshness.
		var metadata jsontext.Value
		if err := c.getJSON(ctx, "intelligence", c.endpoint("intelligence", "/v2/Transcripts/"+id, nil), &metadata); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metadata, &item); err != nil || item.SID != id {
			return nil, errors.New("twilio: classic transcript SID mismatch")
		}
		nestedSource := item.Channel.MediaProperties.SourceSID
		if nestedSource != "" {
			if item.SourceSID != "" && item.SourceSID != nestedSource {
				return nil, errors.New("twilio: contradictory classic transcript source")
			}
			item.SourceSID = nestedSource
		}
		if item.SourceSID != sourceID {
			return nil, errors.New("twilio: classic transcript source mismatch")
		}
		if err := c.validateAccount(item.AccountSID); err != nil {
			return nil, err
		}
		if c.options.IntelligenceServiceSID != "" && item.ServiceSID != "" && item.ServiceSID != c.options.IntelligenceServiceSID {
			return nil, errors.New("twilio: classic transcript service mismatch")
		}
		trMetadata := item
		tr := Transcript{Kind: "classic", ID: id, SourceID: sourceID, Status: item.Status, Complete: completeStatus(item.Status), Raw: metadata}
		if tr.Complete {
			sentences, err := c.collection(ctx, "intelligence", "/v2/Transcripts/"+id+"/Sentences", "sentences", url.Values{pageSizeQuery: {"1000"}})
			if err != nil {
				return nil, err
			}
			parsed := make([]sentence, 0, len(sentences))
			for _, raw := range sentences {
				var item sentence
				if err := json.Unmarshal(raw, &item); err != nil || item.Text == nil {
					return nil, errors.New("twilio: invalid transcript sentence")
				}
				parsed = append(parsed, item)
			}
			sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].Index < parsed[j].Index })
			sentenceIDs := map[string]bool{}
			for _, item := range parsed {
				channelID := 0
				if item.Channel != nil {
					channelID = *item.Channel
				}
				key := fmt.Sprintf("%d:%d", item.Index, channelID)
				if item.SID != "" {
					key = item.SID
				}
				if sentenceIDs[key] {
					continue
				}
				sentenceIDs[key] = true
				if *item.Text != "" {
					speaker := ""
					if item.Channel != nil {
						speaker = fmt.Sprintf("channel %d", channelID)
						for _, participant := range trMetadata.Channel.Participants {
							if participant.Channel == channelID && participant.Role != "" {
								speaker = participant.Role
								break
							}
						}
					}
					tr.Segments = append(tr.Segments, Segment{Speaker: speaker, Text: *item.Text, Scope: sourceID, OffsetSeconds: item.Start})
				}
			}
			tr.Raw, err = rawEvidence(struct {
				Metadata  jsontext.Value   `json:"metadata"`
				Sentences []jsontext.Value `json:"sentences"`
			}{metadata, sentences})
			if err != nil {
				return nil, err
			}
		}
		finishTranscript(&tr)
		result = append(result, tr)
	}
	return result, nil
}
func (c *Client) relaySessions(ctx context.Context, callID string) ([]string, []jsontext.Value, error) {
	items, err := c.collection(ctx, "insights", "/v1/Voice/"+callID+"/Events", "events", url.Values{"Edge": {"carrier_edge"}, pageSizeQuery: {"1000"}})
	if err != nil {
		return nil, nil, err
	}
	var result []string
	seen := map[string]bool{}
	for _, raw := range items {
		var item struct {
			CallSID    string `json:"call_sid"`
			AccountSID string `json:"account_sid"`
			Relay      struct {
				SessionID string `json:"session_id"`
			} `json:"conversation_relay_data"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, nil, errors.New("twilio: invalid Relay event")
		}
		if err := c.validateAccount(item.AccountSID); err != nil {
			return nil, nil, err
		}
		if item.CallSID != "" && item.CallSID != callID {
			return nil, nil, errors.New("twilio: Relay event call mismatch")
		}
		if item.Relay.SessionID == "" {
			continue
		}
		if !validSID(item.Relay.SessionID, "VX") {
			return nil, nil, errors.New("twilio: invalid Relay session SID")
		}
		if !seen[item.Relay.SessionID] {
			seen[item.Relay.SessionID] = true
			result = append(result, item.Relay.SessionID)
		}
	}
	return result, items, nil
}

type batchTranscript struct {
	ID             string `json:"id"`
	AccountID      string `json:"accountId"`
	SourceID       string `json:"sourceId"`
	Status         string `json:"status"`
	ConversationID string `json:"conversationId"`
}

func (c *Client) batch(ctx context.Context, callID, recordingID string) ([]Transcript, []string, error) {
	items, err := c.collection(ctx, "batch", "/v3/Transcriptions", "transcriptions", url.Values{"sourceId": {recordingID}, "pageSize": {"100"}})
	if err != nil {
		return nil, nil, err
	}
	var result []Transcript
	var diagnostics []string
	seen := map[string]bool{}
	for _, raw := range items {
		var item batchTranscript
		if err := json.Unmarshal(raw, &item); err != nil || !resourcePattern.MatchString(item.ID) {
			return nil, diagnostics, errors.New("twilio: invalid Batch transcription")
		}
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		// Batch list metadata can precede conversation retention; refresh the job.
		var detail jsontext.Value
		if err := c.getJSON(ctx, "batch", c.endpoint("batch", "/v3/Transcriptions/"+item.ID, nil), &detail); err != nil {
			return nil, diagnostics, err
		}
		id := item.ID
		var envelope struct {
			OperationID   string           `json:"operationId"`
			Transcription *batchTranscript `json:"transcription"`
		}
		if err := json.Unmarshal(detail, &envelope); err != nil || envelope.Transcription == nil || envelope.Transcription.ID != id || envelope.OperationID != "" && envelope.OperationID != id {
			return nil, diagnostics, errors.New("twilio: Batch transcription ID mismatch")
		}
		item = *envelope.Transcription
		raw = detail
		if item.SourceID != recordingID {
			return nil, diagnostics, errors.New("twilio: Batch source ID mismatch")
		}
		if err := c.validateAccount(item.AccountID); err != nil {
			return nil, diagnostics, err
		}
		tr := Transcript{Kind: "batch", ID: item.ID, SourceID: recordingID, Status: item.Status, Raw: raw, Complete: completeStatus(item.Status)}
		result = append(result, tr)
		if completeStatus(item.Status) {
			if item.ConversationID == "" {
				diagnostics = append(diagnostics, "completed Batch transcription has no retained conversation text")
			} else {
				retained, err := c.communications(ctx, callID, item.ConversationID, "orchestrator", callID)
				if err != nil {
					return result, diagnostics, err
				}
				result = append(result, retained)
			}
		}
	}
	return result, diagnostics, nil
}
func (c *Client) conversations(ctx context.Context, callID string) ([]string, []jsontext.Value, error) {
	items, err := c.collection(ctx, "orchestrator", "/v2/Conversations", "conversations", url.Values{"channelId": {callID}, "pageSize": {"1000"}})
	if err != nil {
		return nil, nil, err
	}
	var result []string
	seen := map[string]bool{}
	for _, raw := range items {
		var item struct {
			ID        string `json:"id"`
			AccountID string `json:"accountId"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || !resourcePattern.MatchString(item.ID) {
			return nil, nil, errors.New("twilio: invalid conversation")
		}
		if err := c.validateAccount(item.AccountID); err != nil {
			return nil, nil, err
		}
		if !seen[item.ID] {
			seen[item.ID] = true
			result = append(result, item.ID)
		}
	}
	return result, items, nil
}
func (c *Client) communications(ctx context.Context, callID, conversationID, kind, sourceID string) (Transcript, error) {
	tr := Transcript{Kind: kind, ID: conversationID, SourceID: sourceID, Status: "completed"}
	if !resourcePattern.MatchString(conversationID) {
		return tr, errors.New("twilio: invalid conversation ID")
	}
	items, err := c.collection(ctx, "orchestrator", "/v2/Conversations/"+conversationID+"/Communications", "communications", url.Values{"channelId": {callID}, "pageSize": {"1000"}})
	if err != nil {
		return tr, err
	}
	seen := map[string]bool{}
	for _, raw := range items {
		var item Communication
		if err := json.Unmarshal(raw, &item); err != nil || !resourcePattern.MatchString(item.ID) {
			return tr, errors.New("twilio: invalid communication")
		}
		if item.ChannelID != callID || item.ConversationID != conversationID {
			return tr, errors.New("twilio: communication belongs to another call or conversation")
		}
		if err := c.validateAccount(item.AccountID); err != nil {
			return tr, err
		}
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		tr.Communications = append(tr.Communications, item)
	}
	sort.SliceStable(tr.Communications, func(i, j int) bool {
		a, b := tr.Communications[i], tr.Communications[j]
		ta, tb := ParseTime(a.OccurredAt), ParseTime(b.OccurredAt)
		if ta.Equal(tb) {
			return a.ID < b.ID
		}
		return ta.Before(tb)
	})
	for _, item := range tr.Communications {
		if item.Content.Type != "TEXT" || item.Content.Text == "" {
			continue
		}
		speaker := item.Author.ParticipantID
		if speaker == "" {
			speaker = item.Author.Address
		}
		var started *time.Time
		if parsed := ParseTime(item.OccurredAt); !parsed.IsZero() {
			started = &parsed
		}
		tr.Segments = append(tr.Segments, Segment{Speaker: speaker, Text: item.Content.Text, Scope: callID, StartedAt: started})
	}
	tr.Raw, err = rawEvidence(items)
	if err != nil {
		return tr, err
	}
	tr.Complete = true
	finishTranscript(&tr)
	return tr, nil
}
