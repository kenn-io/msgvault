package cmd

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestMeetingSubtitleTwentyCLIAndHTTP(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("twenty", "owner@example.test")
	must.NoError(err)
	raw := []byte(`{"schema_version":1,"calendar_event":{"startsAt":"2025-01-01T00:00:00Z"},"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"participant":{"name":"A"},"words":[{"text":"Hello","start_timestamp":{"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`)
	snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "two", SourceConversationID: "two", Title: "Meeting", StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Raw: raw, RawFormat: "twenty_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
	archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	must.NoError(err)
	server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(client.Close()) })
	want := "WEBVTT\n\n1\n00:00:10.000 --> 00:00:12.000\nA: Hello\n\n"
	format := generated.MeetingContextRequestFormat("vtt")
	result, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID, archived.MessageID}), Format: &format})
	must.NoError(err)
	checks.Equal(want, result.Content)
	stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "vtt")
	must.NoError(err)
	checks.Equal(want, stdout)
	snapshot.Raw = []byte(strings.Replace(string(raw), `"Hello"`, `"`+strings.Repeat("x", 5000)+`"`, 1))
	_, err = meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	must.NoError(err)
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "vtt", "--max-bytes", "4096")
	must.ErrorContains(err, "max_bytes")
	checks.Empty(stdout)
}

func TestMeetingSubtitleWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, raw, format, want string
	}{
		{"omi", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello\n  ordinary speech","start":1,"end":2}]}`, "srt", "1\n00:00:01,000 --> 00:00:02,000\nUnknown speaker: Hello\nordinary speech\n\n"},
		{"pocket-vtt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Hello  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Hello  ","start":0,"end":2}]}}}`, "vtt", "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Hello  \n\n"},
		{"pocket-srt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Hello  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Hello  ","start":0,"end":2}]}}}`, "srt", "1\n00:00:00,000 --> 00:00:02,000\nHello\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			st := testutil.NewTestStore(t)
			sourceName := "pocket"
			if tc.name == "omi" {
				sourceName = tc.name
			}
			source, err := st.GetOrCreateSource(sourceName, "owner@example.test")
			must.NoError(err)
			format := sourceName + "_json"
			snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: tc.name, SourceConversationID: tc.name, Title: "Meeting", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Raw: []byte(tc.raw), RawFormat: format, Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
			archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
			must.NoError(err)
			server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			must.NoError(err)
			t.Cleanup(func() { checks.NoError(client.Close()) })
			output := filepath.Join(t.TempDir(), "meeting")
			must.NoError(os.WriteFile(output, []byte("keep"), 0600))
			stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", tc.format, "-o", output)
			must.NoError(err)
			saved, err := os.ReadFile(output)
			must.NoError(err)
			checks.Equal(tc.want, string(saved))
			checks.Empty(stdout)
			requestFormat := generated.MeetingContextRequestFormat(tc.format)
			result, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID}), Format: &requestFormat})
			must.NoError(err)
			checks.Equal(tc.want, result.Content)
		})
	}
}

func TestMeetingSubtitleMarkupPreservesFile(t *testing.T) {
	for _, raw := range []string{
		`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Please type <i> literally</i>","start":0,"end":2}]}`,
		`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"speaker_name":"<i>A</i>","text":"Hello","start":0,"end":2}]}`,
		`{"started_at":"2026-01-01T00:00:00Z","transcript_segments":[{"text":"Hello\n00:00:05,000 --> 00:00:06,000\nWorld","start":0,"end":2}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("omi", "owner@example.test")
			must.NoError(err)
			snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "one", SourceConversationID: "one", Raw: []byte(raw), RawFormat: "omi_json"}
			archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
			must.NoError(err)
			server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			must.NoError(err)
			t.Cleanup(func() { checks.NoError(client.Close()) })
			output := filepath.Join(t.TempDir(), "meeting.srt")
			must.NoError(os.WriteFile(output, []byte("keep"), 0600))
			stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "srt", "-o", output)
			must.ErrorContains(err, "vtt")
			checks.Empty(stdout)
			saved, err := os.ReadFile(output)
			must.NoError(err)
			checks.Equal("keep", string(saved))
		})
	}
}

func TestMeetingSubtitleOldDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"5.0.0"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_meeting_format","message":"format must be json or markdown"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	for _, format := range []string{"srt", "vtt"} {
		t.Run(format, func(t *testing.T) {
			stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", "1", "--format", format)
			require.ErrorContains(t, err, "5.1.0")
			require.ErrorContains(t, err, "upgrade")
			assert.Empty(t, stdout)
		})
	}
}
