package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/telemetry"
)

func TestCalDAVSyncForwardsToDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assertions.Equal([]string{"sync-caldav", "--full", "personal"}, req.Args)
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newSyncCalDAVCmd())
	root.SetContext(ctx)
	root.SetArgs([]string{"sync-caldav", "personal", "--full"})
	requirements.NoError(root.Execute())
	assertions.Equal(int32(1), requests.Load())
}

func TestCalDAVSyncRecordsFailure(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := lifecycleTestConfig(t.TempDir())
	passwordFile := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(passwordFile, []byte("synthetic-password"), 0600))
	// Discovery rejects the untrusted loopback destination before dialing.
	cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal", URL: "https://127.0.0.1/dav", Username: "owner@example.org", PasswordFile: passwordFile}}
	state := testInvocationWithConfig(cfg)
	var events []map[string]any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal(t, "sync_failed", event)
		events = append(events, props)
		return nil
	}}
	cmd := newSyncCalDAVCmd()
	cmd.SetContext(withInvocation(t.Context(), state))
	cmd.SetArgs([]string{"personal"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	require.ErrorContains(t, cmd.Execute(), "discover CalDAV calendars")
	require.Len(t, events, 1)
	assert.Equal(t, "caldav", events[0]["sync_source"])
}
