package cmd

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCommentModerateOptInAndConfirmation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := registerFacebookCommentsProfile(t.Context(), st, cfg.FacebookComments[0])
	require.NoError(err)
	// The fixture denies ad posts; the comment on the organic post is archived.
	_, _ = importFacebookCommentsProfile(t.Context(), st, cfg.FacebookComments[0], meta.ImportOptions{})
	ids, err := st.MessageExistsBatch(source.ID, []string{"100_201"})
	require.NoError(err)
	id := strconv.FormatInt(ids["100_201"], 10)

	run := func(args ...string) (string, error) {
		cmd := newCommentModerateCmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
		err := cmd.Execute()
		return output.String(), err
	}
	hidden := func() bool {
		target, err := meta.ResolveCommentTarget(t.Context(), st, ids["100_201"])
		require.NoError(err)
		return target.Hidden
	}

	_, err = run(id, "--hide")
	var refused *sourceCommandError
	require.ErrorAs(err, &refused)
	assert.Equal(5, refused.ExitCode())
	assert.Equal(`add "hide" to actions in that profile`, refused.Hint())
	assert.False(hidden(), "a profile without actions never changes the comment")

	cfg.FacebookComments[0].Actions = []string{"hide"}
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
	_, err = meta.ResolveCommentTarget(t.Context(), st, ids["100_201"])
	require.ErrorIs(err, store.ErrMessageNotFound)
}
