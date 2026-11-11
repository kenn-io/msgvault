package cmd

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMeetingSubtitleUnknownOriginPreservesFile(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("granola", "owner@example.test")
	require.NoError(t, err)
	snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "one", SourceConversationID: "one", Title: "Meeting", StartedAt: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), Raw: []byte(`{"transcript":[{"speaker":{"name":"A"},"text":"Hello","start_time":"2026-01-01T10:00:02Z","end_time":"2026-01-01T10:00:04Z"}]}`), RawFormat: "granola_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
	archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	require.NoError(t, err)
	server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	output := filepath.Join(t.TempDir(), "meeting.srt")
	require.NoError(t, os.WriteFile(output, []byte("keep"), 0600))
	stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "srt", "-o", output)
	require.ErrorContains(t, err, "recording start")
	assert.Empty(t, stdout)
	saved, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(saved))
}

func TestMeetingSubtitleTwentyCLIAndHTTP(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("twenty", "owner@example.test")
	require.NoError(t, err)
	raw := []byte(`{"schema_version":1,"calendar_event":{"startsAt":"2025-01-01T00:00:00Z"},"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"participant":{"name":"A"},"words":[{"text":"Hello","start_timestamp":{"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`)
	snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "two", SourceConversationID: "two", Title: "Meeting", StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Raw: raw, RawFormat: "twenty_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
	archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	require.NoError(t, err)
	server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	want := "WEBVTT\n\n1\n00:00:10.000 --> 00:00:12.000\nA: Hello\n\n"
	format := generated.MeetingContextRequestFormat("vtt")
	result, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID, archived.MessageID}), Format: &format})
	require.NoError(t, err)
	assert.Equal(t, want, result.Content)
	stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "vtt", "--include-transcript=false")
	require.NoError(t, err)
	assert.Equal(t, want, stdout)
}
