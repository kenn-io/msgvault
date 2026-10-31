package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS == "windows" {
		t.Skip("the password file needs an owner-only ACL")
	}
	assertions, requirements := assert.New(t), require.New(t)
	markDaemonCLISubprocessForTest(t)
	cfg := lifecycleTestConfig(t.TempDir())
	passwordFile := filepath.Join(t.TempDir(), "password")
	requirements.NoError(os.WriteFile(passwordFile, []byte("synthetic-password"), 0600))
	// Discovery rejects the untrusted loopback destination before dialing.
	cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal", URL: "https://127.0.0.1/dav", Username: "owner@example.org", PasswordFile: passwordFile}}
	state := testInvocationWithConfig(cfg)
	var events []map[string]any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assertions.Equal("sync_failed", event)
		events = append(events, props)
		return nil
	}}
	cmd := newSyncCalDAVCmd()
	cmd.SetContext(withInvocation(t.Context(), state))
	cmd.SetArgs([]string{"personal"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	requirements.ErrorContains(cmd.Execute(), "discover CalDAV calendars")
	requirements.Len(events, 1)
	assertions.Equal("caldav", events[0]["sync_source"])
}
