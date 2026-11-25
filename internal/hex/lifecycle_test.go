package hex

import (
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
	summary, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), summary.MeetingsAdded, "an upstream-valid large calendar must not suppress its transcript")
	check.Zero(summary.Warnings)
	raw, err := st.GetMessageRaw(messageID(t, st, summary.SourceID, "all-hands"))
	must.NoError(err)
	check.Len(meetingcontent.Decode(RawFormat, raw, nil).SourceParticipants, 201)
}
