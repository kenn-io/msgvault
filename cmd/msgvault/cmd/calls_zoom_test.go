package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/zoomcalls"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestZoomRosterThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	peer, err := st.EnsureParticipantByPhone(t.Context(), "+12025550109", "Synthetic peer", "zoom")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-key"}}
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	dir := t.TempDir()
	rosterPath, bindingsPath := filepath.Join(dir, "roster.json"), filepath.Join(dir, "bindings.json")
	require.NoError(os.WriteFile(bindingsPath, []byte(fmt.Sprintf(`{"peer":%d}`, peer)), 0600))
	run := func(raw string) {
		t.Helper()
		require.NoError(os.WriteFile(rosterPath, []byte(raw), 0600))
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&out)
		command.SetArgs([]string{"import-zoom-meeting", "--roster", rosterPath, "--bindings", bindingsPath, "--uuid", "instance", "--account", "account/owner", "--owner-id", "owner"})
		require.NoError(command.ExecuteContext(t.Context()), out.String())
		assert.Contains(out.String(), `"upserted":1`)
	}
	joined := `{"participants":[{"id":"owner","status":"in_meeting","join_time":"2026-10-01T10:00:00Z","leave_time":"2026-10-01T10:10:00Z"},{"id":"peer","status":"in_meeting","join_time":"2026-10-01T10:05:00Z","leave_time":"2026-10-01T10:15:00Z"}]}`
	for range 2 {
		run(joined)
	}
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), "call_history", (callhistory.Evidence{Provider: "zoom_meetings", Account: zoomcalls.AccountNamespace("account/owner", "owner")}).SourceIdentifier())
	require.NoError(err)
	assert.Equal("Zoom meetings — account/owner", source.DisplayName.String)
	q := store.CallQuery{PersonID: person.ID}
	page, err := client.SummarizeCalls(t.Context(), q)
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(int64(1), page.Items[0].Answered)
	observations, err := st.ListCallObservationsContext(t.Context(), store.CallQuery{})
	require.NoError(err)
	require.Len(observations, 1)
	rawBefore, err := st.GetMessageRawContext(t.Context(), observations[0].MessageID)
	require.NoError(err)
	require.NoError(os.WriteFile(rosterPath, []byte(`{"participants":[{}],"total_records":1}`), 0600))
	command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
	command.SetArgs([]string{"import-zoom-meeting", "--roster", rosterPath, "--bindings", bindingsPath, "--uuid", "instance", "--account", "account/owner", "--owner-id", "owner"})
	require.ErrorContains(command.ExecuteContext(t.Context()), "participant status")
	page, err = client.SummarizeCalls(t.Context(), q)
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(int64(1), page.Items[0].Answered)
	rawAfter, err := st.GetMessageRawContext(t.Context(), observations[0].MessageID)
	require.NoError(err)
	assert.Equal(rawBefore, rawAfter)
	run(`{"participants":[]}`)
	page, err = client.SummarizeCalls(t.Context(), q)
	require.NoError(err)
	assert.Empty(page.Items)
	run(joined)
	page, err = client.SummarizeCalls(t.Context(), q)
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(int64(1), page.Items[0].Answered)
}

// Generated daemon metadata pages exercise the real staging/parser path at its
// 10k boundary without creating ten thousand database records on a shared host.
type zoomMetadataClient struct {
	callCommandClient

	calls int
}

func (c *zoomMetadataClient) ListCallObservations(_ context.Context, q store.CallQuery) (*generated.CallEvidenceResponse, error) {
	c.calls++
	var page generated.CallEvidenceResponse
	for i := int64(1); i <= 100; i++ {
		id := q.AfterID + i
		record := fmt.Sprintf("unrelated-%d", id)
		page.Items = append(page.Items, generated.CallObservationView{MessageID: id, RecordID: record})
		item := &page.Items[len(page.Items)-1]
		item.Version = 1
		item.Outcome = "answered"
		item.OwnerParticipation = "joined"
		item.Direction = "unknown"
		item.OccurredAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		item.Provider = "zoom_meetings"
		item.Account = zoomcalls.AccountNamespace("account", "owner")
	}
	return &page, nil
}

func TestZoomReconciliationUnresolvedIDReachesBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := &zoomMetadataClient{}
	items := []zoomcalls.MeetingRecord{{UUID: "second", Roster: []byte(`{"participants":[]}`)}}
	_, _, err := stageZoomMeetings(t.Context(), client, zoomOptions{account: "account", owner: "owner"}, items, nil)
	require.ErrorContains(err, "10000")
	assert.Equal(100, client.calls)
}
