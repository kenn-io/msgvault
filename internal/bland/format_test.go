package bland

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

func TestCanonicalRetainedTransfer(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	c, e := parseCall([]byte(`{"call_id":"call-1","started_at":"2026-10-01T12:00:00Z","end_at":"2026-10-01T15:00:00Z","corrected_duration":"20","from":"+12025550100","to":"+12025550101","transcripts":[{"text":"raw duplicate","user":"user"},{"text":"internal action","user":"agent-action"}]}`), "")
	requirements.NoError(e)
	hook := jsontext.Value(`{"data":{"payload":{"corrected_transcript":[{"text":"Corrected speech","speaker_label":"user","start":1,"end":2}],"transfer_offset_seconds":9.573,"post_transfer_transcript":[{"text":"Transferred speech","speaker_label":"representative","start":9.544,"end":17.804}],"live_translation_transcript":[{"text":"translation variant"}]}}}`)
	ev, e := buildEvidence(c, hook, nil)
	requirements.NoError(e)
	assertions.Equal(meetingcontent.StateAvailable, ev.Content.Transcript.State)
	requirements.Len(ev.Content.Transcript.Segments, 2)
	assertions.InDelta(19.117, *ev.Content.Transcript.Segments[1].OffsetSeconds, 0.0001)
	assertions.NotContains(ev.Content.Transcript.Text, "duplicate")
	assertions.NotContains(ev.Content.Transcript.Text, "internal action")
	assertions.NotContains(ev.Content.Transcript.Text, "translation variant")
	assertions.InDelta(20.0, *ev.Content.DurationSeconds, 0.0001)
	assertions.Equal("+12025550101", ev.Content.SourceParticipants[0].Phone)
	ev2, e := buildEvidence(c, nil, ev)
	requirements.NoError(e)
	assertions.Equal(ev.Content.Transcript, ev2.Content.Transcript)
	raw, e := marshalEvidence(ev)
	requirements.NoError(e)
	assertions.Equal(ev.Content, meetingcontent.Decode(RawFormat, raw, nil))
}
func TestMissingTransferOffsetAndRecordingOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	c, e := parseCall([]byte(`{"call_id":"recording-only","completed":true,"record":true,"call_length":1.5}`), "")
	requirements.NoError(e)
	ev, e := buildEvidence(c, nil, nil)
	requirements.NoError(e)
	assertions.Equal(meetingcontent.StateUnavailable, ev.Content.Transcript.State)
	assertions.InDelta(90.0, *ev.Content.DurationSeconds, 0.0001)
	hook := jsontext.Value(`{"data":{"payload":{"post_transfer_transcript":[{"text":"Transfer","speaker_label":"user","start":2,"end":3}]}}}`)
	ev, e = buildEvidence(c, hook, nil)
	requirements.NoError(e)
	requirements.Len(ev.Content.Transcript.Segments, 1)
	assertions.Nil(ev.Content.Transcript.Segments[0].OffsetSeconds)
	assertions.Nil(ev.Content.Transcript.Segments[0].StartedAt)
}

func TestRetainedPayloadFallbacks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	c, err := parseCall([]byte(`{"call_id":"call-1","queue_status":"complete_error"}`), "")
	requirements.NoError(err)
	hook := jsontext.Value(`{"data":{"call_id":"call-1","payload":{"call_id":"call-1","summary":"Retained summary","corrected_duration":"18","record":true,"recording_url":"https://example.com/recording","transcripts":[{"text":"Retained ordinary transcript","user":"robot","created_at":"2026-10-01T12:00:00Z"}],"warm_transfer_call":{"proxy_agent_calls":[{"call_id":"proxy-1"}]}}}}`)
	effective, err := enrichedCall(c, hook)
	requirements.NoError(err)
	assertions.True(effective.Record)
	assertions.True(effective.ended())
	assertions.InDelta(18.0, *effective.duration(), 0.0001)
	ev, err := buildEvidence(c, hook, nil)
	requirements.NoError(err)
	assertions.Equal("Retained summary", ev.Content.Summary.Text)
	assertions.Contains(ev.Content.Transcript.Text, "Retained ordinary")
	assertions.Equal([]string{"proxy-1"}, ev.ProxyCallIDs)
	c.Raw = jsontext.Value(`{"call_id":"call-1","concatenated_transcript":"Detail fallback"}`)
	ev, err = buildEvidence(c, hook, nil)
	requirements.NoError(err)
	assertions.Contains(ev.Content.Transcript.Text, "Retained ordinary")
	assertions.NotContains(ev.Content.Transcript.Text, "Detail fallback")
	_, err = buildEvidence(c, jsontext.Value(`{"data":{"payload":{"call_id":"another-call"}}}`), nil)
	requirements.ErrorIs(err, ErrInvalidPayload)
}
func TestTerminalStatusesAndMalformedDuration(t *testing.T) {
	assertions := assert.New(t)

	for _, status := range []string{"completed", "failed", "busy", "no-answer", "canceled", "cancelled"} {
		assertions.True((&Call{Status: status}).ended(), status)
	}
	for _, status := range []string{"complete", "complete_error", "pre_queue_error", "queue_error", "call_error"} {
		assertions.True((&Call{QueueStatus: status}).ended(), status)
	}
	assertions.False((&Call{Status: "started", QueueStatus: "call_error"}).ended())
	assertions.False((&Call{Status: "unknown", QueueStatus: "unknown"}).ended())
	length := 1.25
	for _, duration := range []string{"NaN", "-3", "not-duration", "Infinity"} {
		c := &Call{CorrectedDuration: duration, CallLength: &length}
		require.NotNil(t, c.duration())
		assertions.InDelta(75.0, *c.duration(), 0.0001)
	}
}

func TestInvalidRenditionPreservesPriorValidSpeech(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	call, err := parseCall([]byte(`{"call_id":"call-1","completed":true,"transcripts":[{"text":"ordinary fallback","user":"user"}]}`), "")
	requirements.NoError(err)
	valid := jsontext.Value(`{"data":{"payload":{"corrected_transcript":[{"text":"validated correction","speaker_label":"user","start":0,"end":1}],"post_transfer_transcript":[{"text":"validated transfer","speaker_label":"representative","start":0,"end":1}],"transfer_offset_seconds":10}}}`)
	previous, err := buildEvidence(call, valid, nil)
	requirements.NoError(err)
	invalid := jsontext.Value(`{"data":{"payload":{"corrected_transcript":[{"text":"bad correction","start":-1,"end":1}],"post_transfer_transcript":[{"text":"bad transfer","start":-1,"end":1}],"transfer_offset_seconds":20}}}`)
	current, err := buildEvidence(call, invalid, previous)
	requirements.ErrorIs(err, ErrInvalidPayload)
	requirements.NotNil(current)
	assertions.Equal(previous.Content.Transcript, current.Content.Transcript)
	assertions.JSONEq(string(invalid), string(current.PostCall))
	assertions.Equal(previous.Corrected, current.Corrected)
	assertions.Equal(previous.PostTransfer, current.PostTransfer)
	assertions.Equal("invalid_transcript_rendition", current.RenditionError)
}

func TestInvalidEnhancedAndOrdinaryRenditionsKeepPriorOrdinarySpeech(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	call, err := parseCall([]byte(`{"call_id":"call-1","completed":true,"transcripts":[{"user":"user","text":"validated ordinary speech"}]}`), "")
	requirements.NoError(err)
	previous, err := buildEvidence(call, nil, nil)
	requirements.NoError(err)
	omitted, err := parseCall([]byte(`{"call_id":"call-1","completed":true}`), "")
	requirements.NoError(err)
	invalid := jsontext.Value(`{"data":{"payload":{"transcripts":{"invalid":"shape"},"corrected_transcript":[{"text":"bad correction","start":-1,"end":1}]}}}`)
	current, err := buildEvidence(omitted, invalid, previous)
	requirements.ErrorIs(err, ErrInvalidPayload)
	requirements.NotNil(current)
	assertions.Equal(previous.Content.Transcript, current.Content.Transcript)
	assertions.Equal(previous.Original, current.Original)
	assertions.JSONEq(string(invalid), string(current.PostCall))
}

func TestTranscriptWithoutSpokenContentPreservesPriorSpeech(t *testing.T) {
	requirements := require.New(t)

	initial, err := parseCall([]byte(`{"call_id":"call-1","completed":true,"transcripts":[{"text":"Previously archived speech","user":"user"}]}`), "")
	requirements.NoError(err)
	previous, err := buildEvidence(initial, nil, nil)
	requirements.NoError(err)

	wantTranscript := meetingcontent.Transcript{
		State: meetingcontent.StateAvailable,
		Text:  "user: Previously archived speech",
		Segments: []meetingcontent.Segment{{
			Speaker: "user",
			Text:    "Previously archived speech",
		}},
	}

	for _, tc := range []struct {
		name string
		call string
	}{
		{
			name: "action-only transcript",
			call: `{"call_id":"call-1","completed":true,"transcripts":[{"text":"agent action","user":"agent-action"}]}`,
		},
		{
			name: "whitespace-only concatenated transcript",
			call: `{"call_id":"call-1","completed":true,"concatenated_transcript":" \n\t "}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			refreshed, err := parseCall([]byte(tc.call), "")
			require.NoError(t, err)
			current, err := buildEvidence(refreshed, nil, previous)
			require.NoError(t, err)

			assertions.Equal(wantTranscript, current.Content.Transcript)
			assertions.Equal(previous.Original, current.Original)
		})
	}
}

func TestEmptyPostTransferRefreshPreservesPriorTransfer(t *testing.T) {
	requirements := require.New(t)

	call, err := parseCall([]byte(`{"call_id":"call-1","completed":true,"transcripts":[{"text":"Original speech","user":"user"}]}`), "")
	requirements.NoError(err)
	previousHook := jsontext.Value(`{"data":{"payload":{"post_transfer_transcript":[{"text":"Previously transferred speech","speaker_label":"representative","start":0,"end":1}],"transfer_offset_seconds":12}}}`)
	previous, err := buildEvidence(call, previousHook, nil)
	requirements.NoError(err)
	requirements.NotNil(previous.TransferOffset)

	for _, tc := range []struct {
		name string
		hook string
	}{
		{
			name: "action-only rendition",
			hook: `{"data":{"payload":{"post_transfer_transcript":[{"text":"agent action","speaker_label":"agent-action","start":0,"end":1}],"transfer_offset_seconds":99}}}`,
		},
		{
			name: "whitespace-only rendition",
			hook: `{"data":{"payload":{"post_transfer_transcript":" \n\t ","transfer_offset_seconds":99}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			refreshed, err := parseCall([]byte(`{"call_id":"call-1","completed":true,"transcripts":[{"text":"Original speech","user":"user"}]}`), "")
			require.NoError(t, err)
			current, err := buildEvidence(refreshed, jsontext.Value(tc.hook), previous)
			require.NoError(t, err)

			assertions.Equal(previous.Content.Transcript, current.Content.Transcript)
			assertions.Equal(previous.PostTransfer, current.PostTransfer)
			require.NotNil(t, current.TransferOffset)
			assertions.InDelta(*previous.TransferOffset, *current.TransferOffset, 0.0001)
		})
	}
}
