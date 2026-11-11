package cmd

import (
	"log/slog"
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
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--id", meetingTestInt64(archived.MessageID), "--format", "vtt")
	require.NoError(t, err)
	assert.Equal(t, want, stdout)
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--id", "999999", "--format", "vtt")
	require.ErrorContains(t, err, "exactly one distinct meeting ID")
	assert.Empty(t, stdout)
	snapshot.Raw = []byte(strings.Replace(string(raw), `"Hello"`, `"`+strings.Repeat("x", 5000)+`"`, 1))
	_, err = meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	require.NoError(t, err)
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "vtt", "--max-bytes", "4096")
	require.ErrorContains(t, err, "--max-bytes")
	assert.Empty(t, stdout)
}

func TestMeetingSubtitleRelativeUnknownOriginPreservesFile(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"omi", `{"structured":{},"transcript_segments":[{"text":"Hello","start":0,"end":2}]}`},
		{"pocket", `{"schema_version":1,"started_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Hello","start":0,"end":2}]}}}`},
		{"twenty", `{"schema_version":1,"calendar_event":{"startsAt":"2026-01-01T00:00:00Z"},"recording":{"transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0},"end_timestamp":{"relative":2}}]}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.name, "owner@example.test")
			require.NoError(t, err)
			snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "one", SourceConversationID: "one", Title: "Meeting", StartedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Raw: []byte(tc.raw), RawFormat: tc.name + "_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
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
			format := generated.MeetingContextRequestFormat("srt")
			_, err = client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID}), Format: &format})
			var apiErr *daemonclient.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, "invalid_meeting_subtitles", apiErr.Code)
			assert.Equal(t, 400, apiErr.Status)
		})
	}
}

func TestMeetingSubtitleTwilioAndPocketWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, raw, format, want string
		bad                     bool
	}{
		{"twilio", `{"recordings":[{"sid":"RE1","start_time":"Sat, 03 Oct 2026 10:00:00 +0000"}],"transcript":"A: Hello","transcript_segments":[{"speaker":"A","text":"Hello","scope":"RE1","offset_seconds":1.25}],"evidence":{"transcripts":[{"kind":"classic","id":"GT1","source_id":"RE1","usable":true,"segments":[{"speaker":"A","text":"Hello","scope":"RE1","offset_seconds":1.25}],"raw":{"metadata":{"channel":{"participants":[{"channel_participant":1,"role":"A"}]}},"sentences":[{"sentence_index":0,"media_channel":1,"start_time":"1.25","end_time":"2.5","transcript":"Hello"}]}}]}}`, "srt", "1\n00:00:01,250 --> 00:00:02,500\nA: Hello\n\n", false},
		{"pocket-vtt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Hello  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Hello  ","start":0,"end":2}]}}}`, "vtt", "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Hello  \n\n", false},
		{"pocket-blank-srt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Open C:\\tmp {x}\n\nworld","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Open C:\\tmp {x}\n\nworld","start":0,"end":2}]}}}`, "srt", "1\n00:00:00,000 --> 00:00:02,000\nOpen C:\\tmp {x}\nworld\n\n", false},
		{"pocket-blank-vtt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Open C:\\tmp {x}\n\nworld","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Open C:\\tmp {x}\n\nworld","start":0,"end":2}]}}}`, "vtt", "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\nOpen C:\\tmp {x}\nworld\n\n", false},
		{"pocket-srt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Hello  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Hello  ","start":0,"end":2}]}}}`, "srt", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("subtitle", "owner@example.test")
			require.NoError(t, err)
			format := "pocket_json"
			if tc.name == "twilio" {
				format = "twilio_call_json"
			}
			snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: tc.name, SourceConversationID: tc.name, Title: "Meeting", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Raw: []byte(tc.raw), RawFormat: format, Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
			archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
			require.NoError(t, err)
			server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			output := filepath.Join(t.TempDir(), "meeting")
			require.NoError(t, os.WriteFile(output, []byte("keep"), 0600))
			stdout, _, err := executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", tc.format, "-o", output)
			if tc.bad {
				require.ErrorContains(t, err, "invalid meeting subtitles")
				saved, readErr := os.ReadFile(output)
				require.NoError(t, readErr)
				assert.Equal(t, "keep", string(saved))
				assert.Empty(t, stdout)
				return
			}
			require.NoError(t, err)
			saved, err := os.ReadFile(output)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(saved))
			assert.Empty(t, stdout)
			requestFormat := generated.MeetingContextRequestFormat(tc.format)
			result, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID}), Format: &requestFormat})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.Content)
		})
	}
}
