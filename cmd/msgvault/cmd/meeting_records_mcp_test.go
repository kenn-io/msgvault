package cmd

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func meetingRecordsMCPSession(t *testing.T, backend *daemonclient.Client, delegated bool, permissions []string) *sdkmcp.ClientSession {
	t.Helper()
	requirements := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeTransport(ctx, mcpserver.ServeOptions{Engine: daemonclient.NewEngineAdapter(backend), MeetingRecords: backend, ArchiveCalendarEvents: backend, DelegatedOnly: delegated, GrantPermissions: permissions}, serverTransport)
	}()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "meeting-record-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	t.Cleanup(func() {
		if session != nil {
			_ = session.Close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			require.Fail(t, "MCP server did not stop after cancellation")
		}
	})
	requirements.NoError(err)
	return session
}

func TestMeetingRecordsMCPThroughStoreAdapter(t *testing.T) {
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	note, notesSource := meetingRecordAdapterNote(t, f)
	participant, err := f.st.EnsureParticipant("guest@example.com", "Synthetic Guest", "example.com")
	requirements.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), participant, note)
	requirements.NoError(err)
	person, _, err := f.st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	grant, err := f.owner.IssueAgentToken(t.Context(), "meeting-reader", []string{"message.read"}, []int64{f.sourceID, notesSource}, nil, time.Time{})
	requirements.NoError(err)
	// The real daemon's delegated credential keeps source admission in the HTTP path.
	client := f.owner
	for _, delegated := range []bool{false, true} {
		t.Run(strconv.FormatBool(delegated), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			if delegated {
				var err error
				client, err = daemonclient.New(daemonclient.Config{URL: f.owner.BaseURL(), AgentToken: grant.Secret, AllowInsecure: true})
				requirements.NoError(err)
				t.Cleanup(func() { require.NoError(t, client.Close()) })
			}
			session := meetingRecordsMCPSession(t, client, delegated, []string{"message.read"})
			names := mcpDraftToolNames(t, session)
			for _, name := range []string{mcpserver.ToolGetMeeting, mcpserver.ToolListPersonMeetings, mcpserver.ToolSuggestMeetingLinks} {
				assertions.Contains(names, name)
			}
			assertions.NotContains(names, "link_meeting")
			assertions.NotContains(names, "unlink_meeting")
			for _, tc := range []struct {
				name string
				args map[string]any
			}{
				{mcpserver.ToolGetMeeting, map[string]any{"ref": fmt.Sprintf("evt:%d", f.eventID)}},
				{mcpserver.ToolListPersonMeetings, map[string]any{"ref": fmt.Sprintf("person:%d", person.ID), "from": "2027-03-01T00:00:00Z", "to": "2027-03-02T00:00:00Z"}},
				{mcpserver.ToolSuggestMeetingLinks, map[string]any{"ref": fmt.Sprintf("evt:%d", f.eventID)}},
			} {
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
				requirements.NoError(err)
				requirements.False(result.IsError)
				assertions.NotNil(result.StructuredContent)
				requirements.NotEmpty(result.Content)
				text, ok := result.Content[0].(*sdkmcp.TextContent)
				requirements.True(ok)
				if tc.name == mcpserver.ToolGetMeeting {
					assertions.Contains(text.Text, "Recorded evidence")
				}
				if tc.name == mcpserver.ToolListPersonMeetings {
					assertions.Contains(text.Text, `"item_count":1`)
				}
			}
		})
	}
}
