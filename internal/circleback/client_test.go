package circleback

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type contractToolCall struct {
	name string
	args map[string]any
}

var officialCirclebackInputSchemas = map[string]map[string]any{
	toolSearchMeetings: {
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"intent":    map[string]any{"type": "string"},
			"pageIndex": map[string]any{"type": "integer"},
			"startDate": map[string]any{"type": "string", "format": "date"},
			"endDate":   map[string]any{"type": "string", "format": "date"},
		},
		"required": []string{"intent", "pageIndex"},
	},
	toolReadMeetings: {
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"intent": map[string]any{"type": "string"},
			"meetingIds": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []string{"intent", "meetingIds"},
	},
	toolGetTranscripts: {
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"intent": map[string]any{"type": "string"},
			"meetingIds": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []string{"intent", "meetingIds"},
	},
}

// newContractMCPSession registers tools with the strict input schemas exposed
// by @circleback/cli 0.2.2. The typed helper performs handler-side validation;
// Server.AddTool alone only advertises the schema.
func newContractMCPSession(t *testing.T, results map[string]string, calls *[]contractToolCall) *Session {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-circleback", Version: "0.2.2"}, nil)
	for name, payload := range results {
		mcp.AddTool[map[string]any, any](server, &mcp.Tool{
			Name:        name,
			Description: "official Circleback contract fixture",
			InputSchema: officialCirclebackInputSchemas[name],
		}, func(_ context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			*calls = append(*calls, contractToolCall{name: name, args: input})
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: payload}},
			}, nil, nil
		})
	}

	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Wait() })

	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return NewSessionForTesting(cs)
}

func requireTranscriptClassification(t *testing.T, tr *Transcript, want TranscriptClassification) {
	t.Helper()
	require.NotNil(t, tr)
	assert.Equal(t, want, tr.Classification())
}

// newFakeMCPSession spins up an in-process MCP server whose tools return the
// given canned JSON (as text content, the shape Circleback uses), and
// connects a Session to it over in-memory transports.
func newFakeMCPSession(t *testing.T, results map[string]string) *Session {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-circleback", Version: "0"}, nil)
	for name, payload := range results {
		server.AddTool(&mcp.Tool{
			Name:        name,
			Description: "fake",
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: payload}},
			}, nil
		})
	}

	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Wait() })

	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return NewSessionForTesting(cs)
}

func TestSearchMeetings_EnvelopeShapes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"bare array", `[{"id": 7, "name": "Standup"}]`},
		{"meetings envelope", `{"meetings": [{"id": "7", "name": "Standup"}]}`},
		{"results envelope", `{"results": [{"id": 7, "title": "Standup"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			s := newFakeMCPSession(t, map[string]string{toolSearchMeetings: tc.payload})
			meetings, err := s.SearchMeetings(context.Background(), "2026-01-01", "", 0)
			require.NoError(err)
			require.Len(meetings, 1)
			require.Equal("7", string(meetings[0].ID), "string and numeric IDs both decode")
			require.Equal("Standup", meetings[0].DisplayName())
			require.NotEmpty(meetings[0].Raw, "verbatim payload preserved")
		})
	}
}

func TestSearchMeetings_OfficialArgumentsAndSchemas(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{toolSearchMeetings: `[]`}, &calls)

	_, err := s.SearchMeetings(context.Background(), "2026-01-01", "2026-02-01", 3)
	require.NoError(err)
	require.Len(calls, 1)
	assert.Equal(toolSearchMeetings, calls[0].name)
	assert.EqualValues(3, calls[0].args["pageIndex"])
	assert.Equal("2026-01-01", calls[0].args["startDate"])
	assert.Equal("2026-02-01", calls[0].args["endDate"])
	assert.NotEmpty(calls[0].args["intent"])
	assert.NotContains(calls[0].args, "limit")

	_, err = s.SearchMeetings(context.Background(), "", "", 0)
	require.NoError(err)
	require.Len(calls, 2)
	assert.EqualValues(0, calls[1].args["pageIndex"])
	assert.NotContains(calls[1].args, "startDate")
	assert.NotContains(calls[1].args, "endDate")
	assert.NotContains(calls[1].args, "limit")
}

func TestReadMeetings_OfficialArgumentsAndSchemas(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolReadMeetings: `{"meetings":[{
			"id":"meeting-7",
			"name":"Planning",
			"actionItems":[{
				"title":"Send recap",
				"status":"open",
				"assignee":{"name":"Bob Example","email":"bob@example.com"}
			}]
		}]}`,
	}, &calls)

	meetings, err := s.ReadMeetings(context.Background(), []string{"meeting-7"})
	require.NoError(err)
	require.Len(calls, 1)
	assert.Equal(toolReadMeetings, calls[0].name)
	assert.NotEmpty(calls[0].args["intent"])
	assert.Equal([]any{"meeting-7"}, calls[0].args["meetingIds"])
	require.Len(meetings, 1)
	assert.Contains(buildBody(&meetings[0], nil), "- Send recap (Bob Example) [open]")
}

func TestBuildBodyIncludesSearchableMeetingMetadata(t *testing.T) {
	body := buildBody(&Meeting{
		Name: "Planning",
		ActionItems: []ActionItem{{
			Description: "Review budget delta",
		}},
		Insights: []Insight{{
			Title: "Customer signal", Content: "Expansion interest is rising",
		}},
		Tags: []string{"forecast", "customer-health"},
	}, nil)

	assert.Contains(t, body, "- Review budget delta")
	assert.Contains(t, body, "Insights:\n- Customer signal: Expansion interest is rising")
	assert.Contains(t, body, "Tags: forecast, customer-health")
}

// Circleback returns "insights" as a label-keyed object, not the array the
// decoder originally required. Every meeting in a workspace carries the same
// shape, so an array-only decoder rejects all of them and fails the entire
// sync run — no meetings reach the archive at all.
func TestReadMeetings_KeyedInsightsObjectDoesNotRejectMeeting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolReadMeetings: `{"meetings":[{
			"id":"meeting-9",
			"name":"Planning",
			"notes":"Agreed on scope.",
			"insights":{},
			"actionItems":[{"title":"Send recap","status":"open"}],
			"tags":["planning"]
		}]}`,
	}, &calls)

	meetings, err := s.ReadMeetings(context.Background(), []string{"meeting-9"})
	require.NoError(err)
	require.Len(meetings, 1)
	assert.Equal("Planning", meetings[0].DisplayName())
	assert.Empty(meetings[0].Insights)

	// An empty insight set must not emit an empty "Insights:" heading.
	body := buildBody(&meetings[0], nil)
	assert.Contains(body, "- Send recap")
	assert.NotContains(body, "Insights:")
}

// scriptedResponse is one canned tool result. The last entry repeats once the
// script is exhausted, so an "always fails" case needs a single entry.
type scriptedResponse struct {
	isError bool
	text    string
}

// newScriptedMCPSession serves responses in order for one tool and counts
// calls, so retry behaviour can be observed. The returned Session has a zero
// backoff delay (see NewSessionForTesting), so the retry loop runs without
// real sleeping.
func newScriptedMCPSession(t *testing.T, tool string, responses []scriptedResponse, calls *int) *Session {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-circleback", Version: "0.2.2"}, nil)
	mcp.AddTool[map[string]any, any](server, &mcp.Tool{
		Name:        tool,
		Description: "scripted rate-limit fixture",
		InputSchema: officialCirclebackInputSchemas[tool],
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		response := responses[min(*calls, len(responses)-1)]
		*calls++
		return &mcp.CallToolResult{
			IsError: response.isError,
			Content: []mcp.Content{&mcp.TextContent{Text: response.text}},
		}, nil, nil
	})

	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Wait() })

	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return NewSessionForTesting(cs)
}

const rateLimitText = "Rate limit exceeded. Please try again later."

// A first backfill reliably trips Circleback's limit. Without a retry the
// rejection fails the whole sync run, so a transient limit reads as a hard
// sync failure.
func TestCallToolJSON_RetriesProviderRateLimit(t *testing.T) {
	require := require.New(t)
	calls := 0
	s := newScriptedMCPSession(t, toolReadMeetings, []scriptedResponse{
		{isError: true, text: rateLimitText},
		{isError: true, text: rateLimitText},
		{text: `{"meetings":[{"id":"meeting-1","name":"Planning"}]}`},
	}, &calls)

	meetings, err := s.ReadMeetings(context.Background(), []string{"meeting-1"})
	require.NoError(err)
	require.Len(meetings, 1)
	require.Equal(3, calls, "should retry until the provider recovers")
}

func TestCallToolJSON_RateLimitGivesUpAfterBoundedAttempts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	calls := 0
	s := newScriptedMCPSession(t, toolReadMeetings, []scriptedResponse{
		{isError: true, text: rateLimitText},
	}, &calls)

	_, err := s.ReadMeetings(context.Background(), []string{"meeting-1"})
	require.Error(err)
	assert.Equal(rateLimitAttempts, calls, "retries must be bounded, not infinite")
	assert.Contains(err.Error(), "still rate limited")
	// The provider's own wording survives so the cause stays diagnosable.
	assert.Contains(err.Error(), "Rate limit exceeded")
}

// Only rate limits are transient. Retrying a contract or missing-result
// failure would repeat a call that cannot succeed.
func TestCallToolJSON_DoesNotRetryOtherServerErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	calls := 0
	s := newScriptedMCPSession(t, toolReadMeetings, []scriptedResponse{
		{isError: true, text: "meeting not found"},
	}, &calls)

	_, err := s.ReadMeetings(context.Background(), []string{"meeting-1"})
	require.Error(err)
	assert.Equal(1, calls)
	assert.Contains(err.Error(), "meeting not found")
	assert.NotContains(err.Error(), "still rate limited")
}

func TestIsRateLimitMessage(t *testing.T) {
	for _, tt := range []struct {
		message string
		want    bool
	}{
		{"Rate limit exceeded. Please try again later.", true},
		{"rate-limit hit", true},
		{"Too Many Requests", true},
		{"HTTP 429", true},
		{"meeting not found", false},
		{"", false},
	} {
		assert.Equal(t, tt.want, isRateLimitMessage(tt.message), tt.message)
	}
}

func TestInsightListTolerantShapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Insight
	}{
		{name: "null", input: `null`},
		{name: "empty array", input: `[]`},
		{name: "empty object (production shape)", input: `{}`},
		{
			name:  "array of objects preserved",
			input: `[{"title":"Risk","content":"Timeline is tight"}]`,
			want:  []Insight{{Title: "Risk", Content: "Timeline is tight"}},
		},
		{
			// Labels are sorted so a Go map's random iteration order cannot
			// churn the composed body between otherwise identical syncs.
			name:  "keyed strings sort by label",
			input: `{"Zeta":"last","Alpha":"first"}`,
			want: []Insight{
				{Name: "Alpha", Content: "first"},
				{Name: "Zeta", Content: "last"},
			},
		},
		{
			name:  "keyed object borrows its label when untitled",
			input: `{"Risk":{"content":"Timeline is tight"}}`,
			want:  []Insight{{Name: "Risk", Content: "Timeline is tight"}},
		},
		{
			name:  "keyed object keeps its own title",
			input: `{"Risk":{"title":"Schedule risk","content":"Tight"}}`,
			want:  []Insight{{Title: "Schedule risk", Content: "Tight"}},
		},
		{
			name:  "scalar value degrades to verbatim JSON",
			input: `{"Score":7}`,
			want:  []Insight{{Name: "Score", Content: "7"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got InsightList
			require.NoError(t, json.Unmarshal([]byte(tt.input), &got))
			if tt.want == nil {
				// Empty is the contract; nil vs. empty slice is not, since
				// json.Unmarshal yields a non-nil empty slice for "[]".
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, InsightList(tt.want), got)
		})
	}
}

func TestGetTranscripts_OfficialArgumentsAndSchemas(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolGetTranscripts: `{"transcripts":[{
			"id":"meeting-7",
			"transcript":[
				{"speaker":"Alice Example","text":"Hello.","timestamp":0},
				{"speaker":"Bob Example","text":"Ready.","timestamp":65.5}
			]
		}]}`,
	}, &calls)

	transcripts, err := s.GetTranscripts(context.Background(), []string{"meeting-7"})
	require.NoError(err)
	require.Len(calls, 1)
	assert.Equal(toolGetTranscripts, calls[0].name)
	assert.NotEmpty(calls[0].args["intent"])
	assert.Equal([]any{"meeting-7"}, calls[0].args["meetingIds"])
	tr := transcripts["meeting-7"]
	require.NotNil(tr, "official transcript id must key the result")
	requireTranscriptClassification(t, tr, TranscriptPresent)
	assert.Contains(buildBody(&Meeting{Name: "Planning"}, tr), "[01:05] Bob Example: Ready.")
	assert.NotEmpty(tr.Raw)
}

func TestTranscript_AliasesAndBlankEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolGetTranscripts: `{"transcripts":[{
			"meetingId":"alias-1",
			"transcript":[
				{"speakerName":"Alice Example","content":"Content alias","start":"0"},
				{"speaker":"Bob Example","words":"Words alias","startTimestamp":"65"},
				{"speaker":"Blank Example","text":"   ","start":70},
				{"speaker":"Also Blank","content":"\t","timestamp":75}
			]
		}]}`,
	}, &calls)

	transcripts, err := s.GetTranscripts(context.Background(), []string{"alias-1"})
	require.NoError(err)
	tr := transcripts["alias-1"]
	require.NotNil(tr)
	requireTranscriptClassification(t, tr, TranscriptPresent)
	body := buildBody(&Meeting{Name: "Aliases"}, tr)
	assert.Contains(body, "[00:00] Alice Example: Content alias")
	assert.Contains(body, "[01:05] Bob Example: Words alias")
	assert.NotContains(body, "Blank Example:")
	assert.NotContains(body, "Also Blank:")
}

func TestTranscript_RecognizedEmpty(t *testing.T) {
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolGetTranscripts: `{"transcripts":[
			{"id":"empty-list","transcript":[]},
			{"id":"blank-text","text":"  \n\t"}
		]}`,
	}, &calls)

	transcripts, err := s.GetTranscripts(context.Background(), []string{"empty-list", "blank-text"})
	require.NoError(err)
	requireTranscriptClassification(t, transcripts["empty-list"], TranscriptRecognizedEmpty)
	requireTranscriptClassification(t, transcripts["blank-text"], TranscriptRecognizedEmpty)
}

func TestTranscript_PresentContentWinsOverUnsupportedEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolGetTranscripts: `{"transcripts":[{
			"id":"mixed-content",
			"transcript":[
				{"speaker":"Alice Example","text":"Supported text","timestamp":0},
				{"speaker":"Bob Example","words":[{"word":"raw timing data"}],"timestamp":1}
			]
		}]}`,
	}, &calls)

	transcripts, err := s.GetTranscripts(context.Background(), []string{"mixed-content"})
	require.NoError(err)
	tr := transcripts["mixed-content"]
	requireTranscriptClassification(t, tr, TranscriptPresent)
	body := buildBody(&Meeting{Name: "Mixed content"}, tr)
	assert.Contains(body, "Supported text")
	assert.NotContains(body, "raw timing data")
	assert.NotEmpty(tr.Raw, "unsupported entries must remain in the raw archive")
}

func TestTranscript_MixedAbsoluteAndNumericOffsets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []contractToolCall
	s := newContractMCPSession(t, map[string]string{
		toolGetTranscripts: `{"transcripts":[{
			"id":"mixed-offsets",
			"transcript":[
				{"speaker":"Alice Example","text":"Absolute time","timestamp":"2026-06-10T17:00:00Z"},
				{"speaker":"Bob Example","text":"Numeric offset","startTimestamp":65}
			]
		}]}`,
	}, &calls)

	transcripts, err := s.GetTranscripts(context.Background(), []string{"mixed-offsets"})
	require.NoError(err)
	body := buildBody(&Meeting{Name: "Mixed offsets"}, transcripts["mixed-offsets"])
	assert.Contains(body, "[00:00] Alice Example: Absolute time")
	assert.Contains(body, "[01:05] Bob Example: Numeric offset")
}

func TestTranscript_ContractErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing identity",
			payload: `{"transcripts":[{"transcript":[]}]}`,
		},
		{
			name:    "unrecognized transcript shape",
			payload: `{"transcripts":[{"id":"meeting-7","segments":[]}]}`,
		},
		{
			name:    "unsupported entry text key",
			payload: `{"transcripts":[{"id":"meeting-7","transcript":[{"utterance":"hello"}]}]}`,
		},
		{
			name:    "non-string words",
			payload: `{"transcripts":[{"id":"meeting-7","transcript":[{"words":["hello"]}]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []contractToolCall
			s := newContractMCPSession(t, map[string]string{toolGetTranscripts: tc.payload}, &calls)

			_, err := s.GetTranscripts(context.Background(), []string{"meeting-7"})

			require.Error(t, err)
			assert.Contains(t, err.Error(), "contract")
		})
	}
}

func TestGetTranscripts_KeysByMeetingID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := newFakeMCPSession(t, map[string]string{
		toolGetTranscripts: `{"transcripts": [
			{"meetingId": 7, "transcript": [
				{"speaker": "Alice Smith", "text": "Hello everyone.", "start": 0},
				{"speaker": "Bob Jones", "text": "Morning!", "start": 4.5}
			]}
		]}`,
	})
	trs, err := s.GetTranscripts(context.Background(), []string{"7"})
	require.NoError(err)
	tr, ok := trs["7"]
	require.True(ok, "transcript keyed by meeting ID")
	require.Len(tr.Entries, 2)
	assert.Equal("Alice Smith", tr.Entries[0].SpeakerLabel())
	assert.Equal("Morning!", tr.Entries[1].Utterance())
}

func TestCallToolJSON_ServerErrorSurfaces(t *testing.T) {
	require := require.New(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	server.AddTool(&mcp.Tool{
		Name: toolReadMeetings, Description: "fake",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "no such meeting"}},
		}, nil
	})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = ss.Wait() })
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = cs.Close() })

	_, err = NewSessionForTesting(cs).ReadMeetings(context.Background(), []string{"1"})
	require.Error(err)
	require.Contains(err.Error(), "no such meeting")
}

func TestMeetingAccessors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var m Meeting
	require.NoError(json.Unmarshal([]byte(`{
		"id": 42,
		"title": "Planning",
		"createdAt": "2026-06-01T15:00:00Z",
		"startTime": "2026-06-01T15:02:00Z",
		"durationSeconds": "1800",
		"notes": "## Agenda",
		"meetingUrl": "https://meet.example.com/abc",
		"recordingUrl": "https://cdn.example.com/rec.mp4"
	}`), &m))
	assert.Equal("42", string(m.ID))
	assert.Equal("Planning", m.DisplayName())
	assert.Equal("## Agenda", m.NotesMarkdown())
	assert.Equal("https://meet.example.com/abc", m.PlatformURL())
	assert.EqualValues(1800, m.DurationSecs())
	assert.Equal("2026-06-01T15:02:00Z", m.StartedAt().Format("2006-01-02T15:04:05Z"))
	assert.Equal("2026-06-01T15:00:00Z", m.CreatedTime().Format("2006-01-02T15:04:05Z"))
}

func TestMeetingAccessors_AssigneeShapesPreserveFallbackJSON(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		display string
	}{
		{name: "string", raw: `"Bob Example"`, display: "Bob Example"},
		{name: "empty string", raw: `""`, display: ""},
		{name: "object", raw: `{"name":"Bob Example","email":"bob@example.com"}`, display: "Bob Example"},
		{name: "null", raw: `null`, display: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var assignee Assignee
			require.NoError(json.Unmarshal([]byte(tc.raw), &assignee))
			assert.Equal(tc.display, assignee.Display())

			encoded, err := json.Marshal(&assignee)
			require.NoError(err)
			assert.JSONEq(tc.raw, string(encoded))
		})
	}
}
