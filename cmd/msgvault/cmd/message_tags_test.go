package cmd

import (
	"bytes"
	"context"
	"io"
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
	if change != nil && !change.DryRun {
		result.ReceiptID, result.ReceiptStatus, result.IdempotencyKey = "receipt-cli", "verified", "key-cli"
	}
	if b.fail {
		return result, emailtags.Failure("remote_unknown", "Read tags before retrying", result, nil)
	}
	return result, nil
}

type messageTagFailWriter struct{}

func (messageTagFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestMessageTagsCommandShowsReceipt(t *testing.T) {
	assertions := assert.New(t)

	backend := &cliTagsBackend{}
	cmd := newMessageTagsCommand(backend)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"7", "--add", "Next"})
	require.NoError(t, cmd.Execute())
	assertions.Contains(out.String(), "receipt-cli")
	assertions.Contains(out.String(), "verified")
	assertions.Equal(1, backend.calls)
}

func TestMessageTagsCommandOutputLossKeepsReceipt(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "JSON"}[asJSON], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			backend := &cliTagsBackend{}
			cmd := newMessageTagsCommand(backend)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(messageTagFailWriter{})
			args := []string{"7", "--add", "Next"}
			if asJSON {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			var failure *emailtags.Error
			requirements.ErrorAs(err, &failure)
			assertions.Equal("remote_unknown", failure.Code)
			requirements.NotNil(failure.Result)
			assertions.Equal("receipt-cli", failure.Result.ReceiptID)
			assertions.Equal(1, backend.calls)
		})
	}
}
func TestMessageTagsCommandReadEditAndPartialJSON(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
	requirements.NoError(err)
	assertions.Nil(backend.change)
	assertions.Equal("INBOX", backend.mailbox)
	out, err := run("7", "--add", "Next", "--remove", "Old", "--dry-run", "--json")
	requirements.NoError(err)
	requirements.NotNil(backend.change)
	assertions.True(backend.change.DryRun)
	assertions.Contains(out, `"dry_run":true`)
	before := backend.calls
	for _, args := range [][]string{{"0"}, {"7", "--dry-run"}} {
		_, err := run(args...)
		requirements.Error(err)
	}
	assertions.Equal(before, backend.calls)
	backend.fail = true
	out, err = run("7", "--add", "Next", "--json")
	requirements.Error(err)
	assertions.Contains(out, `"error":"remote_unknown"`)
	assertions.Contains(out, `"result"`)
}
