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
	"go.kenn.io/msgvault/internal/testutil"
)

type calendarEventsTestBackend struct {
	query store.CalendarOccurrenceQuery
	ref   string
	page  store.CalendarOccurrencePage
}

func (s *calendarEventsTestBackend) ListCalendarOccurrencesContext(_ context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	s.query = query
	return s.page, nil
}

func (s *calendarEventsTestBackend) GetCalendarOccurrenceContext(_ context.Context, ref string) (store.CalendarOccurrenceDetail, error) {
	s.ref = ref
	return store.CalendarOccurrenceDetail{}, nil
}

func TestCalendarArchiveToolsPermissionGatedReads(t *testing.T) {
	asOf := time.Date(2027, time.March, 1, 12, 0, 0, 0, time.UTC)
	coverage := store.CalendarCoverage{
		Complete: false,
		Notice:   "Archived calendar data, not live availability.",
		Sources: []store.CalendarSourceCoverage{{
			SourceID: 42, SourceType: "gcal", Status: "stale", AsOf: &asOf, LatestRunStatus: "completed",
		}},
	}
	for _, permission := range []string{"calendar.event.read", "message.read", "calendar.read"} {
		t.Run(permission, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := &calendarEventsTestBackend{page: store.CalendarOccurrencePage{Coverage: coverage}}
			opts := ServeOptions{Engine: &querytest.MockEngine{}, CalEvents: st, DelegatedOnly: true, GrantPermissions: []string{permission}}
			tools := map[string]bool{}
			for _, d := range operationCatalog(opts, nil) {
				tools[d.name] = true
				if d.name == ToolListCalendarEvents || d.name == ToolGetCalendarEvent {
					assertions.Contains(d.description, untrustedCalendarContent)
				}
				if d.name == ToolListCalendarEvents {
					assertions.Contains(d.description, "coverage.complete")
					assertions.Contains(d.description, "live free/busy")
				}
			}
			if permission == "calendar.read" {
				assertions.NotContains(tools, ToolListCalendarEvents)
				assertions.NotContains(tools, ToolGetCalendarEvent)
				return
			}
			assertions.Contains(tools, ToolListCalendarEvents)
			assertions.Contains(tools, ToolGetCalendarEvent)
			listed := rawCallTool(t, opts, ToolListCalendarEvents, map[string]any{"from": "2027-03-01T00:00:00Z", "to": "2027-03-02T00:00:00Z", "limit": float64(1)})
			requirements.NotEqual(true, listed["isError"], fmt.Sprint(listed["content"]))
			gotCoverage, ok := toolStructuredContent(t, listed)["coverage"].(map[string]any)
			requirements.True(ok, "calendar list response must include coverage")
			assertions.Equal(coverage.Complete, gotCoverage["complete"])
			assertions.Equal(coverage.Notice, gotCoverage["notice"])
			assertions.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), st.query.From)
			assertions.Equal(time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC), st.query.To)
			assertions.Equal(new(1), st.query.Limit)
			detail := rawCallTool(t, opts, ToolGetCalendarEvent, map[string]any{"ref": "evt:42"})
			assertions.NotEqual(true, detail["isError"], fmt.Sprint(detail["content"]))
			assertions.Equal("evt:42", st.ref)
		})
	}
}

func TestCalendarArchiveRejectsInvalidArguments(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, tt := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"from", ToolListCalendarEvents, map[string]any{"from": "", "to": "2027-03-03T00:00:00Z"}, ""},
		{"to", ToolListCalendarEvents, map[string]any{"from": "2027-03-01T00:00:00Z", "to": ""}, ""},
		{"string limit", ToolListCalendarEvents, map[string]any{"from": "2027-03-01T00:00:00Z", "to": "2027-03-03T00:00:00Z", "limit": "1"}, "integer"},
		{"fractional limit", ToolListCalendarEvents, map[string]any{"from": "2027-03-01T00:00:00Z", "to": "2027-03-03T00:00:00Z", "limit": 1.5}, "integer"},
		{"numeric cursor", ToolListCalendarEvents, map[string]any{"from": "2027-03-01T00:00:00Z", "to": "2027-03-03T00:00:00Z", "cursor": 1}, "string"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := rawCallTool(t, ServeOptions{CalEvents: st}, tt.tool, tt.args)
			assert.Equal(t, true, result["isError"], fmt.Sprint(result["content"]))
			assert.NotEmpty(t, result["content"])
			if tt.want != "" {
				assert.Contains(t, fmt.Sprint(result["content"]), tt.want)
			}
		})
	}
}
