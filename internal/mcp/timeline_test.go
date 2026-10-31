package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/timeline"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
			assert := assertpkg.New(t)
			backend := &timelineTestBackend{}
			opts := ServeOptions{Engine: &querytest.MockEngine{}, Timeline: backend, DelegatedOnly: tc.delegated, GrantPermissions: tc.permissions}
			var found bool
			for _, d := range operationCatalog(opts, nil) {
				if d.name == ToolTimeline {
					found = true
					for _, convention := range []string{"untrusted_text", "hidden_text", "open", "↓", "↑", "UTC", "copies"} {
						assert.Contains(d.description, convention)
					}
				}
			}
			requirepkg.Equal(t, tc.want, found)
			if !found {
				return
			}
			for _, format := range []string{"default", "markdown", "json"} {
				args := map[string]any{}
				if format != "default" {
					args["format"] = format
				}
				got := rawCallTool(t, opts, ToolTimeline, args)
				assert.NotEqual(true, got["isError"], fmt.Sprint(got))
				wantFormat := format
				if format == "default" {
					wantFormat = "markdown"
				}
				assert.Equal(wantFormat, backend.req.Format)
				if wantFormat == "markdown" {
					assert.NotContains(got, "structuredContent")
					assert.Equal([]any{map[string]any{"type": "text", "text": "# Timeline\n"}}, got["content"])
				} else {
					assert.Contains(got, "structuredContent")
				}
				assert.Contains(fmt.Sprint(got), map[string]string{"markdown": "# Timeline", "json": "day"}[wantFormat])
			}
			backend.err = &timeline.ErrAmbiguousReference{Candidates: []timeline.Candidate{{PersonID: 7, DisplayName: "Alice"}}}
			got := rawCallTool(t, opts, ToolTimeline, map[string]any{"with": "Alice"})
			assert.Equal(true, got["isError"])
			assert.Contains(fmt.Sprint(got["structuredContent"]), "Alice")
		})
	}
}

func TestTimelineOpenTargetsCallableByCaller(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	at := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	message := f.NewMessage().WithSentAt(at).WithSnippet("Synthetic message").Build()
	message.SenderID = sql.NullInt64{Int64: peer, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC"})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	calendarIDs, _ := testutil.CalendarArchive(t, f.Store)
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "synthetic-owner"
	cfg.Server.AgentAccess = true
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirepkg.NoError(t, server.Shutdown(context.Background())) })
	body, err := json.Marshal(map[string]any{"label": "synthetic-reader", "permissions": []string{"message.read", "search.read"}, "source_ids": append([]int64{f.Source.ID}, calendarIDs...)})
	require.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(body))
	request.Header.Set("X-Api-Key", cfg.Server.APIKey)
	request.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, request)
	require.Equal(http.StatusCreated, rec.Code, rec.Body.String())
	var issued struct {
		Secret string `json:"secret"`
	}
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &issued))
	require.NotEmpty(issued.Secret)
	srv := httptest.NewServer(server.Router())
	t.Cleanup(srv.Close)
	for _, delegated := range []bool{false, true} {
		t.Run(fmt.Sprint(delegated), func(t *testing.T) {
			assert := assertpkg.New(t)
			require := requirepkg.New(t)
			dc := daemonclient.Config{URL: srv.URL, AllowInsecure: true}
			if delegated {
				dc.AgentToken = issued.Secret
			} else {
				dc.APIKey = cfg.Server.APIKey
			}
			client, err := daemonclient.New(dc)
			require.NoError(err)
			opts := ServeOptions{Engine: daemonclient.NewEngineAdapter(client), Timeline: client, CalEvents: client, DelegatedOnly: delegated, GrantPermissions: []string{"message.read", "search.read"}}
			result := rawCallTool(t, opts, ToolTimeline, map[string]any{"date": "2027-03-01", "format": "json"})
			require.NotEqual(true, result["isError"], fmt.Sprint(result))
			wire, err := json.Marshal(result["structuredContent"])
			require.NoError(err)
			var got timeline.Timeline
			require.NoError(json.Unmarshal(wire, &got))
			var opened []string
			for _, day := range got.Days {
				for _, entries := range [][]timeline.Entry{day.Bursts, day.Meetings, day.Calls} {
					for _, entry := range entries {
						for _, open := range entry.Open {
							call := rawCallTool(t, opts, open.Tool, open.Args)
							assert.NotEqual(true, call["isError"], fmt.Sprint(call))
							opened = append(opened, open.Tool)
						}
					}
				}
			}
			assert.Contains(opened, "get_message")
			assert.Contains(opened, "list_messages")
			assert.Contains(opened, "get_calendar_event")
		})
	}
}

func TestTimelineToolPreservesNeighborPresence(t *testing.T) {
	assert := assertpkg.New(t)
	backend := &timelineTestBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, Timeline: backend}
	for _, args := range []map[string]any{{}, {"before": 0}, {"after": 0}, {"before": 0, "after": 0}} {
		got := rawCallTool(t, opts, ToolTimeline, args)
		requirepkg.NotEqual(t, true, got["isError"], fmt.Sprint(got))
		_, before := args["before"]
		_, after := args["after"]
		assert.Equal(before, backend.req.BeforeSet)
		assert.Equal(after, backend.req.AfterSet)
		assert.Zero(backend.req.Before)
		assert.Zero(backend.req.After)
	}
}
