package hex

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/meetingimport"
)

// Hex's writer atomically publishes newline-terminated JSON snapshots. All
// data here is synthetic; neither defaults nor user source folders are read.
func TestHexMeetingEditsRescansAndSourceExpiry(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st, imp, opts := importFixture(t)
	dir := fixtureMeeting(t, opts.MeetingsDir, "workflow", "recording")
	rolling := "{\"covered_ms\":{\"system\":4000,\"microphone\":4000}}\n{\"source\":\"system\",\"start_ms\":1000,\"end_ms\":4000,\"text\":\"Recovered synthetic discussion\"}\n"
	must.NoError(os.WriteFile(filepath.Join(dir, "transcript.rolling.ndjson"), []byte(rolling), 0600))
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsAdded)
	id := messageID(t, st, sum.SourceID, "workflow")
	raw, err := st.GetMessageRaw(id)
	must.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	check.Contains(content.Transcript.Text, "Recovered synthetic discussion")
	check.Equal("hex_rolling", content.Transcript.Reason)
	must.Len(content.SourceParticipants, 2)
	check.InDelta(60.333, *content.DurationSeconds, 0.0001)
	fixtureTranscript(t, dir)
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsUpdated)
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsAdded + sum.MeetingsUpdated)
	source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
	_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	sends := 0
	sender := func(ctx context.Context, r RemoteRequest) (RemoteResult, error) {
		sends++
		return imp.ImportRemote(ctx, r)
	}
	_, err = ScanRemote(t.Context(), opts, sender)
	must.NoError(err)
	_, err = ScanRemote(t.Context(), opts, sender)
	must.NoError(err)
	check.Equal(1, sends)
	must.NoError(os.RemoveAll(dir))
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsProcessed)
	raw, err = st.GetMessageRaw(id)
	must.NoError(err)
	check.Contains(meetingcontent.Decode(RawFormat, raw, nil).Transcript.Text, "Synthetic transcript")
}

func TestHexUnreadablePreservesArchivedTranscript(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)

			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "preserve", "complete")
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
			id := messageID(t, st, sum.SourceID, "preserve")
			// A failed read must not turn existing evidence into a deliberate deletion.
			// A directory guarantees a read failure even for a privileged test runner.
			must.NoError(os.Remove(filepath.Join(dir, "transcript.ndjson")))
			must.NoError(os.Mkdir(filepath.Join(dir, "transcript.ndjson"), 0700))
			sum, err = sync()
			must.NoError(err)
			check.Positive(sum.Warnings)
			raw, err := st.GetMessageRaw(id)
			must.NoError(err)
			content := meetingcontent.Decode(RawFormat, raw, nil)
			check.Contains(content.Transcript.Text, "Synthetic transcript", "last readable transcript must survive source I/O failure")
		})
	}
}

func TestHexPendingMarkdownRemainsUntouched(t *testing.T) {
	must := require.New(t)

	root := t.TempDir()
	dir := fixtureMeeting(t, root, "pending-publication", "transcribing")
	fixtureTranscript(t, dir)
	must.NoError(os.Rename(filepath.Join(dir, "transcript.md"), filepath.Join(dir, "transcript.md.tmp")))
	reader, err := Open(t.Context(), root)
	must.NoError(err)
	defer func() { _ = reader.Close() }()
	scan, err := reader.ListMeetings(t.Context())
	must.NoError(err)
	must.Len(scan.Meetings, 1)
	// Hex recover_final_publication renames this exact pending Markdown and
	// recovered_status then returns Complete. Reader should infer it read-only.
	assert.Equal(t, "complete", scan.Meetings[0].Manifest.Status)
	_, err = os.Stat(filepath.Join(dir, "transcript.md.tmp"))
	must.NoError(err, "source must remain unchanged")
}

func TestHexObservedTranscriptSpanAndProviderDuration(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	m := Meeting{Manifest: Manifest{SchemaVersion: 1, ID: "interrupted", Status: "interrupted", CreatedAtMS: 1790000000000}, TranscriptState: "rolling", Entries: []Entry{{Source: "system", StartMS: 1000, EndMS: 2000, Text: "Synthetic opening"}, {Source: "microphone", StartMS: 11000, EndMS: 12000, Text: "Synthetic ending"}}}
	snapshot, err := m.ArchiveSnapshot(1, "recorder", "you@example.com")
	must.NoError(err)
	content := meetingcontent.Decode(RawFormat, snapshot.Raw, nil)
	must.NotNil(content.DurationSeconds, "transcript offsets provide a duration estimate even after a crash")
	check.InDelta(11, *content.DurationSeconds, 0.0001)
	check.Equal(meetingcontent.DurationTranscriptSpan, content.DurationBasis)
	check.Equal(time.UnixMilli(m.Manifest.CreatedAtMS+12000).UTC(), *content.Transcript.Segments[1].EndedAt)
	for _, milliseconds := range []int64{0, 45000} {
		m.Manifest.DurationMS = &milliseconds
		snapshot, err = m.ArchiveSnapshot(1, "recorder", "you@example.com")
		must.NoError(err)
		content = meetingcontent.Decode(RawFormat, snapshot.Raw, nil)
		must.NotNil(content.DurationSeconds)
		check.InDelta(float64(milliseconds)/1000, *content.DurationSeconds, 0.0001)
		check.Equal(meetingcontent.DurationProvider, content.DurationBasis)
	}
}

func TestHexCalendarAttendeesAreNotTruncated(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)

			st, imp, opts := importFixture(t)
			dir := fixtureMeeting(t, opts.MeetingsDir, "all-hands", "complete")
			fixtureTranscript(t, dir)
			attendees := make([]string, 201)
			for i := range attendees {
				attendees[i] = fmt.Sprintf("Synthetic Guest %d", i)
			}
			manifest := Manifest{SchemaVersion: 1, ID: "all-hands", Title: "Synthetic all-hands", Status: "complete", CreatedAtMS: 1790000000000, Calendar: &Calendar{Title: "All hands", Attendees: attendees}}
			data, err := json.Marshal(manifest)
			must.NoError(err)
			must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
			source := meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}
			_, err = imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: source})
			must.NoError(err)
			var summary *ImportSummary
			if remote {
				summary, err = ScanRemote(t.Context(), opts, imp.ImportRemote)
			} else {
				summary, err = imp.Import(t.Context(), opts)
			}
			must.NoError(err)
			check.Equal(int64(1), summary.MeetingsAdded, "an upstream-valid large calendar must not suppress its transcript")
			check.Zero(summary.Warnings)
			raw, err := st.GetMessageRaw(messageID(t, st, summary.SourceID, "all-hands"))
			must.NoError(err)
			check.Len(meetingcontent.Decode(RawFormat, raw, nil).SourceParticipants, 201)
		})
	}
}

func TestHexFutureReceiptVersionIsPreserved(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	path := filepath.Join(t.TempDir(), "receipts.json")
	original := []byte(`{"version":3,"meetings":["future-shape"],"recordings":{},"future_state":{"retain":true}}`)
	must.NoError(os.WriteFile(path, original, 0600))
	receipts, err := meetingimport.LoadRecorderUploads[string](path)
	must.Error(err, "older reader must refuse a newer receipt format")
	if receipts != nil {
		must.NoError(receipts.Save())
	}
	after, err := os.ReadFile(path)
	must.NoError(err)
	check.Equal(string(original), string(after), "unknown future state must remain intact")
}
