package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type calendarEventsTestBackend struct{ from, to time.Time }

func (s *calendarEventsTestBackend) ListCalendarOccurrencesContext(_ context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	s.from, s.to = from, to
	return store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{Subject: "Planning"}}}, nil
}

func TestCalendarEventsTool(t *testing.T) {
	for _, tc := range []struct {
		permission string
		available  bool
	}{{"calendar.event.read", true}, {"message.read", false}, {"calendar.read", false}} {
		t.Run(tc.permission, func(t *testing.T) {
			assertions := assert.New(t)
			backend := &calendarEventsTestBackend{}
			opts := ServeOptions{Engine: &querytest.MockEngine{}, CalEvents: backend, DelegatedOnly: true, GrantPermissions: []string{tc.permission}}
			tools := map[string]bool{}
			for _, d := range operationCatalog(opts, nil) {
				tools[d.name] = true
			}
			assertions.Equal(tc.available, tools[ToolListCalendarEvents])
			if !tc.available {
				return
			}
			result := rawCallTool(t, opts, ToolListCalendarEvents, map[string]any{"from": "2027-03-01T00:00:00Z", "to": "2027-03-02T00:00:00Z"})
			require.NotEqual(t, true, result["isError"], fmt.Sprint(result["content"]))
			assertions.Contains(fmt.Sprint(result), "Planning")
			assertions.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), backend.from)
			assertions.Equal(time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC), backend.to)
		})
	}
}
