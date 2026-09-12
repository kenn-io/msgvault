package daemonclient_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func newMeetingDaemonClient(t *testing.T) (*daemonclient.Client, []meetingimport.Result) {
	t.Helper()
	st := testutil.NewTestStore(t)
	importer := meetingimport.NewImporter(st, meetingimport.Hooks{})
	actions := []meetingimport.MeetingActionItem{
		{
			SourceID: "action-1", Title: "Send draft", Description: "Send the reviewed draft",
			AssigneeName: "Alice", AssigneeEmail: "alice@example.com", Status: "open",
		},
		{SourceID: "action-2", Title: "Archive notes", Status: "done"},
	}
	unrelatedActions := []meetingimport.MeetingActionItem{
		{SourceID: "action-unrelated", Title: "Send unrelated draft", AssigneeEmail: "alice@example.com", Status: "open"},
	}
	requests := []meetingimport.Request{
		{
			Source: meetingimport.Source{Identifier: "meeting-client-fixture", AccountEmail: "owner@example.com"},
			Meeting: meetingimport.Meeting{
				ExternalID: "planning", Title: "Quarterly planning",
				StartedAt: "2026-01-15T10:00:00Z", EndedAt: "2026-01-15T10:30:00Z",
				SummaryText: "Agreed on the launch sequence.", Transcript: "Alice: send the reviewed draft.",
				Organizer:   &meetingimport.MeetingPerson{Name: "Owner", Email: "owner@example.com"},
				Attendees:   []meetingimport.MeetingPerson{{Name: "Alice", Email: "alice@example.com"}},
				ActionItems: &actions,
			},
		},
		{
			Source: meetingimport.Source{Identifier: "meeting-client-unrelated", AccountEmail: "owner@other.example"},
			Meeting: meetingimport.Meeting{
				ExternalID: "undated-duration", Title: "Follow-up review",
				StartedAt: "2026-02-03T09:00:00Z", SummaryText: "Reviewed open work.",
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

func TestMeetingClientReadsStoreBackedDaemonThroughGeneratedSDK(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	client, imported := newMeetingDaemonClient(t)

	messageIDs := []int64{imported[0].MessageID}
	format := generated.Markdown
	includeTranscript := true
	maxBytes := int64(32 * 1024)
	packet, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{
		MessageIds: &messageIDs, Format: &format,
		IncludeTranscript: &includeTranscript, MaxBytes: &maxBytes,
	})
	must.NoError(err)
	must.NotNil(packet)
	checks.Equal("markdown", string(packet.Format))
	checks.Contains(packet.Content, "Quarterly planning")
	checks.Contains(packet.Content, "Alice: send the reviewed draft.")

	status := generated.MeetingActionsRequestStatusPending
	assignee := "alice@example.com"
	limit := int64(10)
	allActions, err := client.ListMeetingActionItems(t.Context(), generated.ListMeetingActionItemsBody{
		AssigneeEmail: &assignee, Status: &status, Limit: &limit,
	})
	must.NoError(err)
	must.NotNil(allActions)
	must.Len(allActions.Rows, 2)
	checks.ElementsMatch([]string{"Send draft", "Send unrelated draft"}, []string{
		allActions.Rows[0].Action.Title, allActions.Rows[1].Action.Title,
	})

	actions, err := client.ListMeetingActionItems(t.Context(), generated.ListMeetingActionItemsBody{
		Scope:         &generated.MeetingScopeRequest{Domains: []string{"example.com"}},
		AssigneeEmail: &assignee, Status: &status, Limit: &limit,
	})
	must.NoError(err)
	must.NotNil(actions)
	must.Len(actions.Rows, 1)
	checks.Equal("Send draft", actions.Rows[0].Action.Title)
	checks.Equal("pending", string(actions.Rows[0].Action.Status))
	checks.Equal("Quarterly planning", actions.Rows[0].Meeting.Title)
	checks.NotEqual(imported[0].SourceID, imported[1].SourceID)

	metrics, err := client.GetMeetingMetrics(t.Context(), generated.GetMeetingMetricsBody{})
	must.NoError(err)
	must.NotNil(metrics)
	checks.Equal(int64(2), metrics.Totals.MeetingCount)
	checks.Equal(int64(1), metrics.Totals.KnownDurationCount)
	checks.Equal(int64(1), metrics.Totals.UnknownDurationCount)
	must.NotNil(metrics.Totals.AverageKnownSeconds)
	checks.InDelta(1800, *metrics.Totals.AverageKnownSeconds, 0)
	checks.Equal([]string{"2026-01", "2026-02"}, []string{metrics.Months[0].Month, metrics.Months[1].Month})

	metrics, err = client.GetMeetingMetrics(t.Context(), generated.GetMeetingMetricsBody{
		Scope: &generated.MeetingScopeRequest{SourceIds: []int64{imported[0].SourceID}},
	})
	must.NoError(err)
	must.NotNil(metrics)
	checks.Equal(int64(1), metrics.Totals.MeetingCount)
	checks.Equal(int64(1), metrics.Totals.KnownDurationCount)
	checks.Zero(metrics.Totals.UnknownDurationCount)
	must.NotNil(metrics.Totals.AverageKnownSeconds)
	checks.InDelta(1800, *metrics.Totals.AverageKnownSeconds, 0)
	must.Len(metrics.Months, 1)
	checks.Equal("2026-01", metrics.Months[0].Month)
}

func TestMeetingClientPreservesExplicitEmptyScopeAndNullableMetrics(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client, _ := newMeetingDaemonClient(t)
	empty := []int64{}

	metrics, err := client.GetMeetingMetrics(t.Context(), generated.GetMeetingMetricsBody{
		Scope: &generated.MeetingScopeRequest{MessageIds: &empty},
	})
	requirements.NoError(err)
	requirements.NotNil(metrics)
	assertions.Zero(metrics.Totals.MeetingCount)
	assertions.Nil(metrics.Totals.AverageKnownSeconds)
	assertions.Empty(metrics.Months)
}

func TestMeetingClientPropagatesDaemonErrorsAndRequestContext(t *testing.T) {
	t.Run("semantic HTTP error", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte(`{"error":"meeting_store_unavailable","message":"meeting store unavailable"}`))
			assert.NoError(t, err)
		}))
		t.Cleanup(server.Close)
		client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
		requirements.NoError(err)

		result, err := client.GetMeetingMetrics(t.Context(), generated.GetMeetingMetricsBody{})
		requirements.Error(err)
		assertions.Nil(result)
		var apiErr *daemonclient.APIError
		requirements.ErrorAs(err, &apiErr)
		assertions.Equal(http.StatusServiceUnavailable, apiErr.Status)
		assertions.Equal("meeting_store_unavailable", apiErr.APIErrorCode())
	})

	t.Run("canceled context", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		requestStarted := make(chan struct{})
		releaseHandler := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(requestStarted)
			select {
			case <-r.Context().Done():
			case <-releaseHandler:
			}
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(releaseHandler) })
		client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
		requirements.NoError(err)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-requestStarted
			cancel()
		}()

		result, err := client.GetMeetingMetrics(ctx, generated.GetMeetingMetricsBody{})
		requirements.Error(err)
		assertions.Nil(result)
		requirements.ErrorIs(err, context.Canceled)
	})
}
