package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestMeetingRecordsCatalogAndDelegatedPermissions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, permission := range []string{"message.read", "calendar.read", "calendar.event.read"} {
		defs := operationCatalog(ServeOptions{Engine: &querytest.MockEngine{}, MeetingRecords: &daemonclient.Client{}, DelegatedOnly: true, GrantPermissions: []string{permission}}, &handlers{})
		found := map[string]toolDefinition{}
		for _, d := range defs {
			found[d.name] = d
		}
		for _, name := range []string{ToolGetMeeting, ToolListPersonMeetings, ToolSuggestMeetingLinks} {
			d, ok := found[name]
			if permission != "message.read" {
				assertions.False(ok)
				continue
			}
			requirements.True(ok)
			assertions.NotNil(d.inputSchema)
			assertions.NotNil(d.outputSchema)
			p, read := agentReadToolPermission(name)
			assertions.True(read)
			assertions.Equal(agentgrant.PermissionMessageRead, p)
		}
	}
	defs := operationCatalog(ServeOptions{ArchiveCalendarEvents: &calendarEventsTestBackend{}}, &handlers{})
	names := []string{}
	for _, d := range defs {
		names = append(names, d.name)
	}
	assertions.Contains(names, ToolListCalendarEvents)
	assertions.Contains(names, ToolGetCalendarEvent)
	for _, name := range []string{ToolGetMeeting, ToolListPersonMeetings, ToolSuggestMeetingLinks} {
		assertions.NotContains(names, name)
	}
}
