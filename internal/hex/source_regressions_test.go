package hex

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

func TestHexObservedTranscriptSpanMetrics(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "span", "interrupted")
			manifest := Manifest{SchemaVersion: 1, ID: "span", Title: "Interrupted call", Status: "interrupted", CreatedAtMS: 1790000000000}
			data, err := json.Marshal(manifest)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":60000,\"end_ms\":61000,\"text\":\"One second of speech\"}\n"), 0600))
			var sum *ImportSummary
			if remote {
				_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
				require.NoError(t, err)
				sum, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
			} else {
				sum, err = imp.Import(t.Context(), opts)
			}
			require.NoError(t, err)
			id := messageID(t, st, sum.SourceID, "span")
			ids := []int64{id}
			metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids})
			require.NoError(t, err)
			assert.InDelta(t, 1.0, metrics.Totals.TotalKnownSeconds, 0.001, "transcript-span excludes the unobserved first minute")
		})
	}
}

func TestHexCorruptFinalPreservesReadableArchive(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "corrupt-final", "complete")
			fixtureTranscript(t, dir)
			source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
			_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
			require.NoError(t, err)
			sync := func() (*ImportSummary, error) {
				if remote {
					return ScanRemote(t.Context(), opts, imp.ImportRemote)
				}
				return imp.Import(t.Context(), opts)
			}
			sum, err := sync()
			require.NoError(t, err)
			id := messageID(t, st, sum.SourceID, "corrupt-final")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("malformed complete line\n"), 0600))
			for _, full := range []bool{false, true} {
				opts.Full = full
				if full {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":1000,\"text\":\"Partial replacement\"}\nmalformed complete line\n"), 0600))
				}
				sum, err = sync()
				require.NoError(t, err)
				assert.Positive(t, sum.Warnings)
				msg, err := st.GetMessage(id)
				require.NoError(t, err)
				assert.Contains(t, msg.Body, "Synthetic transcript", "unreadable replacement must not erase readable archive")
				_, n, err := st.SearchMessagesContext(t.Context(), "Synthetic transcript", 0, 10)
				require.NoError(t, err)
				assert.Equal(t, int64(1), n, "archived transcript must remain searchable")
				raw, err := st.GetMessageRaw(id)
				require.NoError(t, err)
				assert.Contains(t, meetingcontent.Decode(RawFormat, raw, nil).Transcript.Text, "Synthetic transcript")
			}
			first := fixtureMeeting(t, opts.MeetingsDir, "first-partial", "complete")
			require.NoError(t, os.WriteFile(filepath.Join(first, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":1000,\"text\":\"Readable salvage\"}\ninvalid\n"), 0600))
			sum, err = sync()
			require.NoError(t, err)
			raw, err := st.GetMessageRaw(messageID(t, st, sum.SourceID, "first-partial"))
			require.NoError(t, err)
			content := meetingcontent.Decode(RawFormat, raw, nil)
			assert.Contains(t, content.Transcript.Text, "Readable salvage")
			assert.Equal(t, "hex_partial", content.Transcript.Reason)
		})
	}
}
