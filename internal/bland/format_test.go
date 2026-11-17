package bland

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

// Which transcript rendition wins, and how transfer timing applies to it.
func TestCanonicalRetainedTransfer(t *testing.T) {
	for _, tc := range []struct {
		name, call, hook string
		check            func(*assert.Assertions, *require.Assertions, *Call, *Evidence)
	}{
		{
			name: "corrected and transferred speech",
			call: `{"call_id":"call-1","started_at":"2026-10-01T12:00:00Z","end_at":"2026-10-01T15:00:00Z","corrected_duration":"20","from":"+12025550100","to":"+12025550101","transcripts":[{"text":"raw duplicate","user":"user"},{"text":"internal action","user":"agent-action"}]}`,
			hook: `{"data":{"payload":{"corrected_transcript":[{"text":"Corrected speech","speaker_label":"user","start":1,"end":2}],"transfer_offset_seconds":9.573,"post_transfer_transcript":[{"text":"Transferred speech","speaker_label":"representative","start":9.544,"end":17.804}],"live_translation_transcript":[{"text":"translation variant"}]}}}`,
			check: func(assertions *assert.Assertions, requirements *require.Assertions, c *Call, ev *Evidence) {
				assertions.Equal(meetingcontent.StateAvailable, ev.Content.Transcript.State)
				requirements.Len(ev.Content.Transcript.Segments, 2)
				assertions.InDelta(19.117, *ev.Content.Transcript.Segments[1].OffsetSeconds, 0.0001)
				assertions.NotContains(ev.Content.Transcript.Text, "duplicate")
				assertions.NotContains(ev.Content.Transcript.Text, "internal action")
				assertions.NotContains(ev.Content.Transcript.Text, "translation variant")
				assertions.InDelta(20.0, *ev.Content.DurationSeconds, 0.0001)
				assertions.Equal("+12025550101", ev.Content.SourceParticipants[0].Phone)
				ev2, err := evidenceFor(c, nil, ev)
				requirements.NoError(err)
				assertions.Equal(ev.Content.Transcript, ev2.Content.Transcript)
				raw, err := marshalEvidence(ev)
				requirements.NoError(err)
				assertions.Equal(ev.Content, meetingcontent.Decode(RawFormat, raw, nil))
			},
		},
		{
			name: "transfer without offset has no timing",
			call: `{"call_id":"recording-only","completed":true,"record":true}`,
			hook: `{"data":{"payload":{"post_transfer_transcript":[{"text":"Transfer","speaker_label":"user","start":2,"end":3}]}}}`,
			check: func(assertions *assert.Assertions, requirements *require.Assertions, _ *Call, ev *Evidence) {
				requirements.Len(ev.Content.Transcript.Segments, 1)
				assertions.Nil(ev.Content.Transcript.Segments[0].OffsetSeconds)
				assertions.Nil(ev.Content.Transcript.Segments[0].StartedAt)
			},
		},
		{
			name: "postcall transcript beats the details' concatenated text",
			call: `{"call_id":"call-1","concatenated_transcript":"Detail fallback"}`,
			hook: `{"data":{"call_id":"call-1","payload":{"call_id":"call-1","transcripts":[{"text":"Retained ordinary transcript","user":"robot"}]}}}`,
			check: func(assertions *assert.Assertions, _ *require.Assertions, _ *Call, ev *Evidence) {
				assertions.Contains(ev.Content.Transcript.Text, "Retained ordinary")
				assertions.NotContains(ev.Content.Transcript.Text, "Detail fallback")
			},
		},
		{
			name: "concatenated text beside an empty transcripts list",
			call: `{"call_id":"call-1","completed":true,"transcripts":[ ],"concatenated_transcript":"concatenated speech"}`,
			check: func(assertions *assert.Assertions, _ *require.Assertions, _ *Call, ev *Evidence) {
				assertions.Equal("concatenated speech", ev.Content.Transcript.Text)
			},
		},
		{
			name: "concatenated text beside action-only transcripts",
			call: `{"call_id":"call-1","completed":true,"transcripts":[{"text":"agent action","user":"agent-action"}],"concatenated_transcript":"concatenated speech"}`,
			check: func(assertions *assert.Assertions, _ *require.Assertions, _ *Call, ev *Evidence) {
				assertions.Equal("concatenated speech", ev.Content.Transcript.Text)
			},
		},
		{
			name: "postcall concatenated text when the details have none",
			call: `{"call_id":"call-1","completed":true}`,
			hook: `{"data":{"call_id":"call-1","payload":{"call_id":"call-1","concatenated_transcript":"archive orchard phrase"}}}`,
			check: func(assertions *assert.Assertions, _ *require.Assertions, _ *Call, ev *Evidence) {
				assertions.Equal("archive orchard phrase", ev.Content.Transcript.Text)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			c, err := decodeCall([]byte(tc.call), "", false)
			requirements.NoError(err)
			ev, err := evidenceFor(c, jsontext.Value(tc.hook), nil)
			requirements.NoError(err)
			tc.check(assertions, requirements, c, ev)
		})
	}
}

// Every malformed field fails the response's decode by name, so nothing past
// the client sees it; the well-formed edge cases decode.
func TestDecodeRejectsMalformed(t *testing.T) {
	const at = `"created_at":"2026-10-01T11:00:00Z"`
	for _, tc := range []struct {
		name, kind, body, field string
		// want, when field is empty, is the decode's error.
		want error
	}{
		{"c_id alone names the call", "details", `{"c_id":"call-1",` + at + `}`, "", nil},
		{"null data with coded errors is absent", "postcall", `{"data":null,"errors":[{"error":"NOT_FOUND","message":"No postcall webhook data"}]}`, "", ErrNotFound},
		{"c_id disagrees", "details", `{"call_id":"call-1","c_id":"call-2",` + at + `}`, "c_id", nil},
		{"another call", "details", `{"call_id":"call-2",` + at + `}`, "call_id", nil},
		{"unreadable time", "details", `{"call_id":"call-1","created_at":"yesterday"}`, "created_at", nil},
		{"undated", "details", `{"call_id":"call-1"}`, "created_at", nil},
		{"unreadable duration", "details", `{"call_id":"call-1",` + at + `,"corrected_duration":"NaN"}`, "corrected_duration", nil},
		{"duration not a number", "details", `{"call_id":"call-1",` + at + `,"call_length":"bad"}`, "call_length", nil},
		{"duration overflows", "details", `{"call_id":"call-1",` + at + `,"call_length":1e308}`, "call_length", nil},
		{"flag not a bool", "details", `{"call_id":"call-1",` + at + `,"completed":"yes"}`, "completed", nil},
		{"transcripts not a list", "details", `{"call_id":"call-1",` + at + `,"transcripts":{"invalid":"shape"}}`, "transcripts", nil},
		{"null entry", "details", `{"call_id":"call-1",` + at + `,"transcripts":[null]}`, "transcripts", nil},
		{"entry text not a string", "details", `{"call_id":"call-1",` + at + `,"transcripts":[{"text":1}]}`, "transcripts", nil},
		{"speaker an object", "details", `{"call_id":"call-1",` + at + `,"transcripts":[{"text":"hi","speaker":{}}]}`, "transcripts", nil},
		{"unreadable entry time", "details", `{"call_id":"call-1",` + at + `,"transcripts":[{"text":"hi","created_at":"soon"}]}`, "transcripts", nil},
		{"concatenated not text", "details", `{"call_id":"call-1",` + at + `,"concatenated_transcript":[1]}`, "concatenated_transcript", nil},
		{"negative timing", "details", `{"call_id":"call-1",` + at + `,"corrected_transcript":[{"text":"hi","start":-1,"end":1}]}`, "corrected_transcript", nil},
		{"negative transfer offset", "details", `{"call_id":"call-1",` + at + `,"transfer_offset_seconds":-1}`, "transfer_offset_seconds", nil},
		{"transfer offset overflows", "details", `{"call_id":"call-1",` + at + `,"transfer_offset_seconds":1e308,"post_transfer_transcript":[{"text":"hi","start":1e308}]}`, "post_transfer_transcript", nil},
		{"listed IDs disagree", "listed", `{"call_id":"call-1","c_id":"call-2"}`, "call_id", nil},
		{"empty postcall", "postcall", `{}`, "data", nil},
		{"uncoded postcall error", "postcall", `{"data":null,"errors":[null]}`, "errors", nil},
		{"postcall for another call", "postcall", `{"data":{"call_id":"call-2","payload":{}}}`, "data.call_id", nil},
		{"payload not an object", "postcall", `{"data":{"payload":"not an object"}}`, "payload", nil},
		{"payload for another call", "postcall", `{"data":{"payload":{"call_id":"call-2"}}}`, "payload.call_id", nil},
		{"payload field", "postcall", `{"data":{"payload":{"call_length":"bad"}}}`, "call_length", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			switch tc.kind {
			case "details":
				_, err = decodeCall([]byte(tc.body), "call-1", true)
			case "listed":
				_, err = decodeListed([]byte(tc.body))
			default:
				_, err = parsePostCall([]byte(tc.body), "call-1")
			}
			requirements := require.New(t)
			if tc.field == "" {
				if tc.want == nil {
					requirements.NoError(err)
				} else {
					requirements.ErrorIs(err, tc.want)
				}
				return
			}
			requirements.ErrorIs(err, ErrInvalidPayload)
			requirements.ErrorContains(err, tc.field)
		})
	}
}

// A refresh with less than was archived keeps the prior speech and call
// metadata: an empty rendition, a sparse details response, or a
// postcall payload that disappears or loses fields.
func TestRefreshKeepsPriorEvidence(t *testing.T) {
	const full = `{"call_id":"call-1","completed":true,"created_at":"2026-10-01T11:00:00Z","started_at":"2026-10-01T11:00:00Z","from":"+12025550100","to":"+12025550101","summary":"retained summary"}`
	const sparse = `{"call_id":"call-1","completed":true}`
	const ordinary = `{"call_id":"call-1","completed":true,"transcripts":[{"text":"Original speech","user":"user"}]}`
	const transferred = `{"data":{"payload":{"post_transfer_transcript":[{"text":"Previously transferred speech","speaker_label":"representative","start":0,"end":1}],"transfer_offset_seconds":12}}}`
	for _, tc := range []struct {
		name, call, previousHook string
		refreshCall, refreshHook string
		refreshes                int
		// summary, when set, checks the call metadata instead of the speech.
		summary string
	}{
		{name: "action-only transcript", call: ordinary, refreshCall: `{"call_id":"call-1","completed":true,"transcripts":[{"text":"agent action","user":"agent-action"}]}`},
		{name: "whitespace-only concatenated transcript", call: ordinary, refreshCall: `{"call_id":"call-1","completed":true,"concatenated_transcript":" \n\t "}`},
		{name: "action-only transfer", call: ordinary, previousHook: transferred, refreshCall: ordinary, refreshHook: `{"data":{"payload":{"post_transfer_transcript":[{"text":"agent action","speaker_label":"agent-action","start":0,"end":1}],"transfer_offset_seconds":99}}}`},
		{name: "whitespace-only transfer", call: ordinary, previousHook: transferred, refreshCall: ordinary, refreshHook: `{"data":{"payload":{"post_transfer_transcript":" \n\t ","transfer_offset_seconds":99}}}`},
		{name: "postcall disappears", call: sparse, previousHook: `{"data":{"payload":` + full + `}}`, refreshCall: sparse, summary: "retained summary"},
		{name: "details go sparse twice", call: full, refreshCall: sparse, refreshes: 2, summary: "retained summary"},
		{name: "postcall loses fields", call: sparse, previousHook: `{"data":{"payload":` + full + `}}`, refreshCall: sparse, refreshHook: `{"data":{"payload":{"call_id":"call-1","summary":"updated summary"}}}`, summary: "updated summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			call, err := decodeCall([]byte(tc.call), "", false)
			requirements.NoError(err)
			previous, err := evidenceFor(call, jsontext.Value(tc.previousHook), nil)
			requirements.NoError(err)
			current := previous
			for range max(tc.refreshes, 1) {
				refreshed, err := decodeCall([]byte(tc.refreshCall), "", false)
				requirements.NoError(err)
				current, err = evidenceFor(refreshed, jsontext.Value(tc.refreshHook), current)
				requirements.NoError(err)
			}
			if tc.summary != "" {
				effective, err := decodeCall(current.EffectiveCall, "", false)
				requirements.NoError(err)
				assertions.True(effective.started().Equal(time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)))
				requirements.NotEmpty(current.Content.SourceParticipants)
				assertions.Equal("+12025550101", current.Content.SourceParticipants[0].Phone)
				assertions.Equal(tc.summary, current.Content.Summary.Text)
				return
			}
			assertions.Equal(previous.Content.Transcript, current.Content.Transcript)
			assertions.Equal(previous.Original, current.Original)
			if tc.previousHook != "" {
				assertions.Equal(previous.Corrected, current.Corrected)
				assertions.Equal(previous.PostTransfer, current.PostTransfer)
				assertions.Equal(previous.TransferOffset, current.TransferOffset)
			}
		})
	}
}

// evidenceFor builds c's evidence the way archiveCall does.
func evidenceFor(c *Call, hook jsontext.Value, previous *Evidence) (*Evidence, error) {
	var parsed *PostCall
	if len(hook) > 0 {
		var err error
		if parsed, err = parsePostCall(hook, ""); err != nil {
			return nil, err
		}
	}
	ev, _, err := buildEvidence(c, parsed, previous, "")
	return ev, err
}
