package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftLifecycleEndToEnd(t *testing.T) {
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	configureRemoteDaemonForTest(t, server.URL)
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "msgvault"}
		root.AddCommand(newDraftReplyCommand(), newDraftGetCommand(), newDraftEditCommand(), newDraftDeleteCommand())
		silenceUsageInRunE(root)
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(t.Context())
		requirements.Empty(stderr.String())
		return stdout.String(), err
	}
	createdJSON, err := run("draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", "initial body", "--json")
	requirements.NoError(err)
	var created struct {
		DraftID  string `json:"draft_id"`
		Revision int64  `json:"revision"`
	}
	requirements.NoError(json.Unmarshal([]byte(createdJSON), &created))
	requirements.NotEmpty(created.DraftID)
	requirements.Equal(int64(1), created.Revision)

	getEvent, err := run(api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	requirements.Contains(getEvent, "initial body")
	humanGet, err := run(api.CLIRunDraftGetCommand, created.DraftID)
	requirements.NoError(err)
	requirements.Contains(humanGet, "content:\ninitial body")
	requirements.Contains(humanGet, "receipt (revision 1): Drafts")

	editEvent, err := run(api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "edited body", "--json")
	requirements.NoError(err)
	requirements.Contains(editEvent, "\"revision\":2")

	deleteEvent, err := run(api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.Contains(deleteEvent, "\"lifecycle\":\"discarded\"")
	_, err = run(api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
}
