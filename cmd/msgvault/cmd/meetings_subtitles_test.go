package cmd

import (
	"context"
	"encoding/json/v2"
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
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMeetingSubtitlesArchiveHTTPCLI(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("granola", "subtitle@example.test")
	require.NoError(t, err)
	raw := []byte(`{"summary_markdown":"Keep summary","transcript":[{"speaker":{"name":"A<&>"},"text":"Hello --> 世界","start_time":"2026-01-01T10:00:02Z","end_time":"2026-01-01T10:00:04.250Z"},{"speaker":{"name":"B"},"text":"Next","start_time":"2026-01-01T10:00:03Z","end_time":"2026-01-01T10:00:05Z"}]}`)
	metadata := []byte(`{"platform":"granola","retained":"synthetic"}`)
	snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "subtitle@example.test", SourceMessageID: "subtitle", SourceConversationID: "subtitle", Title: "Subtitle fixture", StartedAt: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), Raw: raw, RawFormat: "granola_json", Metadata: metadata, Organizer: &meetingarchive.Person{Name: "Owner", Email: "subtitle@example.test"}}
	archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	require.NoError(t, err)
	before, err := st.GetMessageContext(t.Context(), archived.MessageID)
	require.NoError(t, err)
	server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	id := meetingTestInt64(archived.MessageID)
	output := filepath.Join(t.TempDir(), "meeting.vtt")
	for _, format := range []string{"srt", "vtt"} {
		stdout, stderr, err := executeMeetingsCommand(t, client, "subtitles", "--id", id, "--format", format)
		require.NoError(t, err)
		assert.Empty(t, stderr)
		want := "1\n00:00:00,000 --> 00:00:02,250\nA&lt;&amp;&gt;: Hello --&gt; 世界\n\n2\n00:00:01,000 --> 00:00:03,000\nB: Next\n\n"
		if format == "vtt" {
			want = "WEBVTT\n\n" + strings.ReplaceAll(want, ",", ".")
		}
		assert.Equal(t, want, stdout)
		stdout, stderr, err = executeMeetingsCommand(t, client, "subtitles", "--id", id, "--format", format, "-o", output)
		require.NoError(t, err)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, output)
		saved, err := os.ReadFile(output)
		require.NoError(t, err)
		assert.Equal(t, want, string(saved))
		t.Logf("synthetic CLI %s output:\n%s", format, want)
	}
	after, err := st.GetMessageContext(t.Context(), archived.MessageID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	storedRaw, err := st.GetMessageRawContext(t.Context(), archived.MessageID)
	require.NoError(t, err)
	assert.Equal(t, raw, storedRaw)
	storedMetadata, err := st.GetMessageMetadataContext(t.Context(), archived.MessageID)
	require.NoError(t, err)
	require.True(t, storedMetadata.Valid)
	assert.Equal(t, string(metadata), storedMetadata.String)
	// Real archive responses refuse formatting errors, missing timing and size
	// truncation without replacing an existing destination or emitting stdout.
	for name, badRaw := range map[string][]byte{
		"layout": []byte(strings.Replace(string(raw), "Hello --> 世界", `first\n\nsecond`, 1)),
		"timing": []byte(strings.Replace(string(raw), `"end_time":"2026-01-01T10:00:04.250Z"`, `"end_time":null`, 1)),
		"size":   []byte(strings.Replace(string(raw), "Hello --> 世界", strings.Repeat("large ", 200000), 1)),
	} {
		t.Run(name, func(t *testing.T) {
			snapshot.Raw = badRaw
			_, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(output, []byte("keep"), 0600))
			stdout, _, err := executeMeetingsCommand(t, client, "subtitles", "--id", id, "-o", output)
			require.ErrorContains(t, err, map[string]string{"layout": "format", "timing": "timing", "size": "size limit"}[name])
			assert.Empty(t, stdout)
			saved, err := os.ReadFile(output)
			require.NoError(t, err)
			assert.Equal(t, "keep", string(saved))
		})
	}
}

func TestMeetingSubtitlesFlags(t *testing.T) {
	for _, args := range [][]string{{}, {"--id", "0"}, {"--id", "9007199254740992"}, {"--id", "1,2"}, {"--id", "1", "--id", "2"}, {"--id", "1", "--format", "json"}} {
		command := newMeetingsCommand(meetingCommandDeps{})
		command.SetArgs(append([]string{"subtitles"}, args...))
		require.Error(t, command.ExecuteContext(t.Context()))
	}
}

// Override only the packet boundary; transport/version behavior is covered by
// existing meeting command tests and the real HTTP archive test above.
type subtitlePacketClient struct {
	meetingCommandClient
	result *meetingcontent.PacketResult
}

func (c subtitlePacketClient) GetMeetingContext(context.Context, generated.GetMeetingContextBody) (*meetingcontent.PacketResult, error) {
	return c.result, nil
}

func TestMeetingSubtitlesRejectIncompletePackets(t *testing.T) {
	client, _ := newMeetingCommandClient(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	transcript := meetingcontent.Transcript{State: meetingcontent.StateAvailable, Segments: []meetingcontent.Segment{{Text: "Complete cue", StartedAt: &start, EndedAt: new(start.Add(time.Second))}}}
	for _, kind := range []string{"schema", "outer schema", "truncated", "outer truncated", "omitted", "outer omitted", "truncations", "wrong id", "multiple", "requested id", "json", "empty"} {
		t.Run(kind, func(t *testing.T) {
			packet := meetingcontent.Packet{SchemaVersion: 1, RequestedMessageIDs: []int64{42}, Meetings: []meetingcontent.Entry{{Meeting: meetingcontent.MeetingRef{MessageID: 42}, Content: meetingcontent.Content{Transcript: transcript}}}}
			result := &meetingcontent.PacketResult{SchemaVersion: 1, Format: meetingcontent.FormatJSON}
			switch kind {
			case "schema":
				packet.SchemaVersion = 2
			case "outer schema":
				result.SchemaVersion = 2
			case "truncated":
				packet.Truncated = true
			case "outer truncated":
				result.Truncated = true
			case "omitted":
				packet.OmittedMessageIDs = []int64{42}
			case "outer omitted":
				result.OmittedMessageIDs = []int64{42}
			case "truncations":
				packet.Truncations = []meetingcontent.Truncation{{MessageID: 42}}
			case "wrong id":
				packet.Meetings[0].Meeting.MessageID = 43
			case "multiple":
				packet.Meetings = append(packet.Meetings, packet.Meetings[0])
			case "requested id":
				packet.RequestedMessageIDs = []int64{43}
			}
			encoded, err := json.Marshal(packet)
			require.NoError(t, err)
			result.Content = string(encoded)
			if kind == "json" {
				result.Content = "{"
			}
			if kind == "empty" {
				result = nil
			}
			output := filepath.Join(t.TempDir(), "keep.srt")
			require.NoError(t, os.WriteFile(output, []byte("keep"), 0600))
			stdout, _, err := executeMeetingsCommand(t, subtitlePacketClient{client, result}, "subtitles", "--id", "42", "-o", output)
			require.Error(t, err)
			assert.Empty(t, stdout)
			saved, err := os.ReadFile(output)
			require.NoError(t, err)
			assert.Equal(t, "keep", string(saved))
		})
	}
}
