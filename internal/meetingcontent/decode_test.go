package meetingcontent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeGranolaEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte(`{
		"owner":{"name":"Morgan Example","email":"morgan@example.com"},
		"attendees":[{"name":"Riley Example","email":"riley@example.com"}],
		"summary_markdown":"## Decision\nShip café support",
		"summary_text":"fallback",
		"transcript":[
			{"speaker":{"source":"microphone","name":"Morgan Example"},"text":"Start","start_time":"2026-09-12T10:00:00Z","end_time":"2026-09-12T10:00:05Z"},
			{"speaker":{"source":"speaker","diarization_label":"Speaker A"},"text":"Done","start_time":"2026-09-12T10:02:00Z","end_time":"2026-09-12T10:02:00.5Z"}
		]
	}`)

	content := Decode("granola_json", raw, nil)
	assertions.Equal(Section{State: StateAvailable, Text: "## Decision\nShip café support"}, content.Summary)
	assertions.Equal(StateUnsupported, content.Notes.State)
	assertions.Equal(StateAvailable, content.Transcript.State)
	requirements.Len(content.Transcript.Segments, 2)
	assertions.Equal("Morgan Example", content.Transcript.Segments[0].Speaker)
	assertions.Equal("Speaker A", content.Transcript.Segments[1].Speaker)
	assertions.Equal(mustTime(t, "2026-09-12T10:00:00Z"), content.Transcript.Segments[0].StartedAt)
	assertions.Equal(mustTime(t, "2026-09-12T10:02:00.5Z"), content.Transcript.Segments[1].EndedAt)
	assertions.Equal(CoverageUnsupported, content.ActionCoverage)
	assertions.Equal("no_structured_actions", content.ActionReason)
	requirements.NotNil(content.DurationSeconds)
	assertions.InDelta(120.5, *content.DurationSeconds, 0)
	assertions.Equal(DurationTranscriptSpan, content.DurationBasis)
	assertions.Equal([]Participant{
		{Name: "Morgan Example", Email: "morgan@example.com", Role: "from"},
		{Name: "Riley Example", Email: "riley@example.com", Role: "to"},
	}, content.SourceParticipants)

	scheduled := Decode("granola_json", []byte(`{
		"summary_markdown":"",
		"calendar_event":{"organiser":"host@example.com","scheduled_start_time":"2026-09-12T10:00:00Z","scheduled_end_time":"2026-09-12T10:45:00Z"},
		"transcript":[{"speaker":{"source":"microphone"},"text":"Only","start_time":"2026-09-12T10:00:00Z"}]
	}`), nil)
	assertions.Equal(StateEmpty, scheduled.Summary.State)
	assertions.Equal(StateAvailable, scheduled.Transcript.State)
	requirements.NotNil(scheduled.DurationSeconds)
	assertions.InDelta(float64(2700), *scheduled.DurationSeconds, 0)
	assertions.Equal(DurationScheduled, scheduled.DurationBasis)

	unknown := Decode("granola_json", []byte(`{"summary_text":"Fallback","transcript":[{"speaker":{"source":"microphone"},"text":"Only"}]}`), nil)
	assertions.Nil(unknown.DurationSeconds)
	assertions.Empty(unknown.DurationBasis)
}

func TestDecodeCirclebackEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte(`{
		"meeting":{
			"id":42,
			"notes":"## Decisions\nUse deterministic packets",
			"summary":"fallback",
			"durationSeconds":"90.5",
			"organizer":{"name":"Morgan Example","email":"morgan@example.com"},
			"attendees":[{"name":"Riley Example","email":"riley@example.com"},{"name":"Guest"}],
			"actionItems":[
				{"title":"Send recap","description":"Include decisions","status":"IN_PROGRESS","assignee":{"displayName":"Riley Example","email":"riley@example.com"},"dueDate":"2026-09-15"},
				{"name":"Confirm launch","status":"done","assignee":"label@example.com"}
			],
			"recordingUrl":"https://cdn.example.test/private"
		},
		"transcript":{"meetingId":42,"transcript":[
			{"speakerName":"Morgan Example","content":"Hello","start":"0"},
			{"speaker":"Riley Example","words":"Ready","startTimestamp":65.25}
		]}
	}`)

	content := Decode("circleback_json", raw, []byte(`{"recording_url":"must-not-escape"}`))
	assertions.Equal(Section{State: StateAvailable, Text: "## Decisions\nUse deterministic packets"}, content.Summary)
	assertions.Equal(StateUnsupported, content.Notes.State)
	requirements.Len(content.Actions, 2)
	assertions.Equal(Action{
		Ordinal: 0, Title: "Send recap", Description: "Include decisions",
		AssigneeName: "Riley Example", AssigneeEmail: "riley@example.com",
		Status: StatusPending, SourceStatus: "IN_PROGRESS", DueDate: "2026-09-15",
		Origin: "structured", Locator: "meeting.actionItems[0]",
	}, content.Actions[0])
	assertions.Equal("label@example.com", content.Actions[1].AssigneeName)
	assertions.Empty(content.Actions[1].AssigneeEmail)
	assertions.Equal(StatusCompleted, content.Actions[1].Status)
	assertions.Equal(CoverageAvailable, content.ActionCoverage)
	assertions.Equal(StateAvailable, content.Transcript.State)
	requirements.Len(content.Transcript.Segments, 2)
	assertions.Equal(new(float64(0)), content.Transcript.Segments[0].OffsetSeconds)
	assertions.Equal(new(65.25), content.Transcript.Segments[1].OffsetSeconds)
	requirements.NotNil(content.DurationSeconds)
	assertions.InDelta(90.5, *content.DurationSeconds, 0)
	assertions.Equal(DurationProvider, content.DurationBasis)
	assertions.Equal([]Participant{
		{Name: "Morgan Example", Email: "morgan@example.com", Role: "from"},
		{Name: "Riley Example", Email: "riley@example.com", Role: "to"},
		{Name: "Guest", Role: "to"},
	}, content.SourceParticipants)

	omitted := Decode("circleback_json", []byte(`{"meeting":{"summary":"Summary"},"transcript":{"text":"Plain transcript"}}`), nil)
	assertions.Equal(CoverageUnavailable, omitted.ActionCoverage)
	assertions.Equal("missing_field", omitted.ActionReason)
	assertions.Equal(Transcript{State: StateAvailable, Text: "Plain transcript"}, omitted.Transcript)

	empty := Decode("circleback_json", []byte(`{"meeting":{"summary":"Summary","actionItems":[]},"transcript":{"transcript":[]}}`), nil)
	assertions.Equal(CoverageAvailable, empty.ActionCoverage)
	assertions.Empty(empty.Actions)
	assertions.Equal(StateEmpty, empty.Transcript.State)

	scheduled := Decode("circleback_json", []byte(`{"meeting":{"summary":"Summary","actionItems":[],"date":"2026-09-12T10:00:00Z","endTime":"2026-09-12T10:30:00Z"}}`), nil)
	requirements.NotNil(scheduled.DurationSeconds)
	assertions.InDelta(float64(1800), *scheduled.DurationSeconds, 0)
	assertions.Equal(DurationScheduled, scheduled.DurationBasis)
}

func TestDecodeNotionEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte(`{
		"schema_version":1,
		"discovery":{"id":"meeting-1","type":"meeting_notes","meeting_notes":{
			"children":{"summary_block_id":"task-1","notes_block_id":"notes-1","transcript_block_id":"transcript-1"},
			"recording":{"start_time":"2026-09-12T10:00:00Z","end_time":"2026-09-12T10:42:00Z"},
			"calendar_event":{"start_time":"2026-09-12T09:55:00Z","end_time":"2026-09-12T10:45:00Z","attendees":["user-1","user-2"]}
		}},
		"canonical":{"summary":"Decision","notes":"Supporting notes","transcript":"Spoken text"},
		"summary":{
			"root":{"object":"block","id":"task-1","type":"to_do","has_children":true,"to_do":{"rich_text":[{"plain_text":"Prepare plan"}],"checked":false}},
			"pages":[{"results":[
				{"object":"block","id":"task-1","type":"to_do","to_do":{"rich_text":[{"plain_text":"Duplicate"}],"checked":true}},
				{"object":"block","id":"task-2","type":"to_do","to_do":{"rich_text":[{"plain_text":"Ship release"}],"checked":true}}
			],"has_more":false}]
		},
		"notes":{
			"root":{"object":"block","id":"notes-1","type":"paragraph","has_children":true,"paragraph":{"rich_text":[{"plain_text":"Notes"}]}},
			"pages":[{"results":[{"object":"block","id":"task-3","type":"to_do","to_do":{"rich_text":[{"plain_text":"Verify metrics"}]}}],"has_more":false}]
		},
		"transcript":{
			"root":{"object":"block","id":"transcript-1","type":"to_do","to_do":{"rich_text":[{"plain_text":"Ignore transcript checkbox"}],"checked":false}}
		},
		"attendee_labels":["Riley Example","Unresolved Guest"],
		"resolved_users":[{"id":"user-1","name":"Riley Example","email":"riley@example.com","email_verified":true}],
		"page_markdown":{"object":"page_markdown","id":"page-1","markdown":"- [ ] Do not infer"},
		"recording_url":"https://example.test/must-not-escape"
	}`)

	content := Decode("notion_meeting_json", raw, nil)
	assertions.Equal(Section{State: StateAvailable, Text: "Decision"}, content.Summary)
	assertions.Equal(Section{State: StateAvailable, Text: "Supporting notes"}, content.Notes)
	assertions.Equal(Transcript{State: StateAvailable, Text: "Spoken text"}, content.Transcript)
	assertions.Equal(CoverageAvailable, content.ActionCoverage)
	requirements.Len(content.Actions, 3)
	assertions.Equal(Action{Ordinal: 0, SourceID: "task-1", Title: "Prepare plan", Status: StatusPending, SourceStatus: "false", Origin: "checkbox", Locator: "block:task-1"}, content.Actions[0])
	assertions.Equal(Action{Ordinal: 1, SourceID: "task-2", Title: "Ship release", Status: StatusCompleted, SourceStatus: "true", Origin: "checkbox", Locator: "block:task-2"}, content.Actions[1])
	assertions.Equal(Action{Ordinal: 2, SourceID: "task-3", Title: "Verify metrics", Status: StatusUnknown, Origin: "checkbox", Locator: "block:task-3"}, content.Actions[2])
	requirements.NotNil(content.DurationSeconds)
	assertions.InDelta(float64(2520), *content.DurationSeconds, 0)
	assertions.Equal(DurationProvider, content.DurationBasis)
	assertions.Equal([]Participant{
		{Name: "Riley Example", Email: "riley@example.com", Role: "to"},
		{Name: "Unresolved Guest", Role: "to"},
	}, content.SourceParticipants)

	missingTree := Decode("notion_meeting_json", []byte(`{
		"schema_version":1,
		"discovery":{"meeting_notes":{"children":{"summary_block_id":"summary-1"}}},
		"canonical":{"summary":"Still usable"}
	}`), nil)
	assertions.Equal(Section{State: StateAvailable, Text: "Still usable"}, missingTree.Summary)
	assertions.Equal(CoverageUnavailable, missingTree.ActionCoverage)
	assertions.Equal("incomplete_tree", missingTree.ActionReason)

	unsupported := Decode("notion_meeting_json", []byte(`{"schema_version":2,"canonical":{"summary":"Ignored"}}`), nil)
	assertions.Equal(StateUnavailable, unsupported.Summary.State)
	assertions.Equal("unsupported_schema", unsupported.Summary.Reason)
	assertions.Equal(CoverageUnavailable, unsupported.ActionCoverage)
	assertions.Equal("unsupported_schema", unsupported.ActionReason)
}

func TestDecodeGenericEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	old := Decode("meeting_json", []byte(`{
		"summary_text":"Old summary",
		"transcript":"Plain transcript",
		"started_at":"2026-09-12T10:00:00Z",
		"ended_at":"2026-09-12T10:20:00Z"
	}`), nil)
	assertions.Equal(CoverageUnsupported, old.ActionCoverage)
	assertions.Equal("no_structured_actions", old.ActionReason)
	requirements.NotNil(old.DurationSeconds)
	assertions.InDelta(float64(1200), *old.DurationSeconds, 0)
	assertions.Equal(DurationProvider, old.DurationBasis)

	empty := Decode("meeting_json", []byte(`{"summary_markdown":"Summary","action_items":[]}`), nil)
	assertions.Equal(CoverageAvailable, empty.ActionCoverage)
	assertions.NotNil(empty.Actions)
	assertions.Empty(empty.Actions)

	typed := Decode("meeting_json", []byte(`{
		"summary_markdown":"Summary",
		"organizer":{"name":"Morgan Example","email":"morgan@example.com"},
		"attendees":[{"name":"Riley Example","email":"riley@example.com"}],
		"transcript_segments":[
			{"speaker":"Morgan Example","text":"Start","offset_seconds":1.25},
			{"speaker":"Riley Example","text":"End","offset_seconds":61.75}
		],
		"action_items":[
			{"source_id":"task-a","title":"Send recap","description":"Include links","assignee_name":"Riley Example","assignee_email":"riley@example.com","status":"open","due_date":"2026-09-16"},
			{"source_id":"task-b","description":17}
		]
	}`), nil)
	assertions.Equal(CoveragePartial, typed.ActionCoverage)
	assertions.Equal("invalid_section", typed.ActionReason)
	requirements.Len(typed.Actions, 1)
	assertions.Equal(Action{
		Ordinal: 0, SourceID: "task-a", Title: "Send recap", Description: "Include links",
		AssigneeName: "Riley Example", AssigneeEmail: "riley@example.com",
		Status: StatusPending, SourceStatus: "open", DueDate: "2026-09-16",
		Origin: "structured", Locator: "action_items[0]",
	}, typed.Actions[0])
	requirements.NotNil(typed.DurationSeconds)
	assertions.InDelta(60.5, *typed.DurationSeconds, 0)
	assertions.Equal(DurationTranscriptSpan, typed.DurationBasis)
	assertions.Equal([]Participant{
		{Name: "Morgan Example", Email: "morgan@example.com", Role: "from"},
		{Name: "Riley Example", Email: "riley@example.com", Role: "to"},
	}, typed.SourceParticipants)

	projection := ProjectionContent(typed)
	assertions.Equal(typed.Transcript.State, projection.Transcript.State)
	assertions.Equal(typed.Transcript.Reason, projection.Transcript.Reason)
	assertions.Empty(projection.Transcript.Text)
	assertions.Nil(projection.Transcript.Segments)
	assertions.Equal(typed.SourceParticipants, projection.SourceParticipants)
	assertions.NotEmpty(typed.Transcript.Segments, "projection must not mutate its input")
}

func TestDecodeRejectsMissingInvalidAndUnsupportedRaw(t *testing.T) {
	tests := []struct {
		name      string
		rawFormat string
		raw       []byte
		reason    string
	}{
		{name: "missing", rawFormat: "meeting_json", reason: "missing_raw"},
		{name: "invalid", rawFormat: "meeting_json", raw: []byte(`{`), reason: "invalid_raw"},
		{name: "unsupported", rawFormat: "future_json", raw: []byte(`{}`), reason: "unsupported_format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			content := Decode(tt.rawFormat, tt.raw, nil)
			assertions.Equal(StateUnavailable, content.Summary.State)
			assertions.Equal(tt.reason, content.Summary.Reason)
			assertions.Equal(StateUnavailable, content.Notes.State)
			assertions.Equal(tt.reason, content.Transcript.Reason)
			assertions.Equal(CoverageUnavailable, content.ActionCoverage)
			assertions.Equal(tt.reason, content.ActionReason)
			assertions.NotNil(content.Actions)
		})
	}
}

func TestDecodeNormalizesExactStatusTokens(t *testing.T) {
	tests := map[string]Status{
		" pending ":   StatusPending,
		"OPEN":        StatusPending,
		"todo":        StatusPending,
		"to_do":       StatusPending,
		"incomplete":  StatusPending,
		"in_progress": StatusPending,
		"completed":   StatusCompleted,
		"complete":    StatusCompleted,
		"done":        StatusCompleted,
		"cancelled":   StatusCancelled,
		"canceled":    StatusCancelled,
		"not done":    StatusUnknown,
		"":            StatusUnknown,
	}
	for source, want := range tests {
		assert.Equal(t, want, NormalizeStatus(source), source)
	}
}

func mustTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return &parsed
}
