package hex

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/hex/evidence"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

func TestHexObservedTranscriptSpanMetrics(t *testing.T) {
	must := require.New(t)

	st, imp, opts := importFixture(t)
	dir := filepath.Join(opts.MeetingsDir, "span")
	must.NoError(os.Mkdir(dir, 0700))
	manifest := Manifest{SchemaVersion: 1, ID: "span", Title: "Interrupted call", Status: "interrupted", CreatedAtMS: 1790000000000}
	data, err := json.Marshal(manifest)
	must.NoError(err)
	must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
	must.NoError(os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte("{\"source\":\"system\",\"start_ms\":60000,\"end_ms\":61000,\"text\":\"One second of speech\"}\n"), 0600))
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
	must.NoError(err)
	sum, err := ScanRemote(t.Context(), opts, imp.ImportRemote)
	must.NoError(err)
	id := messageID(t, st, sum.SourceID, "span")
	ids := []int64{id}
	metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids})
	must.NoError(err)
	assert.InDelta(t, 1.0, metrics.Totals.TotalKnownSeconds, 0.001, "transcript-span excludes the unobserved first minute")
}

// Hex #1462 writes imported audio as a system track with imported=true.
// Its public schema labels those turns Speaker, not the recorded-call Them.
func TestHexImportedAudioSpeaker(t *testing.T) {
	st, imp, opts := importFixture(t)
	_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
	require.NoError(t, err)
	for _, tc := range []struct{ id, flag, source, speaker string }{
		{"imported", `,"imported":true`, "system", "Speaker"},
		{"recorded-system", "", "system", "Them"},
		{"recorded-microphone", "", "microphone", "You"},
	} {
		dir := filepath.Join(opts.MeetingsDir, tc.id)
		require.NoError(t, os.Mkdir(dir, 0700))
		manifest := `{"schema_version":1,"id":"` + tc.id + `","title":"Synthetic audio","status":"complete","created_at_ms":1790000000000` + tc.flag + `}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600))
		transcript := `{"source":"` + tc.source + `","start_ms":0,"end_ms":1000,"text":"Synthetic speech"}` + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.ndjson"), []byte(transcript), 0600))
		sum, err := ScanRemote(t.Context(), opts, func(ctx context.Context, request RemoteRequest) (RemoteResult, error) {
			wire, marshalErr := json.Marshal(request)
			require.NoError(t, marshalErr)
			decoded, decodeErr := DecodeRemoteRequest(bytes.NewReader(wire), MaxRemoteRequestBytes)
			require.NoError(t, decodeErr)
			return imp.ImportRemote(ctx, decoded)
		})
		require.NoError(t, err)
		id := messageID(t, st, sum.SourceID, tc.id)
		body, err := st.GetMessageBodyText(t.Context(), id)
		require.NoError(t, err)
		assert.Contains(t, body, tc.speaker+": Synthetic speech")
		raw, err := st.GetMessageRaw(id)
		require.NoError(t, err)
		envelope, err := evidence.Decode(raw)
		require.NoError(t, err)
		assert.Equal(t, tc.id == "imported", envelope.Meeting.Manifest.Imported)
		assert.Equal(t, map[bool]int{false: 1, true: 2}[tc.id == "imported"], envelope.Version)
		content := meetingcontent.Decode(RawFormat, raw, nil)
		require.Len(t, content.Transcript.Segments, 1)
		assert.Equal(t, tc.speaker, content.Transcript.Segments[0].Speaker)
		assert.Contains(t, content.Transcript.Text, tc.speaker+": Synthetic speech")
	}
}

// These bytes and receipt digest were produced by the pre-imported-field writer
// at 2fb9a67. Ordinary meetings must stay compatible with its strict decoder.
func TestHexRecordedWireCompatibility(t *testing.T) {
	const previousWire = `{"action":"upsert","source":{"identifier":"recorder","account_email":"you@example.com"},"meeting":{"manifest":{"schema_version":1,"id":"recorded","title":"Synthetic call","title_source":"user","status":"complete","created_at_ms":1790000000000},"entries":[{"source":"system","start_ms":0,"end_ms":1000,"text":"Synthetic speech"}],"transcript_state":"final","transcript_partial":false,"title_source_inferred":false},"full":false,"build_cache":false,"no_build_cache":false}`
	request, err := DecodeRemoteRequest(bytes.NewBufferString(previousWire), MaxRemoteRequestBytes)
	require.NoError(t, err)
	wire, err := json.Marshal(request)
	require.NoError(t, err)
	//nolint:testifylint // Byte-for-byte wire compatibility is required; JSONEq would hide changes.
	assert.Equal(t, previousWire, string(wire))
	digest, err := meetingimport.MeetingDigest(*request.Meeting)
	require.NoError(t, err)
	assert.Equal(t, "de0f18c0f8cc2b8c847e3ce0b085925bb0537f5722331ff1be8e35dde3f4d2e7", digest)
}
