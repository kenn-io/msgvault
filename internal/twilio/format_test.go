package twilio

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotSnippetPreservesUnicode(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	text := strings.Repeat("界", 501)
	snapshot, err := (archivedCall{Call: Call{SID: testCA}, Evidence: Evidence{Transcripts: []Transcript{{Kind: "classic", SourceID: testRE, Complete: true, Usable: true, Segments: []Segment{{Text: text}}}}}}).snapshot(1, "owner@example.com")
	require.NoError(err)
	assert.True(utf8.ValidString(snapshot.Snippet))
	assert.Equal(strings.Repeat("界", 500), snapshot.Snippet)
	assert.Equal(text, snapshot.Body)
}

// An unusable duration archives as zero; an explicit "0" stays zero rather
// than falling back to end minus start.
func TestSnapshotDuration(t *testing.T) {
	for _, call := range []Call{
		{SID: testCA, Duration: "NaN"},
		{SID: testCA, Duration: "Inf"},
		{SID: testCA, Duration: "-1"},
		{SID: testCA, Duration: "invalid"},
		{SID: testCA, StartTime: "2026-10-03T10:00:00Z", EndTime: "2026-10-03T10:05:00Z", Duration: "0"},
	} {
		t.Run(call.Duration, func(t *testing.T) {
			snapshot, err := (archivedCall{Call: call}).snapshot(1, "owner@example.com")
			require.NoError(t, err)
			assert.Contains(t, string(snapshot.Raw), `"duration_seconds":0`)
		})
	}
}

// One transcript per recording drives the text: Intelligence over legacy, then
// the newest retranscription.
func TestCanonicalSegments(t *testing.T) {
	for _, tc := range []struct {
		name        string
		transcripts []Transcript
		recordings  []Recording
		want        []Segment
	}{
		{"intelligence over legacy", []Transcript{
			{Kind: "legacy", ID: "legacy", SourceID: testRE, Complete: true, Usable: true, Segments: []Segment{{Text: "legacy text"}}},
			{Kind: "classic", ID: testGT, SourceID: testRE, Complete: true, Usable: true, Segments: []Segment{{Text: "intelligence text"}}},
		}, nil, []Segment{{Text: "intelligence text"}}},
		{"newest retranscription", []Transcript{
			{Kind: "legacy", ID: "TRbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", SourceID: testRE, DateCreated: "Sat, 03 Oct 2026 10:05:00 +0000", Complete: true, Usable: true, Segments: []Segment{{Text: "obsolete"}}},
			{Kind: "legacy", ID: "TRaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceID: testRE, DateCreated: "Sat, 03 Oct 2026 12:00:00 +0000", Complete: true, Usable: true, Segments: []Segment{{Text: "current"}}},
		}, nil, []Segment{{Text: "current"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segments, complete := canonicalSegments(tc.transcripts, tc.recordings)
			assert.True(t, complete)
			assert.Equal(t, tc.want, segments)
		})
	}
}

// Without call metadata, the meeting starts at the earliest recording, not the
// first by SID, using its creation time when it has no start time. With two
// recordings, the text names them by order, never by SID, and an empty
// speaker adds no separator.
func TestSnapshotStartsAtEarliestRecording(t *testing.T) {
	later, earlier := "REaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "REbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	legacy := func(id, recording, text string) Transcript {
		return Transcript{Kind: "legacy", ID: id, SourceID: recording, Complete: true, Usable: true, Segments: []Segment{{Text: text, Scope: recording}}}
	}
	snapshot, err := (archivedCall{Call: Call{SID: testCA}, Recordings: []Recording{
		{SID: later, StartTime: "Sat, 03 Oct 2026 10:30:00 +0000"},
		{SID: earlier, DateCreated: "Sat, 03 Oct 2026 10:00:00 +0000"},
	}, Evidence: Evidence{Transcripts: []Transcript{legacy("first", later, "after transfer"), legacy("second", earlier, "before transfer")}}}).snapshot(1, "owner@example.com")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), snapshot.StartedAt)
	assert.Equal(t, "Recording 1: before transfer\nRecording 2: after transfer", snapshot.Body)
}

func TestCallTitleNamesTheOtherParty(t *testing.T) {
	for _, tc := range []struct {
		call Call
		want string
	}{
		{Call{Direction: "inbound", From: "+12025550101", To: "+12025550102"}, "Call from +12025550101"},
		{Call{Direction: "outbound-api", From: "+12025550101", To: "+12025550102"}, "Call to +12025550102"},
		{Call{Direction: "inbound"}, "Twilio call"},
		{Call{From: "+12025550101", To: "+12025550102"}, "Twilio call"},
	} {
		assert.Equal(t, tc.want, callTitle(tc.call), tc.call.Direction)
	}
}
