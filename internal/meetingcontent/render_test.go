package meetingcontent

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSummaryOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Decode("meeting_json",
		[]byte(`{"summary_markdown":"Decision","transcript":"TRANSCRIPT_SENTINEL","action_items":[]}`), nil)
	requirements.Equal(StateAvailable, content.Summary.State)
	requirements.Equal(CoverageAvailable, content.ActionCoverage)
	result, err := Render("synthetic-archive", []Entry{{
		Meeting: MeetingRef{MessageID: 12, ConversationID: 8, SourceID: 2,
			SourceType: "meeting_import", SourceIdentifier: "example",
			SourceMessageID: "meeting:weekly", Title: "Weekly",
			ArchivePath: "/api/v1/messages/12"},
		Participants: []Participant{}, Content: content,
	}}, PacketOptions{Format: FormatJSON, MaxBytes: 4096})
	requirements.NoError(err)

	want := `{"schema_version":1,"archive_uid":"synthetic-archive","requested_message_ids":[12],"meetings":[{"meeting":{"message_id":12,"conversation_id":8,"source_id":2,"source_type":"meeting_import","source_identifier":"example","source_message_id":"meeting:weekly","title":"Weekly","occurred_at":null,"archive_path":"/api/v1/messages/12"},"participants":[],"content":{"summary":{"state":"available","text":"Decision"},"notes":{"state":"unsupported"},"transcript":{"state":"omitted_by_request"},"actions":[],"action_coverage":"available","duration_seconds":null}}],"omitted_message_ids":[],"truncated":false,"truncations":[]}`
	assertions.Equal(want, result.Content)
	assertions.Equal(len([]byte(result.Content)), result.ContentBytes)
	assertions.LessOrEqual(result.ContentBytes, 4096)
	assertions.False(result.Truncated)
	assertions.Empty(result.OmittedMessageIDs)
	assertions.NotContains(result.Content, "TRANSCRIPT_SENTINEL")
	assertions.Contains(result.Content, "omitted_by_request")
}

func TestRenderDeterministicallySortsAndMergesParticipants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	occurred := mustRenderTime(t, "2026-09-12T10:00:00Z")
	entries := []Entry{
		{
			Meeting:      MeetingRef{MessageID: 9, Title: "Undated", ArchivePath: "/api/v1/messages/9"},
			Participants: []Participant{{Name: "Name only archive", Role: "to"}},
			Content: Content{
				Summary: Section{State: StateAvailable, Text: "Later"}, Notes: Section{State: StateEmpty},
				Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageAvailable,
				SourceParticipants: []Participant{{Name: "Name only source", Role: "to"}},
			},
		},
		{
			Meeting: MeetingRef{MessageID: 3, Title: "Café <review>", OccurredAt: occurred, ArchivePath: "/api/v1/messages/3"},
			Participants: []Participant{
				{ParticipantID: new(int64(7)), Name: "Morgan Archive", Email: "MORGAN@example.com", Role: "from"},
				{ParticipantID: new(int64(8)), Name: "Zed", Email: "zed@example.com", Role: "to"},
			},
			Content: Content{
				Summary: Section{State: StateAvailable, Text: "Résumé & decision"}, Notes: Section{State: StateUnsupported},
				Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageUnsupported,
				ActionReason: "no_structured_actions",
				SourceParticipants: []Participant{
					{Name: "Morgan Source", Email: "morgan@example.com", Role: "from"},
					{Name: "Alpha", Email: "alpha@example.com", Role: "to"},
				},
			},
		},
	}
	options := PacketOptions{Format: FormatJSON, MaxBytes: 16384}

	first, err := Render("archive-é", entries, options)
	requirements.NoError(err)
	second, err := Render("archive-é", entries, options)
	requirements.NoError(err)
	assertions.Equal(first, second)
	assertions.Contains(first.Content, "archive-é")
	assertions.Contains(first.Content, "Café")
	assertions.Contains(first.Content, `\u003creview\u003e`)
	assertions.NotContains(first.Content, "source_participants")

	var packet Packet
	requirements.NoError(json.Unmarshal([]byte(first.Content), &packet))
	requirements.Len(packet.Meetings, 2)
	assertions.Equal([]int64{3, 9}, packet.RequestedMessageIDs)
	assertions.Equal(int64(3), packet.Meetings[0].Meeting.MessageID)
	assertions.Equal(int64(9), packet.Meetings[1].Meeting.MessageID)
	assertions.Equal([]Participant{
		{ParticipantID: new(int64(7)), Name: "Morgan Archive", Email: "MORGAN@example.com", Role: "from"},
		{Name: "Alpha", Email: "alpha@example.com", Role: "to"},
		{ParticipantID: new(int64(8)), Name: "Zed", Email: "zed@example.com", Role: "to"},
	}, packet.Meetings[0].Participants)
	assertions.Equal([]Participant{
		{Name: "Name only archive", Role: "to"},
		{Name: "Name only source", Role: "to"},
	}, packet.Meetings[1].Participants)
}

func TestRenderTranscriptRequiresOptIn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Decode("meeting_json", []byte(`{
		"summary_markdown":"Summary",
		"transcript_segments":[
			{"speaker":"Morgan Example","text":"TRANSCRIPT_SENTINEL","offset_seconds":0},
			{"speaker":"Riley Example","text":"Second utterance","offset_seconds":4.5}
		],
		"action_items":[]
	}`), nil)
	entry := renderEntry(21, "Transcript", content)

	omitted, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: 8192})
	requirements.NoError(err)
	assertions.NotContains(omitted.Content, "TRANSCRIPT_SENTINEL")
	assertions.Contains(omitted.Content, "omitted_by_request")

	included, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, IncludeTranscript: true, MaxBytes: 8192})
	requirements.NoError(err)
	assertions.Contains(included.Content, "TRANSCRIPT_SENTINEL")
	assertions.Contains(included.Content, `"offset_seconds":4.5`)
	assertions.NotContains(included.Content, "omitted_by_request")
}

func TestRenderHonorsExactUnicodeJSONBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	summary := strings.Repeat("Résumé <alpha> 🙂 ", 20) + "suffix"
	content := Decode("meeting_json", []byte(`{"summary_markdown":`+strconv.Quote(summary)+`,"action_items":[]}`), nil)
	entry := renderEntry(31, "Unicode", content)
	full, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: 8192})
	requirements.NoError(err)
	assertions.Contains(full.Content, `\u003calpha\u003e`)

	exact, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: full.ContentBytes})
	requirements.NoError(err)
	assertions.Equal(full.Content, exact.Content)
	assertions.Equal(len([]byte(exact.Content)), exact.ContentBytes)

	bounded, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: full.ContentBytes - 1})
	requirements.NoError(err)
	assertions.LessOrEqual(bounded.ContentBytes, full.ContentBytes-1)
	assertions.True(bounded.Truncated)
	assertions.True(utf8.ValidString(bounded.Content))
	assertions.True(json.Valid([]byte(bounded.Content)))
	assertions.NotEmpty(packetFromJSON(t, bounded.Content).Truncations)
}

func TestRenderKeepsActionRecordsWhole(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Content{
		Summary: Section{State: StateEmpty}, Notes: Section{State: StateUnsupported},
		Transcript: Transcript{State: StateEmpty}, ActionCoverage: CoverageAvailable,
		Actions: []Action{
			{Ordinal: 0, SourceID: "action-0", Title: strings.Repeat("First action ", 8), Status: StatusPending, Origin: "structured", Locator: "action_items[0]"},
			{Ordinal: 1, SourceID: "action-1", Title: strings.Repeat("Second action ", 8), Status: StatusCompleted, Origin: "structured", Locator: "action_items[1]"},
		},
	}
	entry := renderEntry(41, "Actions", content)
	full, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: 8192})
	requirements.NoError(err)

	var partial *PacketResult
	for budget := full.ContentBytes - 1; budget > 0; budget-- {
		candidate, renderErr := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: budget})
		if errors.Is(renderErr, ErrBudgetTooSmall) {
			break
		}
		requirements.NoError(renderErr)
		packet := packetFromJSON(t, candidate.Content)
		if len(packet.Meetings) == 1 && len(packet.Meetings[0].Content.Actions) == 1 {
			partial = candidate
			break
		}
	}
	requirements.NotNil(partial, "a useful budget must retain one whole action")
	packet := packetFromJSON(t, partial.Content)
	requirements.Len(packet.Meetings[0].Content.Actions, 1)
	assertions.Equal(content.Actions[0], packet.Meetings[0].Content.Actions[0])
	requirements.Len(packet.Truncations, 1)
	assertions.Equal("actions", packet.Truncations[0].Section)
	assertions.Equal(1, packet.Truncations[0].OmittedItems)
}

func TestRenderShortensFinalTranscriptSegmentWithoutLosingTiming(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Content{
		Summary: Section{State: StateEmpty}, Notes: Section{State: StateUnsupported},
		Transcript: Transcript{State: StateAvailable, Segments: []Segment{
			{Speaker: "Morgan Example", Text: strings.Repeat("spoken evidence ", 80), OffsetSeconds: new(12.5)},
		}},
		Actions: []Action{}, ActionCoverage: CoverageAvailable,
	}
	entry := renderEntry(45, "Transcript truncation", content)
	full, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, IncludeTranscript: true, MaxBytes: 16384})
	requirements.NoError(err)

	var partial Packet
	for budget := full.ContentBytes - 1; budget > 0; budget-- {
		candidate, renderErr := Render("archive", []Entry{entry}, PacketOptions{Format: FormatJSON, IncludeTranscript: true, MaxBytes: budget})
		if errors.Is(renderErr, ErrBudgetTooSmall) {
			break
		}
		requirements.NoError(renderErr)
		packet := packetFromJSON(t, candidate.Content)
		if len(packet.Meetings) == 1 && len(packet.Meetings[0].Content.Transcript.Segments) == 1 &&
			len(packet.Meetings[0].Content.Transcript.Segments[0].Text) < len(content.Transcript.Segments[0].Text) {
			partial = packet
			break
		}
	}
	requirements.Len(partial.Meetings, 1)
	requirements.Len(partial.Meetings[0].Content.Transcript.Segments, 1)
	segment := partial.Meetings[0].Content.Transcript.Segments[0]
	assertions.Equal("Morgan Example", segment.Speaker)
	assertions.Equal(new(12.5), segment.OffsetSeconds)
	assertions.NotEmpty(segment.Text)
	assertions.True(strings.HasPrefix(content.Transcript.Segments[0].Text, segment.Text))
	assertions.Equal(StateTruncated, partial.Meetings[0].Content.Transcript.State)
}

func TestRenderOmitsOversizedProvenanceAndKeepsLaterMeeting(t *testing.T) {
	assertions := assert.New(t)
	huge := renderEntry(51, strings.Repeat("provenance", 500), Content{
		Summary: Section{State: StateEmpty}, Notes: Section{State: StateEmpty},
		Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageAvailable,
	})
	small := renderEntry(52, "Small", Content{
		Summary: Section{State: StateEmpty}, Notes: Section{State: StateEmpty},
		Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageAvailable,
	})

	var got *PacketResult
	for budget := 512; budget <= 2048; budget++ {
		candidate, err := Render("archive", []Entry{huge, small}, PacketOptions{Format: FormatJSON, MaxBytes: budget})
		if err != nil {
			continue
		}
		packet := packetFromJSON(t, candidate.Content)
		if len(packet.Meetings) == 1 && packet.Meetings[0].Meeting.MessageID == 52 {
			got = candidate
			break
		}
	}
	require.NotNil(t, got)
	assertions.Equal([]int64{51}, got.OmittedMessageIDs)
	assertions.True(got.Truncated)
	assertions.NotContains(got.Content, strings.Repeat("provenance", 20))
}

func TestRenderMarkdownIsDeterministicAndCitesArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Decode("meeting_json", []byte(`{
		"summary_markdown":"Décision <keep user link https://example.test>",
		"transcript":"TRANSCRIPT_SENTINEL",
		"action_items":[{"source_id":"a-1","title":"Send résumé","status":"open"}]
	}`), []byte(`{"recording_url":"METADATA_SENTINEL"}`))
	entry := renderEntry(61, "Weekly café", content)
	options := PacketOptions{Format: FormatMarkdown, MaxBytes: 8192}

	first, err := Render("archive-é", []Entry{entry}, options)
	requirements.NoError(err)
	second, err := Render("archive-é", []Entry{entry}, options)
	requirements.NoError(err)
	assertions.Equal(first, second)
	assertions.Equal(len([]byte(first.Content)), first.ContentBytes)
	assertions.Contains(first.Content, "Archive UID: `archive-é`")
	assertions.Contains(first.Content, "Requested message IDs: 61")
	assertions.Contains(first.Content, "Omitted message IDs: none")
	assertions.Contains(first.Content, "[Archived meeting](/api/v1/messages/61)")
	assertions.Contains(first.Content, "Action coverage: available")
	assertions.Contains(first.Content, "Décision <keep user link https://example.test>")
	assertions.Contains(first.Content, "Send résumé")
	assertions.NotContains(first.Content, "TRANSCRIPT_SENTINEL")
	assertions.NotContains(first.Content, "METADATA_SENTINEL")
}

func TestRenderRejectsImpossibleMinimalBudget(t *testing.T) {
	result, err := Render("archive", nil, PacketOptions{Format: FormatJSON, MaxBytes: 1})
	assert.Nil(t, result)
	require.ErrorIs(t, err, ErrBudgetTooSmall)
}

func renderEntry(messageID int64, title string, content Content) Entry {
	return Entry{
		Meeting: MeetingRef{
			MessageID: messageID, ConversationID: messageID + 100, SourceID: 2,
			SourceType: "meeting_import", SourceIdentifier: "example",
			SourceMessageID: "meeting:" + title, Title: title,
			ArchivePath: "/api/v1/messages/" + jsonNumber(messageID),
		},
		Participants: []Participant{}, Content: content,
	}
}

func packetFromJSON(t *testing.T, content string) Packet {
	t.Helper()
	var packet Packet
	require.NoError(t, json.Unmarshal([]byte(content), &packet))
	return packet
}

func mustRenderTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return &parsed
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}
