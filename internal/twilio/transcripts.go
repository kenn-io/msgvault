package twilio

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
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

// Transcripts collects each recording's legacy and Conversation Intelligence
// transcripts. A failed collection never exposes a partial transcript as complete.
func (c *Client) Transcripts(ctx context.Context, recordings []Recording) (Evidence, error) {
	var evidence Evidence
	var failures []error
	// A refused read leaves that recording's transcript unavailable with a
	// note; any other failure fails the call.
	addFailure := func(product string, err error) {
		switch {
		case err == nil:
		case refused(err):
			apiErr, _ := errors.AsType[*APIError](err)
			evidence.Diagnostics = append(evidence.Diagnostics, fmt.Sprintf("%s coverage unavailable (HTTP %d)", product, apiErr.StatusCode))
		default:
			evidence.Diagnostics = append(evidence.Diagnostics, product+" acquisition failed")
			failures = append(failures, err)
		}
	}
	add := func(product string, transcripts []Transcript) {
		for _, tr := range transcripts {
			if tr.Complete && !tr.Usable {
				evidence.Diagnostics = append(evidence.Diagnostics, product+" unavailable: completed transcript "+tr.ID+" has no speech")
			}
		}
		evidence.Transcripts = append(evidence.Transcripts, transcripts...)
	}
	for _, recording := range recordings {
		legacy, err := c.legacy(ctx, recording.SID)
		addFailure("legacy transcription", err)
		add("legacy transcription", legacy)
		if c.options.Region != "us1" {
			continue
		}
		classic, err := c.classic(ctx, recording.SID)
		addFailure("classic intelligence", err)
		add("classic intelligence", classic)
	}
	return evidence, errors.Join(failures...)
}

type legacyTranscript struct {
	SID          string  `json:"sid"`
	AccountSID   string  `json:"account_sid"`
	RecordingSID string  `json:"recording_sid"`
	Status       string  `json:"status"`
	DateCreated  string  `json:"date_created"`
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
		tr := Transcript{Kind: "legacy", ID: item.SID, SourceID: recordingID, Status: item.Status, DateCreated: item.DateCreated, Complete: completeStatus(item.Status), Raw: raw}
		if tr.Complete && item.Text != nil && *item.Text != "" {
			tr.Segments = []Segment{{Text: *item.Text, Scope: recordingID}}
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
	Created    string `json:"date_created"`
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
		// The listing carries the same fields as the transcript resource.
		metadata := raw
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
		tr := Transcript{Kind: "classic", ID: id, SourceID: sourceID, Status: item.Status, DateCreated: item.Created, Complete: completeStatus(item.Status), Raw: metadata}
		if tr.Complete {
			sentences, err := c.collection(ctx, "intelligence", "/v2/Transcripts/"+id+"/Sentences", "sentences", url.Values{pageSizeQuery: {"1000"}})
			if err != nil {
				return nil, err
			}
			parsed := make([]sentence, 0, len(sentences))
			for _, raw := range sentences {
				var item sentence
				if err := json.Unmarshal(raw, &item); err != nil {
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
				if item.Text != nil && *item.Text != "" {
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
