package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

type cliTagsBackend struct {
	calls   int
	id      int64
	change  *emailtags.Change
	mailbox string
	fail    bool
}

func (b *cliTagsBackend) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	b.calls++
	b.id = id
	b.change = change
	b.mailbox = mailbox
	result := &emailtags.Result{MessageID: id, Provider: "imap", Tags: []string{"Next"}, DryRun: change != nil && change.DryRun, Verified: change == nil || !change.DryRun}
	if b.fail {
		return result, emailtags.Failure("remote_unknown", "Read tags before retrying", result, nil)
	}
	return result, nil
}
func TestMessageTagsCommandReadEditAndPartialJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &cliTagsBackend{}
	run := func(args ...string) (string, error) {
		cmd := newMessageTagsCommand(backend)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	_, err := run("7", "--mailbox", "INBOX")
	require.NoError(err)
	assert.Nil(backend.change)
	assert.Equal("INBOX", backend.mailbox)
	out, err := run("7", "--add", "Next", "--remove", "Old", "--dry-run", "--json")
	require.NoError(err)
	require.NotNil(backend.change)
	assert.True(backend.change.DryRun)
	assert.Contains(out, `"dry_run":true`)
	before := backend.calls
	for _, args := range [][]string{{"0"}, {"7", "--dry-run"}, {"7", "--add", "Next", "--remove", "Next"}} {
		_, err := run(args...)
		require.Error(err)
	}
	assert.Equal(before, backend.calls)
	backend.fail = true
	out, err = run("7", "--add", "Next", "--json")
	require.Error(err)
	assert.Contains(out, `"error":"remote_unknown"`)
	assert.Contains(out, `"result"`)
}
