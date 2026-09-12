package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newMeetingCommandClient(t *testing.T) (*daemonclient.Client, []meetingimport.Result) {
	t.Helper()
	st := testutil.NewTestStore(t)
	importer := meetingimport.NewImporter(st, meetingimport.Hooks{})
	actions := []meetingimport.MeetingActionItem{
		{SourceID: "send", Title: "Send draft", AssigneeEmail: "alice@example.com", Status: "open"},
		{SourceID: "done", Title: "Publish notes", Status: "completed"},
	}
	unrelatedActions := []meetingimport.MeetingActionItem{
		{SourceID: "outside", Title: "Send outside draft", AssigneeEmail: "alice@example.com", Status: "open"},
	}
	requests := []meetingimport.Request{
		{
			Source: meetingimport.Source{Identifier: "meeting-command-fixture", AccountEmail: "owner@example.com"},
			Meeting: meetingimport.Meeting{
				ExternalID: "one", Title: "Planning review", StartedAt: "2026-03-10T10:00:00Z",
				EndedAt: "2026-03-10T10:45:00Z", SummaryText: "Selected the launch owner.",
				Transcript: "Alice: I will send the draft.", ActionItems: &actions,
				Organizer: &meetingimport.MeetingPerson{Name: "Owner", Email: "owner@example.com"},
				Attendees: []meetingimport.MeetingPerson{{Name: "Alice", Email: "alice@example.com"}},
			},
		},
		{
			Source: meetingimport.Source{Identifier: "meeting-command-unrelated", AccountEmail: "owner@other.example"},
			Meeting: meetingimport.Meeting{
				ExternalID: "two", Title: "Status review", StartedAt: "2026-04-11T10:00:00Z",
				SummaryText: "Reviewed progress.",
				Organizer:   &meetingimport.MeetingPerson{Name: "Other Owner", Email: "owner@other.example"},
				Attendees:   []meetingimport.MeetingPerson{{Name: "Bob", Email: "bob@other.example"}},
				ActionItems: &unrelatedActions,
			},
		},
	}
	results := make([]meetingimport.Result, len(requests))
	for i, request := range requests {
		var err error
		results[i], err = importer.Import(t.Context(), request)
		require.NoError(t, err)
	}
	server := httptest.NewServer(api.NewServer(
		&config.Config{}, st, nil, slog.New(slog.DiscardHandler),
	).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	return client, results
}

func meetingTestInt64(value int64) string { return strconv.FormatInt(value, 10) }

func executeMeetingsCommand(
	t *testing.T, client meetingCommandClient, args ...string,
) (string, string, error) {
	t.Helper()
	command := newMeetingsCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) {
		return client, func() {}, nil
	}})
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs(args)
	err := command.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

func TestMeetingsCommandsUseDaemonResultsAndFilters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client, imported := newMeetingCommandClient(t)

	_, _, err := executeMeetingsCommand(t, client,
		"context", "--id", "999999", "--format", "markdown")
	requirements.Error(err)
	requirements.ErrorContains(err, "not found")

	messageIDs := []int64{imported[0].MessageID}
	format := generated.Markdown
	includeTranscript := true
	expected, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{
		MessageIds: &messageIDs, Format: &format, IncludeTranscript: &includeTranscript,
	})
	requirements.NoError(err)
	stdout, stderr, err := executeMeetingsCommand(t, client,
		"context", "--id", meetingTestInt64(imported[0].MessageID), "--include-transcript", "--format", "markdown")
	requirements.NoError(err)
	assertions.Empty(stderr)
	assertions.Equal(expected.Content, stdout)
	assertions.Contains(stdout, "Planning review")
	assertions.Contains(stdout, "Alice: I will send the draft.")

	stdout, _, err = executeMeetingsCommand(t, client,
		"actions", "--assignee", "alice@example.com", "--status", "pending", "--json")
	requirements.NoError(err)
	assertions.Contains(stdout, `"title":"Send draft"`)
	assertions.Contains(stdout, `"title":"Send outside draft"`)
	assertions.NotContains(stdout, "Publish notes")

	stdout, _, err = executeMeetingsCommand(t, client,
		"actions", "--domain", "example.com", "--assignee", "alice@example.com", "--status", "pending", "--json")
	requirements.NoError(err)
	assertions.Contains(stdout, `"title":"Send draft"`)
	assertions.NotContains(stdout, "Send outside draft")
	assertions.NotContains(stdout, "Publish notes")
	assertions.NotEqual(imported[0].SourceID, imported[1].SourceID)
	stdout, _, err = executeMeetingsCommand(t, client, "metrics")
	requirements.NoError(err)
	assertions.Contains(stdout, "Meetings: 2")
	assertions.Contains(stdout, "Known duration: 1")
	assertions.Contains(stdout, "Unknown duration: 1")
	assertions.Contains(stdout, "2026-03")
	assertions.Contains(stdout, "2026-04")

	stdout, _, err = executeMeetingsCommand(t, client,
		"metrics", "--source-id", meetingTestInt64(imported[0].SourceID))
	requirements.NoError(err)
	assertions.Contains(stdout, "Meetings: 1")
	assertions.Contains(stdout, "Known duration: 1")
	assertions.Contains(stdout, "Unknown duration: 0")
	assertions.Contains(stdout, "provider")
	assertions.Contains(stdout, "2026-03")
	assertions.NotContains(stdout, "2026-04")
}

func TestMeetingContextWritesExactContentAtomically(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client, imported := newMeetingCommandClient(t)
	output := filepath.Join(t.TempDir(), "context.md")
	messageIDs := []int64{imported[0].MessageID}
	format := generated.Markdown
	expected, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{
		MessageIds: &messageIDs, Format: &format,
	})
	requirements.NoError(err)

	stdout, stderr, err := executeMeetingsCommand(t, client,
		"context", "--id", meetingTestInt64(imported[0].MessageID), "--format", "markdown", "--output", output)
	requirements.NoError(err)
	assertions.Empty(stdout)
	assertions.Contains(stderr, output)
	content, err := os.ReadFile(output)
	requirements.NoError(err)
	assertions.Equal(expected.Content, string(content))
	assertions.Contains(string(content), "Planning review")
}

func TestMeetingsCommandsFailClosedForOlderDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	routeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"2.23.0"}`))
			assert.NoError(t, err)
			return
		}
		routeCalls++
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	requirements.NoError(err)

	_, _, err = executeMeetingsCommand(t, client, "metrics", "--json")
	requirements.Error(err)
	requirements.ErrorContains(err, "API schema 2.25.0 or newer")
	requirements.ErrorContains(err, "upgrade the daemon")
	assertions.Zero(routeCalls)
}

func TestMeetingsCommandsReportMissingDaemonRoute(t *testing.T) {
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"2.25.0"}`))
			assert.NoError(t, err)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, err := w.Write([]byte(`{"error":"not_found","message":"route not found"}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })

	_, _, err = executeMeetingsCommand(t, client, "metrics", "--json")
	requirements.Error(err)
	requirements.ErrorContains(err, "meeting intelligence is unavailable")
	requirements.ErrorContains(err, "upgrade it to API schema 2.25.0 or newer")
}
