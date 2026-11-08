package hex

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
