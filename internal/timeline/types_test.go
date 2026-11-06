package timeline

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "update timeline goldens")

func sampleTimeline(mode Mode) *Timeline {
	now := time.Date(2026, 10, 22, 13, 30, 0, 0, time.UTC)
	person := Person{PersonID: 7, ParticipantID: 8, Name: "Alice", Tier: 1, TierSource: "applied"}
	entries := []Entry{
		{Kind: "meeting", Key: "occ:evt:12", Ref: "evt:12", AnchorRef: "evt:12", Time: now, Meeting: &MeetingEntry{Title: UntrustedText{Text: "Weekly sync"}, Start: now, End: now.Add(time.Hour), AttendeeCount: 1, Attendees: []Person{person}, PeopleCount: 1, People: []Person{person}, Attended: true, LinkedTranscriptIDs: []int64{21}, PossiblyRelated: []string{"msg:23"}, ActionItemCount: 2}},
		{Kind: "transcript", Key: "msg:22", Ref: "msg:22", AnchorRef: "msg:22", Time: now, Meeting: &MeetingEntry{Title: UntrustedText{Text: "Notes"}, Time: now, PeopleCount: 1, People: []Person{person}, ActionItemCount: 2}},
		{Kind: "burst", Key: "burst:1:2:2026-10-22", Ref: "conv:2", AnchorRef: "burst:1:2:2026-10-22", Time: now, Burst: &BurstEntry{Channel: "email", PeopleCount: 1, People: []Person{person}, Inbound: 2, Outbound: 1, FirstAt: now, LastAt: now, Snippet: UntrustedText{Text: "Hello"}, LatestMessageIDs: []int64{31}, Copies: map[int64]int{31: 2}}},
		{Kind: "message", Key: "msg:32", Ref: "msg:32", AnchorRef: "msg:32", Time: now, Message: &MessageEntry{Direction: "inbound", PeopleCount: 1, People: []Person{person}, Snippet: UntrustedText{Text: "Hello"}}},
		{Kind: "call", Key: "m:41", Ref: "msg:41", AnchorRef: "msg:41", Time: now, Call: &CallEntry{Outcome: "answered", DurationSeconds: new(60.0), PeopleCount: 1, People: []Person{{Name: "Alice", TierSource: "none", Participation: "joined", Credited: true}}}},
		{Kind: "loop", Key: "msg:51", Ref: "msg:51", AnchorRef: "msg:51", Time: now.AddDate(0, 0, -4), Loop: &LoopEntry{Kind: "reply_owed", Age: "4d", Person: person}},
		{Kind: "note", Key: "note:61", Ref: "2026-10-22", AnchorRef: "2026-10-22", Time: now, Note: &NoteEntry{Text: UntrustedText{Text: "Follow up"}, PeopleCount: 1, People: []Person{person}}},
	}
	entries[0].Open = []Open{{Tool: "get_calendar_event", Args: map[string]any{"ref": "evt:12"}}, {Tool: "get_meeting_context", Args: map[string]any{"message_ids": []int64{21}}}}
	entries[1].Open = []Open{{Tool: "get_meeting_context", Args: map[string]any{"message_ids": []int64{22}}}}
	entries[2].Open = []Open{{Tool: "list_messages", Args: map[string]any{"conversation_id": 2}}, {Tool: "get_message", Args: map[string]any{"id": 31}}}
	entries[3].Open = []Open{{Tool: "get_message", Args: map[string]any{"id": 32}}}
	entries[4].Open = []Open{{Tool: "get_message", Args: map[string]any{"id": 41}}}
	entries[5].Open = []Open{{Tool: "get_message", Args: map[string]any{"id": 51}}}
	entries[6].Open = []Open{}
	entries[3].HiddenText = true
	entries[3].Message = &MessageEntry{People: []Person{}}
	v := &Timeline{Mode: mode, Timezone: "UTC", TimezoneIsDefault: true, Window: Window{From: "2026-10-20", To: "2026-10-22"}, Now: now,
		Freshness: Freshness{AsOf: now, StaleThreshold: 86400, Sources: []SourceFreshness{{SourceID: new(int64(1)), SourceType: "gmail", Identifier: "owner@example.com", Reason: "stale", Outcome: "failed", Scheduling: "scheduled", SyncCommand: "msgvault sync"}}, Projections: []ProjectionFreshness{{Name: "activity", Frontier: &now, State: "reconciling"}}, Warnings: []string{"The latest sync failed for gmail source owner@example.com."}},
		Gaps:      []Gap{{Section: "meetings", Code: "partial", Retryable: true, Items: []string{"evt:99"}}}, Omitted: Omitted{Budget: map[string]any{"history": 2, "open_loops": "more"}, Noise: 2, Cold: 3}, Cursor: "opaque"}
	if mode == ModeAnchor {
		entries[3].Anchor, entries[3].Filtered = true, true
		v.Anchor = &Anchor{Ref: "msg:32", Kind: "message", Time: now, Filtered: true}
		v.History = []Entry{entries[0], entries[1], entries[2], entries[3], entries[4], entries[6]}
		return v
	}
	v.OpenLoops = &OpenLoops{Items: []Entry{entries[5]}, Truncated: true, CoverageIncomplete: true, SkippedUnparseable: 1}
	future := entries[0]
	future.Key, future.Ref, future.AnchorRef = "occ:evt:13", "evt:13", "evt:13"
	future.Time = now.Add(time.Hour)
	future.Meeting = &MeetingEntry{Title: UntrustedText{Text: "Tomorrow's plans"}, Start: future.Time, End: future.Time.Add(time.Hour), Attendees: []Person{person}, AttendeeCount: 1, People: []Person{person}, PeopleCount: 1}
	future.Open = []Open{{Tool: "get_calendar_event", Args: map[string]any{"ref": "evt:13"}}}
	v.Upcoming = []Entry{future}
	if mode == ModeRelationship {
		v.Person = &PersonHeader{Person: person, Ref: "person:7", Channels: []string{"email"}, FirstAt: &now, LastAt: &now, LastInboundAt: &now, LastOutboundAt: &now, InteractionCount: 3, CadenceStatus: "due", DueAt: &now, InitiatedByContact: 2, InitiatedByOwner: 1, Monthly: [12]int{11: 3}, Brief: &UntrustedText{Text: "Talked about plans"}, Agenda: []UntrustedText{{Text: "Review"}}}
		v.History = []Entry{entries[0], entries[1], entries[2], entries[4]}
	} else {
		v.Omitted.Budget = map[string]any{"days": 2, "open_loops": "more"}
		v.Days = []Day{{Date: "2026-10-22", Meetings: entries[:2], Bursts: entries[2:3], Calls: entries[4:5], Notes: entries[6:]}, {Date: "2026-10-20", Summary: &DaySummary{Meetings: 1, PeopleCount: 1, People: []Person{person}}}}
	}
	return v
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
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), string(got))
}
func TestTimelineJSONGolden(t *testing.T) {
	b, err := json.Marshal(sampleTimeline(ModeDay), json.Deterministic(true), jsontext.Multiline(true))
	require.NoError(t, err)
	golden(t, "timeline_full.json", b)
}
