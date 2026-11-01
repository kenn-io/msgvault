package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/timeline"
)

type timelineTestBackend struct {
	req timeline.Request
	err error
}

func (f *timelineTestBackend) Timeline(_ context.Context, req timeline.Request) (*timeline.Timeline, string, error) {
	f.req = req
	return &timeline.Timeline{Mode: timeline.ModeDay}, "# Timeline\n", f.err
}
func TestTimelineToolAvailabilityAndFormats(t *testing.T) {
	for _, tc := range []struct {
		name        string
		delegated   bool
		permissions []string
		want        bool
	}{
		{"owner", false, nil, true}, {"message", true, []string{"message.read"}, true}, {"calendar", true, []string{"calendar.event.read"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			backend := &timelineTestBackend{}
			opts := ServeOptions{Engine: &querytest.MockEngine{}, Timeline: backend, DelegatedOnly: tc.delegated, GrantPermissions: tc.permissions}
			var found bool
			for _, d := range operationCatalog(opts, nil) {
				if d.name == ToolTimeline {
					found = true
					for _, convention := range []string{"untrusted_text", "hidden_text", "open", "↓", "↑", "UTC", "copies"} {
						assertions.Contains(d.description, convention)
					}
				}
			}
			require.Equal(t, tc.want, found)
			if !found {
				return
			}
			for _, format := range []string{"default", "markdown", "json"} {
				args := map[string]any{}
				if format != "default" {
					args["format"] = format
				}
				got := rawCallTool(t, opts, ToolTimeline, args)
				assertions.NotEqual(true, got["isError"], fmt.Sprint(got))
				wantFormat := format
				if format == "default" {
					wantFormat = "markdown"
				}
				assertions.Equal(wantFormat, backend.req.Format)
				if wantFormat == "markdown" {
					assertions.NotContains(got, "structuredContent")
					assertions.Equal([]any{map[string]any{"type": "text", "text": "# Timeline\n"}}, got["content"])
				} else {
					assertions.Contains(got, "structuredContent")
				}
				assertions.Contains(fmt.Sprint(got), map[string]string{"markdown": "# Timeline", "json": "day"}[wantFormat])
			}
			backend.err = &timeline.ErrAmbiguousReference{Candidates: []timeline.Candidate{{PersonID: 7, DisplayName: "Alice"}}}
			got := rawCallTool(t, opts, ToolTimeline, map[string]any{"with": "Alice"})
			assertions.Equal(true, got["isError"])
			assertions.Contains(fmt.Sprint(got["structuredContent"]), "Alice")
		})
	}
}

func TestTimelineOpenTargetsCallableByCaller(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	at := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSentAt(at).WithSnippet("Synthetic message").Build()
	message.SenderID = sql.NullInt64{Int64: peer, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	requirements.NoError(err)
	requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
	requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC"})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	calendarIDs, _ := testutil.CalendarArchive(t, f.Store)
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "synthetic-owner"
	cfg.Server.AgentAccess = true
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
	body, err := json.Marshal(map[string]any{"label": "synthetic-reader", "permissions": []string{"message.read", "search.read"}, "source_ids": append([]int64{f.Source.ID}, calendarIDs...)})
	requirements.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(body))
	request.Header.Set("X-Api-Key", cfg.Server.APIKey)
	request.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, request)
	requirements.Equal(http.StatusCreated, rec.Code, rec.Body.String())
	var issued struct {
		Secret string `json:"secret"`
	}
	requirements.NoError(json.Unmarshal(rec.Body.Bytes(), &issued))
	requirements.NotEmpty(issued.Secret)
	srv := httptest.NewServer(server.Router())
	t.Cleanup(srv.Close)
	for _, delegated := range []bool{false, true} {
		t.Run(strconv.FormatBool(delegated), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			dc := daemonclient.Config{URL: srv.URL, AllowInsecure: true}
			if delegated {
				dc.AgentToken = issued.Secret
			} else {
				dc.APIKey = cfg.Server.APIKey
			}
			client, err := daemonclient.New(dc)
			requirements.NoError(err)
			opts := ServeOptions{Engine: daemonclient.NewEngineAdapter(client), Timeline: client, CalEvents: client, DelegatedOnly: delegated, GrantPermissions: []string{"message.read", "search.read"}}
			result := rawCallTool(t, opts, ToolTimeline, map[string]any{"date": "2027-03-01", "format": "json"})
			requirements.NotEqual(true, result["isError"], fmt.Sprint(result))
			wire, err := json.Marshal(result["structuredContent"])
			requirements.NoError(err)
			var got timeline.Timeline
			requirements.NoError(json.Unmarshal(wire, &got))
			var opened []string
			for _, day := range got.Days {
				for _, entries := range [][]timeline.Entry{day.Bursts, day.Meetings, day.Calls} {
					for _, entry := range entries {
						for _, open := range entry.Open {
							call := rawCallTool(t, opts, open.Tool, open.Args)
							assertions.NotEqual(true, call["isError"], fmt.Sprint(call))
							opened = append(opened, open.Tool)
						}
					}
				}
			}
			assertions.Contains(opened, "get_message")
			assertions.Contains(opened, "list_messages")
			assertions.Contains(opened, "get_calendar_event")
		})
	}
}

func TestTimelineToolPreservesNeighborPresence(t *testing.T) {
	assertions := assert.New(t)
	backend := &timelineTestBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, Timeline: backend}
	for _, args := range []map[string]any{{}, {"before": 0}, {"after": 0}, {"before": 0, "after": 0}} {
		got := rawCallTool(t, opts, ToolTimeline, args)
		require.NotEqual(t, true, got["isError"], fmt.Sprint(got))
		_, before := args["before"]
		_, after := args["after"]
		assertions.Equal(before, backend.req.BeforeSet)
		assertions.Equal(after, backend.req.AfterSet)
		assertions.Zero(backend.req.Before)
		assertions.Zero(backend.req.After)
	}
}
