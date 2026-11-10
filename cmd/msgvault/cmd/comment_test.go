package cmd

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// commentCommandFixture syncs comment 100_201 from the Facebook fixture and
// returns a runner for comment commands as the daemon runs them.
func commentCommandFixture(t *testing.T) (*config.Config, *store.Store, int64, func(*cobra.Command, ...string) (string, error)) {
	t.Helper()
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := registerFacebookCommentsProfile(t.Context(), st, cfg.FacebookComments[0])
	require.NoError(t, err)
	// The fixture denies ad posts; the comment on the organic post is archived.
	_, _ = importFacebookCommentsProfile(t.Context(), st, cfg.FacebookComments[0], meta.ImportOptions{})
	ids, err := st.MessageExistsBatch(source.ID, []string{"100_201"})
	require.NoError(t, err)
	require.NotZero(t, ids["100_201"])
	run := func(cmd *cobra.Command, args ...string) (string, error) {
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
		err := cmd.Execute()
		return output.String(), err
	}
	return cfg, st, ids["100_201"], run
}

func TestCommentModerateOptInAndConfirmation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg, st, messageID, runCommand := commentCommandFixture(t)
	id := strconv.FormatInt(messageID, 10)
	run := func(args ...string) (string, error) { return runCommand(newCommentModerateCmd(), args...) }
	hidden := func() bool {
		target, err := meta.ResolveCommentTarget(t.Context(), st, messageID)
		require.NoError(err)
		return target.Hidden
	}

	_, err := run(id, "--hide")
	var refused *sourceCommandError
	require.ErrorAs(err, &refused)
	assert.Equal(5, refused.ExitCode())
	assert.Equal(`add "hide" to actions in that profile`, refused.Hint())
	assert.False(hidden(), "a profile without actions never changes the comment")

	cfg.FacebookComments[0].Actions = []string{"hide"}
	_, err = run(id, "--unhide=false")
	require.ErrorContains(err, "choose one of")
	assert.False(hidden(), "a false action flag does not fall back to hide")
	output, err := run(id, "--hide", "--dry-run")
	require.NoError(err)
	assert.Equal("Would hide Facebook comment 100_201 by Example Customer (message "+id+"): Example comment\n", output)
	assert.False(hidden(), "a dry run does not call Meta")

	output, err = run(id, "--unhide")
	require.NoError(err, "the hide opt-in also allows unhide")
	assert.Equal("Unhid Facebook comment 100_201 by Example Customer (message "+id+")\n", output)
	output, err = run(id, "--hide")
	require.NoError(err)
	assert.Equal("Hid Facebook comment 100_201 by Example Customer (message "+id+")\n", output)
	assert.True(hidden())

	_, err = run(id, "--delete", "--yes")
	require.ErrorAs(err, &refused, "hide does not opt in to delete")
	cfg.FacebookComments[0].Actions = []string{"hide", "delete"}
	_, err = run(id, "--delete")
	require.ErrorContains(err, "--yes")
	assert.True(hidden(), "delete without --yes leaves the comment alone")
	_, err = run(id, "--delete", "--yes")
	require.NoError(err)
	_, err = meta.ResolveCommentTarget(t.Context(), st, messageID)
	require.ErrorIs(err, store.ErrMessageNotFound)
}

func TestCommentWritesRefreshAnalyticsCache(t *testing.T) {
	assert := assert.New(t)
	assert.True(commentWriteCLICommand([]string{"comment", "moderate", "--delete", "--yes", "42"}))
	assert.False(commentWriteCLICommand([]string{"comment", "moderate", "--delete", "--dry-run", "42"}))
	assert.True(commentWriteCLICommand([]string{"comment", "reply", "--body=Thanks", "42"}))
	assert.False(commentWriteCLICommand([]string{"comment", "reply", "--body=Thanks", "--clear-unknown", "42"}))
	assert.False(manualSyncCLICommand([]string{"comment", "moderate", "--delete", "--yes", "42"}), "a comment change is not reported as a finished sync")
}

func TestCommentReply(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg, st, messageID, runCommand := commentCommandFixture(t)
	id := strconv.FormatInt(messageID, 10)
	run := func(args ...string) (string, error) { return runCommand(newCommentReplyCmd(), args...) }
	archivedReplies := func() int {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE source_message_id = '100_202' AND reply_to_message_id = ?`, messageID).Scan(&n))
		return n
	}
	cfg.FacebookComments[0].Actions = []string{"hide", "delete"}

	_, err := run(id, "--body", "Thanks")
	var refused *sourceCommandError
	require.ErrorAs(err, &refused)
	assert.Equal(`add "reply" to actions in that profile`, refused.Hint())

	cfg.FacebookComments[0].Actions = []string{"reply"}
	output, err := run(id, "--body", "Thanks", "--dry-run")
	require.NoError(err)
	assert.Equal("Would reply to Facebook comment 100_201 by Example Customer (message "+id+"): Example comment\nReply: Thanks\n", output)
	assert.Zero(archivedReplies(), "a dry run does not reply")

	output, err = run(id, "--body", "Thanks")
	require.NoError(err)
	assert.Equal("Replied to Facebook comment 100_201 by Example Customer (message "+id+"); reply 100_202\n", output)
	assert.Equal(1, archivedReplies())

	_, err = run(id, "--body", "Thanks")
	require.ErrorAs(err, &refused)
	assert.Equal(6, refused.ExitCode())
	assert.Equal("change the text to post another reply", refused.Hint())
}
