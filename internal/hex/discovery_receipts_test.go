package hex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMissingRootAndHiddenFolders(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	_, imp, opts := importFixture(t)
	opts.MeetingsDir = filepath.Join(t.TempDir(), "not-created", "meetings")
	reader, err := Open(t.Context(), opts.MeetingsDir)
	must.NoError(err)
	scan, err := reader.ListMeetings(t.Context())
	must.NoError(err)
	check.Empty(scan.Meetings)
	check.Zero(scan.Warnings)
	must.NoError(reader.Close())
	must.NoError(ForgetRemoteUploads(t.Context(), opts))
	sum, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Zero(sum.MeetingsProcessed)
	sum, err = ScanRemote(t.Context(), opts, nil)
	must.NoError(err)
	check.Zero(sum.MeetingsProcessed)
	_, err = os.Stat(opts.MeetingsDir)
	must.ErrorIs(err, os.ErrNotExist, "setup and scans never create Hex storage")
	must.NoError(os.MkdirAll(filepath.Join(opts.MeetingsDir, ".hidden"), 0700))
	dir := fixtureMeeting(t, opts.MeetingsDir, "visible", "complete")
	fixtureTranscript(t, dir)
	sum, err = imp.Import(t.Context(), opts)
	must.NoError(err)
	check.Equal(int64(1), sum.MeetingsAdded, "a later scan sees the root Hex creates")
	check.Zero(sum.Warnings, "hidden folders are ignored without reading their contents")
}
