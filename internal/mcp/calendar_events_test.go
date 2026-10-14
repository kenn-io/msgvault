package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type calendarEventsTestBackend struct{}

func (*calendarEventsTestBackend) ListCalendarEvents(context.Context, store.CalendarOccurrenceFilter) (store.CalendarOccurrencePage, error) {
	return store.CalendarOccurrencePage{Items: []store.CalendarOccurrence{}, Unexpandable: []store.UnexpandableSeries{}}, nil
}
func (*calendarEventsTestBackend) GetCalendarEvent(context.Context, string) (store.CalendarOccurrence, error) {
	return store.CalendarOccurrence{}, nil
}

func TestCalendarEventsCatalog(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	defs := operationCatalog(ServeOptions{Engine: &querytest.MockEngine{}, ArchiveCalendarEvents: &calendarEventsTestBackend{}}, &handlers{})
	found := map[string]toolDefinition{}
	for _, d := range defs {
		found[d.name] = d
	}
	for _, name := range []string{ToolListCalendarEvents, ToolGetCalendarEvent} {
		d, ok := found[name]
		requirements.True(ok)
		assertions.NotNil(d.inputSchema)
		assertions.NotNil(d.outputSchema)
		permission, ok := agentReadToolPermission(name)
		requirements.True(ok)
		assertions.Equal(agentgrant.PermissionMessageRead, permission)
	}
}

func TestCalendarEventsDelegatedCatalog(t *testing.T) {
	for _, permission := range []string{"message.read", "calendar.read", "calendar.event.read"} {
		defs := operationCatalog(ServeOptions{Engine: &querytest.MockEngine{}, ArchiveCalendarEvents: &calendarEventsTestBackend{}, DelegatedOnly: true, GrantPermissions: []string{permission}}, &handlers{})
		var archiveNames []string
		for _, d := range defs {
			if d.name == ToolListCalendarEvents || d.name == ToolGetCalendarEvent {
				archiveNames = append(archiveNames, d.name)
			}
		}
		if permission == "message.read" {
			assert.Len(t, archiveNames, 2)
		} else {
			assert.Empty(t, archiveNames)
		}
	}
}

func TestCalendarEventsRoundTrip(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"primary","time_zone":"UTC"}`))
	conv, err := st.EnsureConversation(src.ID, "weekly", "Planning")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "weekly", MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`), "gcal_json"))
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)
	backend, err := daemonclient.New(daemonclient.Config{URL: httpServer.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, ArchiveCalendarEvents: backend}, false).Connect(ctx, serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: ToolListCalendarEvents, Arguments: map[string]any{"from": "2027-03-01T00:00:00Z", "to": "2027-03-15T00:00:00Z"}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	structured := meetingStructuredContent(t, result)
	items, ok := structured["items"].([]any)
	requirements.True(ok)
	requirements.Len(items, 2)
	ref := fmt.Sprintf("evt:%d@20270308T100000Z", id)
	result, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: ToolGetCalendarEvent, Arguments: map[string]any{"ref": ref}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	assert.Equal(t, ref, meetingStructuredContent(t, result)["ref"])
}
