package muesli

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
)

func sampleMeeting() Meeting {
	return Meeting{
		ID: 42, Title: "  Weekly   sync ", StartTime: "2026-09-01T14:00:00Z", EndTime: "2026-09-01T14:45:00Z",
		CreatedAt: "2026-09-01 14:00:03", DurationSeconds: 2700, Status: "completed", Source: "meeting",
		RawTranscript:  "[10:00:01] You: hello\n",
		FormattedNotes: "## Decisions\nShip it", ManualNotes: "typed note", WordCount: 2,
		TemplateName: "Auto", TemplateKind: "auto",
		CalendarEventID: "event-1", CalendarSource: "eventKit", CalendarSeriesID: "series-1",
		Folder: "Clients/Acme", FollowUpToID: 41,
		Participants: []Participant{
			{Name: "Carol Example", Source: "manual"},
			{Name: "Alice Example", Email: "alice@example.com", Source: "calendar"},
			{Name: "Alice (manual)", Email: "Alice@Example.com", Source: "manual"},
		},
	}
}

func TestNotesState(t *testing.T) {
	for _, tt := range []struct {
		notes string
		want  string
	}{
		{notes: " \n ", want: "missing"},
		{notes: "  ## Raw Transcript\n\n[10:00:01] You: hi", want: "raw_transcript_fallback"},
		{notes: "## raw transcript", want: "raw_transcript_fallback"},
		{notes: "## Raw Transcript Analysis\nfindings", want: "structured_notes"},
		{notes: "## Summary failed\n\ntimeout", want: "summary_failed"},
		{notes: "## Summary\nfine", want: "structured_notes"},
	} {
		assert.Equal(t, tt.want, NotesState(tt.notes), "notes %q", tt.notes)
	}
}

func TestSourceMessageID(t *testing.T) {
	for _, tt := range []struct {
		createdAt string
		want      string
		wantErr   bool
	}{
		{createdAt: "2026-09-01 14:00:03", want: "meeting:42:20260901T140003Z"},
		{createdAt: "2026-09-01T16:00:03+02:00", want: "meeting:42:20260901T140003Z"},
		{createdAt: "", wantErr: true},
		{createdAt: "yesterday", wantErr: true},
	} {
		got, err := Meeting{ID: 42, CreatedAt: tt.createdAt}.SourceMessageID()
		if tt.wantErr {
			require.Error(t, err, "created_at %q", tt.createdAt)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tt.want, got)
	}
}

func TestEligibility(t *testing.T) {
	for _, tt := range []struct {
		name    string
		meeting Meeting
		want    SkipReason
	}{
		{name: "completed", meeting: sampleMeeting(), want: ""},
		{name: "deleted", meeting: Meeting{Deleted: true, RawTranscript: "x"}, want: SkipDeleted},
		{name: "recording", meeting: Meeting{Status: "recording", RawTranscript: "x"}, want: SkipInProgress},
		{name: "processing", meeting: Meeting{Status: "processing", RawTranscript: "x"}, want: SkipInProgress},
		{name: "empty", meeting: Meeting{Status: "completed", FormattedNotes: " "}, want: SkipEmpty},
		{name: "failed with transcript", meeting: Meeting{Status: "failed", RawTranscript: "x"}, want: ""},
		{name: "unknown future status", meeting: Meeting{Status: "archived", ManualNotes: "x"}, want: ""},
	} {
		assert.Equal(t, tt.want, tt.meeting.Eligibility(), tt.name)
	}
}

func TestArchiveSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	snapshot, err := sampleMeeting().ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)

	assert.Equal(int64(7), snapshot.SourceID)
	assert.Equal("you@example.com", snapshot.AccountEmail)
	assert.Equal("meeting:42:20260901T140003Z", snapshot.SourceMessageID)
	assert.Equal(snapshot.SourceMessageID, snapshot.SourceConversationID)
	assert.Equal("Weekly sync", snapshot.Title)
	assert.Equal(time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC), snapshot.StartedAt)
	assert.Equal(&meetingarchive.Person{Email: "you@example.com"}, snapshot.Organizer)
	assert.Equal([]meetingarchive.Person{{Name: "Alice Example", Email: "alice@example.com"}}, snapshot.Attendees)
	assert.Equal(RawFormat, snapshot.RawFormat)
	assert.Equal("Weekly sync\n"+
		"When: 2026-09-01 14:00 - 14:45\n"+
		"Attendees: Carol Example, Alice Example\n"+
		"\n"+
		"Summary:\n"+
		"## Decisions\nShip it\n"+
		"\n"+
		"Notes:\n"+
		"typed note\n"+
		"\n"+
		"Transcript:\n"+
		"[10:00:01] You: hello", snapshot.Body)
	assert.Equal(snapshot.Body, snapshot.Snippet, "bodies under 200 runes are their own snippet")

	raw := string(snapshot.Raw)
	for _, forbidden := range []string{"contact:", "ABC", "updated_at", "recording", "visual_context", "prompt", "muesli.db"} {
		assert.NotContains(raw, forbidden)
	}
	var evidence map[string]any
	require.NoError(json.Unmarshal(snapshot.Raw, &evidence))
	assert.InDelta(float64(1), evidence["schema_version"], 0)
	meeting, ok := evidence["meeting"].(map[string]any)
	require.True(ok, "meeting is an object")
	assert.Equal("structured_notes", meeting["notes_state"])
	assert.Equal("Clients/Acme", meeting["folder"])
	assert.Equal("2026-09-01T14:00:03Z", meeting["created_at"])
	assert.Equal([]any{
		map[string]any{"name": "Carol Example", "source": "manual"},
		map[string]any{"name": "Alice Example", "email": "alice@example.com", "source": "calendar"},
	}, evidence["participants"])

	var metadata map[string]any
	require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
	assert.Equal("muesli", metadata["platform"])
	assert.Equal("mac", metadata["source_identifier"])
	assert.InDelta(float64(42), metadata["muesli_id"], 0)
	assert.Equal(true, metadata["has_summary"])
	assert.InDelta(float64(1), metadata["name_only_participants"], 0)

	again, err := sampleMeeting().ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)
	assert.Equal(snapshot.Raw, again.Raw, "raw evidence is deterministic")
	assert.Equal(snapshot.Metadata, again.Metadata)
}

func TestArchiveSnapshotOmitsFallbackNotesFromSummary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meeting := sampleMeeting()
	meeting.FormattedNotes = "## Raw Transcript\n\n[10:00:01] You: hello"
	meeting.ManualNotes = ""
	meeting.Participants = nil

	snapshot, err := meeting.ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)

	assert.Equal("Weekly sync\nWhen: 2026-09-01 14:00 - 14:45\n\nTranscript:\n[10:00:01] You: hello", snapshot.Body)
	var metadata map[string]any
	require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
	assert.Equal(false, metadata["has_summary"])
	assert.Equal("raw_transcript_fallback", metadata["notes_state"])
}

func TestArchiveSnapshotTimes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	byDuration := sampleMeeting()
	byDuration.EndTime = ""
	byDuration.DurationSeconds = 600
	snapshot, err := byDuration.ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)
	assert.Contains(snapshot.Body, "When: 2026-09-01 14:00 - 14:10\n")

	backwards := sampleMeeting()
	backwards.EndTime = "2026-09-01T13:00:00Z"
	backwards.DurationSeconds = 0
	snapshot, err = backwards.ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)
	assert.Contains(snapshot.Body, "When: 2026-09-01 14:00\n")

	untitled := sampleMeeting()
	untitled.Title = "  "
	snapshot, err = untitled.ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)
	assert.Equal("Meeting on 2026-09-01", snapshot.Title)

	unparseable := sampleMeeting()
	unparseable.StartTime = "soon"
	_, err = unparseable.ArchiveSnapshot(7, "mac", "you@example.com")
	require.ErrorContains(err, "start_time")

	noCreatedAt := sampleMeeting()
	noCreatedAt.CreatedAt = ""
	_, err = noCreatedAt.ArchiveSnapshot(7, "mac", "you@example.com")
	assert.ErrorContains(err, "created_at")
}

func TestArchiveSnapshotFillsBlankNameFromDuplicateParticipant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meeting := sampleMeeting()
	meeting.Participants = []Participant{
		{Email: "alice@example.com", Source: "calendar"},
		{Name: "Alice Example", Email: "ALICE@example.com", Source: "manual"},
	}

	snapshot, err := meeting.ArchiveSnapshot(7, "mac", "you@example.com")
	require.NoError(err)

	assert.Equal([]meetingarchive.Person{{Name: "Alice Example", Email: "alice@example.com"}}, snapshot.Attendees)
	assert.Contains(snapshot.Body, "Attendees: Alice Example\n")
	assert.Contains(string(snapshot.Raw), `{"name":"Alice Example","email":"alice@example.com","source":"calendar"}`)
}
