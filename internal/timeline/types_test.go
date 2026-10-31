package timeline

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "update timeline goldens")

func sampleTimeline() *Timeline {
	now := time.Date(2026, 10, 22, 13, 30, 0, 0, time.UTC)
	person := Person{PersonID: 7, ParticipantID: 8, Name: "Alice", Tier: 1, TierSource: "applied"}
	entries := []Entry{
		{Kind: "meeting", Key: "occ:evt:12", Ref: "evt:12", AnchorRef: "evt:12", Time: now, Meeting: &MeetingEntry{Title: UntrustedText{Text: "Weekly sync"}, Start: now, End: now.Add(time.Hour), AttendeeCount: 1, Attendees: []Person{person}, Attended: true, LinkedTranscriptIDs: []int64{21}, PossiblyRelated: []string{"msg:22"}, ActionItemCount: 2}},
		{Kind: "transcript", Key: "msg:21", Ref: "msg:21", AnchorRef: "msg:21", Time: now, Meeting: &MeetingEntry{Title: UntrustedText{Text: "Notes"}, Time: now, PeopleCount: 1, People: []Person{person}, ActionItemCount: 2}},
		{Kind: "burst", Key: "burst:1:2:2026-10-22", Ref: "conv:2", AnchorRef: "burst:1:2:2026-10-22", Time: now, Burst: &BurstEntry{Channel: "email", PeopleCount: 1, People: []Person{person}, Inbound: 2, Outbound: 1, FirstAt: now, LastAt: now, Snippet: UntrustedText{Text: "Hello"}, LatestMessageIDs: []int64{31}, Copies: map[int64]int{31: 2}}},
		{Kind: "message", Key: "msg:31", Ref: "msg:31", AnchorRef: "msg:31", Time: now, Message: &MessageEntry{Direction: "inbound", PeopleCount: 1, People: []Person{person}, Snippet: UntrustedText{Text: "Hello"}}},
		{Kind: "call", Key: "m:41", Ref: "msg:41", AnchorRef: "msg:41", Time: now, Call: &CallEntry{Outcome: "answered", DurationSeconds: new(60.0), PeopleCount: 1, People: []Person{{Name: "Alice", TierSource: "none", Participation: "joined", Credited: true}}}},
		{Kind: "loop", Key: "msg:51", Ref: "msg:51", AnchorRef: "msg:51", Time: now, Loop: &LoopEntry{Kind: "reply_owed", Age: "4d", Person: person}},
		{Kind: "note", Key: "note:61", Ref: "note:61", AnchorRef: "2026-10-22", Time: now, Note: &NoteEntry{Text: UntrustedText{Text: "Follow up"}, PeopleCount: 1, People: []Person{person}}},
	}
	for i := range entries {
		entries[i].Open = []Open{{Tool: "get_message", Args: map[string]any{"id": 31}}}
	}
	entries[3].Anchor = true
	entries[3].Filtered = true
	entries[3].HiddenText = true
	entries[3].Message.Snippet = UntrustedText{}
	return &Timeline{Mode: ModeDay, Timezone: "UTC", TimezoneIsDefault: true, Window: Window{From: "2026-10-21", To: "2026-10-22"}, Now: now,
		Freshness: Freshness{AsOf: now, StaleThreshold: 86400, Sources: []SourceFreshness{{SourceID: new(int64(1)), Source: "mail", Status: "stale", LastRun: "failed", Scheduling: "manual", Warnings: []string{"failed"}, SyncCommand: "msgvault sync"}}, Projections: []ProjectionFreshness{{Name: "activity", Frontier: &now, State: "reconciling"}}, Warnings: []string{"incomplete"}},
		Person:    &PersonHeader{Person: person, Ref: "person:7", Channels: []string{"email"}, FirstAt: &now, LastAt: &now, LastInboundAt: &now, LastOutboundAt: &now, InteractionCount: 3, CadenceStatus: "due", DueAt: &now, InitiatedByContact: 2, InitiatedByOwner: 1, Monthly: [12]int{1, 2, 3}, Brief: &UntrustedText{Text: "Talked about plans"}, Agenda: []UntrustedText{{Text: "Review"}}},
		Anchor:    &Anchor{Ref: "msg:31", Kind: "message", Time: now, Filtered: true}, Candidates: []Candidate{{PersonID: 7, PersonUID: "synthetic-7", ParticipantIDs: []int64{8}, DisplayName: "Alice", Address: "alice@example.com"}},
		OpenLoops: &OpenLoops{Items: []Entry{entries[5]}, Truncated: true, CoverageIncomplete: true, SkippedUnparseable: 1}, Upcoming: []Entry{entries[0]}, Days: []Day{{Date: "2026-10-22", Meetings: entries[:2], Bursts: entries[2:4], Calls: entries[4:5], Notes: entries[6:]}, {Date: "2026-10-20", Summary: &DaySummary{Meetings: 1, PeopleCount: 1, People: []Person{person}}}}, History: entries,
		Gaps: []Gap{{Section: "days", Code: "partial", Retryable: true, Items: []string{"evt:99"}}}, Omitted: Omitted{Budget: map[string]any{"history": 2, "open_loops": "more"}, Noise: 2, Cold: 3}, Cursor: "opaque"}
}
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := "testdata/" + name
	if !bytes.HasSuffix(got, []byte{'\n'}) {
		got = append(got, '\n')
	}
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}
func TestTimelineJSONGolden(t *testing.T) {
	b, err := json.Marshal(sampleTimeline(), json.Deterministic(true), jsontext.Multiline(true))
	require.NoError(t, err)
	golden(t, "timeline_full.json", b)
}
func TestEntryValidateExactlyOnePayload(t *testing.T) {
	requirements := require.New(t)
	for _, e := range sampleTimeline().History {
		requirements.NoError(e.Validate())
		e.Note = &NoteEntry{}
		if e.Kind == "note" {
			e.Call = &CallEntry{}
		}
		requirements.ErrorIs(e.Validate(), ErrInvalidRequest)
	}
	requirements.ErrorIs((Entry{Kind: "message"}).Validate(), ErrInvalidRequest)
	requirements.ErrorIs((Entry{Kind: "unknown", Message: &MessageEntry{}}).Validate(), ErrInvalidRequest)
}
func TestEntryAnchorRefPerKind(t *testing.T) {
	for _, e := range sampleTimeline().History {
		require.NoError(t, e.Validate())
		e.AnchorRef = "conv:2"
		require.ErrorIs(t, e.Validate(), ErrInvalidRequest)
	}
}
func TestEntryValidateRejectsMalformedAnchor(t *testing.T) {
	for _, e := range []Entry{{Kind: "meeting", Ref: "evt:garbage", AnchorRef: "evt:garbage", Meeting: &MeetingEntry{}}, {Kind: "message", Ref: "msg:0", AnchorRef: "msg:0", Message: &MessageEntry{}}, {Kind: "burst", Key: "burst:1:2:bad", AnchorRef: "burst:1:2:bad", Burst: &BurstEntry{}}} {
		require.ErrorIs(t, e.Validate(), ErrInvalidRequest)
	}
}
