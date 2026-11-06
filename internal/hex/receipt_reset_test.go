package hex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
)

func TestRemoteReregistrationResetsReceiptsWhileRootMissing(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	_, imp, opts := importFixture(t)
	_, err := imp.ImportRemote(t.Context(), RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: opts.Identifier, AccountEmail: opts.AccountEmail}})
	must.NoError(err)
	dir := fixtureMeeting(t, opts.MeetingsDir, "reset-after-missing-root", "complete")
	fixtureTranscript(t, dir)
	sends := 0
	send := func(ctx context.Context, req RemoteRequest) (RemoteResult, error) {
		sends++
		return imp.ImportRemote(ctx, req)
	}
	_, err = ScanRemote(t.Context(), opts, send)
	must.NoError(err)
	must.Equal(1, sends)
	offline := filepath.Join(t.TempDir(), "offline-meetings")
	must.NoError(os.Rename(opts.MeetingsDir, offline))
	must.NoError(ForgetRemoteUploads(t.Context(), opts), "registration accepts a root Hex has not created or that is offline")
	must.NoError(os.Rename(offline, opts.MeetingsDir))
	_, err = ScanRemote(t.Context(), opts, send)
	must.NoError(err)
	check.Equal(2, sends, "re-registration must reoffer the acknowledged meeting when its folder returns")
}
