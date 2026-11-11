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
	"go.kenn.io/docbank/document/mediatranscript"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMeetingSubtitleUnknownOriginPreservesFile(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("granola", "owner@example.test")
	must.NoError(err)
	snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "one", SourceConversationID: "one", Title: "Meeting", StartedAt: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), Raw: []byte(`{"transcript":[{"speaker":{"name":"A"},"text":"Hello","start_time":"2026-01-01T10:00:02Z","end_time":"2026-01-01T10:00:04Z"}]}`), RawFormat: "granola_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
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
	must.ErrorContains(err, "recording start")
	checks.Empty(stdout)
	saved, err := os.ReadFile(output)
	must.NoError(err)
	checks.Equal("keep", string(saved))
}

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
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--id", meetingTestInt64(archived.MessageID), "--format", "vtt")
	must.NoError(err)
	checks.Equal(want, stdout)
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--id", "999999", "--format", "vtt")
	must.ErrorContains(err, "exactly one distinct meeting ID")
	checks.Empty(stdout)
	snapshot.Raw = []byte(strings.Replace(string(raw), `"Hello"`, `"`+strings.Repeat("x", 5000)+`"`, 1))
	_, err = meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	must.NoError(err)
	stdout, _, err = executeMeetingsCommand(t, client, "context", "--id", meetingTestInt64(archived.MessageID), "--format", "vtt", "--max-bytes", "4096")
	must.ErrorContains(err, "max_bytes")
	checks.Equal(1, strings.Count(err.Error(), "1048576"))
	checks.Empty(stdout)
}

func TestMeetingSubtitleRelativeUnknownOriginPreservesFile(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"omi", `{"structured":{},"transcript_segments":[{"text":"Hello","start":0,"end":2}]}`},
		{"pocket", `{"schema_version":1,"started_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"Hello","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"Hello","start":0,"end":2}]}}}`},
		{"twenty", `{"schema_version":1,"calendar_event":{"startsAt":"2026-01-01T00:00:00Z"},"recording":{"transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":0},"end_timestamp":{"relative":2}}]}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.name, "owner@example.test")
			must.NoError(err)
			snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "one", SourceConversationID: "one", Title: "Meeting", StartedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Raw: []byte(tc.raw), RawFormat: tc.name + "_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
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
			must.ErrorContains(err, "recording start")
			checks.Empty(stdout)
			saved, err := os.ReadFile(output)
			must.NoError(err)
			checks.Equal("keep", string(saved))
			format := generated.MeetingContextRequestFormat("srt")
			_, err = client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID}), Format: &format})
			var apiErr *daemonclient.APIError
			must.ErrorAs(err, &apiErr)
			checks.Equal("invalid_meeting_subtitles", apiErr.Code)
			checks.Equal(400, apiErr.Status)
		})
	}
}

func TestMeetingSubtitleWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, raw, format, want string
	}{
		{"omi", `{"started_at":"2026-01-01T00:00:00Z","structured":{},"transcript_segments":[{"text":"Hello\n  ordinary speech","start":1,"end":2}]}`, "srt", "1\n00:00:01,000 --> 00:00:02,000\nHello\nordinary speech\n\n"},
		{"pocket-vtt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Hello  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Hello  ","start":0,"end":2}]}}}`, "vtt", "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\n  Hello  \n\n"},
		{"pocket-srt", `{"schema_version":1,"recorded_at":"2026-01-01T00:00:00Z","content":{"summary":{"state":"empty"},"notes":{"state":"unsupported"},"transcript":{"state":"available","segments":[{"text":"  Hello  ","offset_seconds":0}]},"action_coverage":"unsupported"},"recording":{"transcript":{"segments":[{"text":"  Hello  ","start":0,"end":2}]}}}`, "srt", "1\n00:00:00,000 --> 00:00:02,000\nHello\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			st := testutil.NewTestStore(t)
			sourceName := "pocket"
			if tc.name == "omi" {
				sourceName = "omi"
			}
			source, err := st.GetOrCreateSource(sourceName, "owner@example.test")
			must.NoError(err)
			format := "pocket_json"
			if tc.name == "omi" {
				format = "omi_json"
			}
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
			if tc.name == "omi" {
				parsed, err := mediatranscript.ParseSubRip(saved, "msgvault", "und")
				must.NoError(err)
				must.Len(parsed.Segments, 1)
				checks.Equal(int64(1000), parsed.Segments[0].StartMS)
				checks.Equal(int64(2000), parsed.Segments[0].EndMS)
				checks.Equal("Hello\nordinary speech", parsed.Segments[0].Text)
			}
			checks.Empty(stdout)
			requestFormat := generated.MeetingContextRequestFormat(tc.format)
			result, err := client.GetMeetingContext(t.Context(), generated.GetMeetingContextBody{MessageIds: new([]int64{archived.MessageID}), Format: &requestFormat})
			must.NoError(err)
			checks.Equal(tc.want, result.Content)
		})
	}
}
