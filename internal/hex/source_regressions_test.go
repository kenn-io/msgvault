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
			must := require.New(t)

			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "span", "interrupted")
			manifest := Manifest{SchemaVersion: 1, ID: "span", Title: "Interrupted call", Status: "interrupted", CreatedAtMS: 1790000000000}
			data, err := json.Marshal(manifest)
			must.NoError(err)
			must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
			must.NoError(os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":60000,\"end_ms\":61000,\"text\":\"One second of speech\"}\n"), 0600))
			var sum *ImportSummary
			if remote {
				_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
				must.NoError(err)
				sum, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
			} else {
				sum, err = imp.Import(t.Context(), opts)
			}
			must.NoError(err)
			id := messageID(t, st, sum.SourceID, "span")
			ids := []int64{id}
			metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids})
			must.NoError(err)
			assert.InDelta(t, 1.0, metrics.Totals.TotalKnownSeconds, 0.001, "transcript-span excludes the unobserved first minute")
		})
	}
}

func TestHexCorruptFinalPreservesReadableArchive(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)

			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "corrupt-final", "complete")
			fixtureTranscript(t, dir)
			source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
			_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
			must.NoError(err)
			sync := func() (*ImportSummary, error) {
				if remote {
					return ScanRemote(t.Context(), opts, imp.ImportRemote)
				}
				return imp.Import(t.Context(), opts)
			}
			sum, err := sync()
			must.NoError(err)
			id := messageID(t, st, sum.SourceID, "corrupt-final")
			must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("malformed complete line\n"), 0600))
			for _, full := range []bool{false, true} {
				opts.Full = full
				if full {
					must.NoError(os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":1000,\"text\":\"Partial replacement\"}\nmalformed complete line\n"), 0600))
				}
				sum, err = sync()
				must.NoError(err)
				check.Positive(sum.Warnings)
				msg, err := st.GetMessage(id)
				must.NoError(err)
				check.Contains(msg.Body, "Synthetic transcript", "unreadable replacement must not erase readable archive")
				_, n, err := st.SearchMessagesContext(t.Context(), "Synthetic transcript", 0, 10)
				must.NoError(err)
				check.Equal(int64(1), n, "archived transcript must remain searchable")
				raw, err := st.GetMessageRaw(id)
				must.NoError(err)
				check.Contains(meetingcontent.Decode(RawFormat, raw, nil).Transcript.Text, "Synthetic transcript")
			}
			first := fixtureMeeting(t, opts.MeetingsDir, "first-partial", "complete")
			must.NoError(os.WriteFile(filepath.Join(first, "transcript.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":0,\"end_ms\":1000,\"text\":\"Readable salvage\"}\ninvalid\n"), 0600))
			sum, err = sync()
			must.NoError(err)
			raw, err := st.GetMessageRaw(messageID(t, st, sum.SourceID, "first-partial"))
			must.NoError(err)
			content := meetingcontent.Decode(RawFormat, raw, nil)
			check.Contains(content.Transcript.Text, "Readable salvage")
			check.Equal("hex_partial", content.Transcript.Reason)
		})
	}
}
