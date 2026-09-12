package meetingcontent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewRound1NotionMentionDoesNotInventAssignee(t *testing.T) {
	assertions := assert.New(t)
	content := Decode("notion_meeting_json", []byte(`{
		"schema_version":1,
		"discovery":{"meeting_notes":{"children":{"summary_block_id":"task-mention"},"calendar_event":{"attendees":["user-1"]}}},
		"canonical":{"summary":"Summary"},
		"summary":{"root":{"id":"task-mention","type":"to_do","has_children":false,"to_do":{
			"rich_text":[
				{"plain_text":"Send draft to "},
				{"plain_text":"@Example","mention":{"type":"user","user":{"id":"user-1"}}}
			],
			"checked":false
		}}},
		"attendee_labels":["Example Person"],
		"resolved_users":[{"id":"user-1","name":"Example Person","email":"person@example.com","email_verified":true}]
	}`), nil)

	require.Len(t, content.Actions, 1)
	assertions.Equal("Send draft to @Example", content.Actions[0].Title)
	assertions.Empty(content.Actions[0].AssigneeName)
	assertions.Empty(content.Actions[0].AssigneeEmail)
	assertions.Equal([]Participant{{Name: "Example Person", Email: "person@example.com", Role: "to"}}, content.SourceParticipants)
}

func TestReviewRound1RendererFinishesProvenanceBeforeContent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	first := renderEntry(71, "First", Content{
		Summary: Section{State: StateAvailable, Text: "x"}, Notes: Section{State: StateEmpty},
		Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageAvailable,
	})
	second := renderEntry(72, "Later", Content{
		Summary: Section{State: StateEmpty}, Notes: Section{State: StateEmpty},
		Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageAvailable,
	})

	firstRendered := first
	firstRendered.Content.Transcript = Transcript{State: StateOmittedByRequest}
	secondRendered := second
	secondRendered.Content.Transcript = Transcript{State: StateOmittedByRequest}
	firstFull := Packet{
		SchemaVersion: 1, ArchiveUID: "archive", RequestedMessageIDs: []int64{71, 72},
		Meetings: []Entry{firstRendered}, OmittedMessageIDs: []int64{72}, Truncated: true, Truncations: []Truncation{},
	}
	secondSkeleton := Packet{
		SchemaVersion: 1, ArchiveUID: "archive", RequestedMessageIDs: []int64{71, 72},
		Meetings: []Entry{secondRendered}, OmittedMessageIDs: []int64{71}, Truncated: true, Truncations: []Truncation{},
	}
	firstTruncated := firstRendered
	firstTruncated.Content.Summary = Section{State: StateTruncated}
	firstSkeleton := Packet{
		SchemaVersion: 1, ArchiveUID: "archive", RequestedMessageIDs: []int64{71, 72},
		Meetings: []Entry{firstTruncated}, OmittedMessageIDs: []int64{72}, Truncated: true,
		Truncations: []Truncation{{MessageID: 71, Section: "summary", OriginalBytes: 1, IncludedBytes: 0}},
	}
	firstFullJSON, err := json.Marshal(firstFull)
	requirements.NoError(err)
	secondSkeletonJSON, err := json.Marshal(secondSkeleton)
	requirements.NoError(err)
	firstSkeletonJSON, err := json.Marshal(firstSkeleton)
	requirements.NoError(err)
	budget := max(len(firstFullJSON), len(secondSkeletonJSON))
	requirements.Less(budget, len(firstSkeletonJSON), "fixture must exercise truncation-accounting overhead")

	result, err := Render("archive", []Entry{first, second}, PacketOptions{Format: FormatJSON, MaxBytes: budget})
	requirements.NoError(err)
	packet := packetFromJSON(t, result.Content)
	requirements.Len(packet.Meetings, 1)
	assertions.Equal(int64(72), packet.Meetings[0].Meeting.MessageID)
	assertions.Equal([]int64{71}, packet.OmittedMessageIDs)
	assertions.NotContains(result.Content, `"text":"x"`)
}

func TestReviewRound1NotionDepthFirstPagesPreserveOrder(t *testing.T) {
	assertions := assert.New(t)
	content := Decode("notion_meeting_json", []byte(`{
		"schema_version":1,
		"discovery":{"meeting_notes":{"children":{"summary_block_id":"summary-root"}}},
		"canonical":{"summary":"Summary"},
		"summary":{
			"root":{"id":"summary-root","type":"paragraph","has_children":true,"paragraph":{"rich_text":[]}},
			"pages":[
				{"results":[
					{"id":"branch","type":"paragraph","has_children":true,"paragraph":{"rich_text":[]}},
					{"id":"task-sibling","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Sibling action"}],"checked":false}}
				],"has_more":true,"next_cursor":"parent-2"},
				{"results":[
					{"id":"task-nested","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Nested action"}],"checked":false}}
				],"has_more":false,"next_cursor":null},
				{"results":[
					{"id":"task-last","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Last action"}],"checked":true}}
				],"has_more":false,"next_cursor":null}
			]
		}
	}`), nil)
	assertions.Equal(CoverageAvailable, content.ActionCoverage)
	require.Len(t, content.Actions, 3)
	assertions.Equal([]string{"task-nested", "task-sibling", "task-last"}, []string{
		content.Actions[0].SourceID, content.Actions[1].SourceID, content.Actions[2].SourceID,
	})
	assertions.Equal([]int{0, 1, 2}, []int{
		content.Actions[0].Ordinal, content.Actions[1].Ordinal, content.Actions[2].Ordinal,
	})
}

func TestReviewRound1NotionIncompleteTraversalKeepsAttributableActions(t *testing.T) {
	tests := []struct {
		name  string
		pages string
		want  []string
	}{
		{
			name: "dangling parent cursor after complete nested child",
			pages: `[
				{"results":[
					{"id":"branch","type":"paragraph","has_children":true,"paragraph":{"rich_text":[]}},
					{"id":"task-sibling","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Sibling action"}],"checked":false}}
				],"has_more":true,"next_cursor":"parent-2"},
				{"results":[
					{"id":"task-nested","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Nested action"}],"checked":false}}
				],"has_more":false,"next_cursor":null}
			]`,
			want: []string{"task-nested", "task-sibling"},
		},
		{
			name: "false has_more with cursor",
			pages: `[
				{"results":[
					{"id":"task-known","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Known action"}],"checked":false}}
				],"has_more":false,"next_cursor":"unexpected"}
			]`,
			want: []string{"task-known"},
		},
		{
			name: "repeated parent cursor",
			pages: `[
				{"results":[],"has_more":true,"next_cursor":"again"},
				{"results":[
					{"id":"task-known","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Known action"}],"checked":false}}
				],"has_more":true,"next_cursor":"again"}
			]`,
			want: []string{"task-known"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{
				"schema_version":1,
				"discovery":{"meeting_notes":{"children":{"summary_block_id":"summary-root"}}},
				"canonical":{"summary":"Summary"},
				"summary":{
					"root":{"id":"summary-root","type":"paragraph","has_children":true,"paragraph":{"rich_text":[]}},
					"pages":` + tt.pages + `
				}
			}`)
			content := Decode("notion_meeting_json", raw, nil)
			assert.Equal(t, CoveragePartial, content.ActionCoverage)
			assert.Equal(t, "incomplete_tree", content.ActionReason)
			ids := make([]string, len(content.Actions))
			for index := range content.Actions {
				ids[index] = content.Actions[index].SourceID
			}
			assert.Equal(t, tt.want, ids)
		})
	}
}

func TestReviewRound1CirclebackNullOptionalsPreserveUsableEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Decode("circleback_json", []byte(`{
		"meeting":{
			"notes":null,
			"summary":"Fallback summary",
			"actionItems":[{"title":null,"name":"Keep action","description":null,"status":null,"dueDate":null,"assignee":null}]
		},
		"transcript":{"text":null,"transcript":[{"speaker":null,"speakerName":"Example Speaker","text":null,"content":"Usable segment","start":0}]}
	}`), nil)
	assertions.Equal(Section{State: StateAvailable, Text: "Fallback summary"}, content.Summary)
	assertions.Equal(CoverageAvailable, content.ActionCoverage)
	requirements.Len(content.Actions, 1)
	assertions.Equal(StatusUnknown, content.Actions[0].Status)
	assertions.Empty(content.Actions[0].Description)
	assertions.Empty(content.Actions[0].SourceStatus)
	assertions.Empty(content.Actions[0].DueDate)
	assertions.Equal(StateAvailable, content.Transcript.State)
	requirements.Len(content.Transcript.Segments, 1)
	assertions.Equal("Example Speaker", content.Transcript.Segments[0].Speaker)
	assertions.Equal("Usable segment", content.Transcript.Segments[0].Text)

	empty := Decode("circleback_json", []byte(`{
		"meeting":{"summary":"Summary","actionItems":[]},
		"transcript":{"text":null,"transcript":null}
	}`), nil)
	assertions.Equal(StateEmpty, empty.Transcript.State)
}

func TestReviewRound1NotionCheckedMustBeBoolean(t *testing.T) {
	tests := []struct {
		name     string
		checked  string
		coverage Coverage
		count    int
	}{
		{name: "null", checked: "null", coverage: CoverageUnavailable},
		{name: "string", checked: `"false"`, coverage: CoverageUnavailable},
		{name: "missing", checked: "", coverage: CoverageAvailable, count: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			checked := ""
			if tt.checked != "" {
				checked = `,"checked":` + tt.checked
			}
			raw := []byte(`{
				"schema_version":1,
				"discovery":{"meeting_notes":{"children":{"summary_block_id":"task-check"}}},
				"canonical":{"summary":"Summary"},
				"summary":{"root":{"id":"task-check","type":"to_do","has_children":false,"to_do":{
					"rich_text":[{"plain_text":"Check evidence"}]` + checked + `
				}}}
			}`)
			content := Decode("notion_meeting_json", raw, nil)
			assertions.Equal(tt.coverage, content.ActionCoverage)
			assertions.Len(content.Actions, tt.count)
			if tt.count == 1 {
				assertions.Equal(StatusUnknown, content.Actions[0].Status)
				assertions.Empty(content.Actions[0].SourceStatus)
			}
		})
	}
}
