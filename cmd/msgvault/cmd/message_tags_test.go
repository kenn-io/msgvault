package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

type cliTagsBackend struct {
	calls   int
	id      int64
	change  *emailtags.MessageTagChange
	mailbox string
	fail    bool
}

func (b *cliTagsBackend) SupportsAPISchemaVersion(context.Context, string) (bool, error) {
	return true, nil
}
func (b *cliTagsBackend) MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string) (*emailtags.MessageTagResult, error) {
	b.calls++
	b.id = id
	b.change = change
	b.mailbox = mailbox
	result := &emailtags.MessageTagResult{MessageID: id, Provider: "imap", Tags: []string{"Next"}, DryRun: change != nil && change.DryRun, Verified: change == nil || !change.DryRun}
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
	for _, args := range [][]string{{"0"}, {"7", "--dry-run"}} {
		_, err := run(args...)
		require.Error(err)
	}
	assert.Equal(before, backend.calls)
	backend.fail = true
	out, err = run("7", "--add", "Next")
	require.Error(err)
	assert.Contains(out, "Last observed tags: Next")
	out, err = run("7", "--add", "Next", "--json")
	require.Error(err)
	assert.Contains(out, `"error":"remote_unknown"`)
	assert.Contains(out, `"result"`)
}

func TestMessageTagsClientRequiresSchema(t *testing.T) {
	requests := 0
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.14.0"}`))
			return
		}
		requests++
	})
	command := newMessageTagsCommand(client)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"7"})
	err := command.Execute()
	require.ErrorContains(t, err, "API schema "+messageTagsMinAPISchemaVersion)
	assert.Zero(t, requests)
}

type cliInboxTagsBackend struct{ cliTagsBackend }

func (b *cliInboxTagsBackend) MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string) (*emailtags.MessageTagResult, error) {
	result, err := b.cliTagsBackend.MessageTags(ctx, id, change, mailbox)
	result.ReceiptID, result.ReceiptStatus, result.IdempotencyKey = "receipt-fixture", "verified", "tags-fixture"
	return result, err
}
func TestInboxMessageTagsCommandPrintsReceipt(t *testing.T) {
	command := newMessageTagsCommand(&cliInboxTagsBackend{})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"7", "--add", "Next"})
	require.NoError(t, command.Execute())
	assert.Contains(t, output.String(), "Inbox receipt: receipt-fixture (verified)")
}

type inboxTagOutputWriter struct{}

func (inboxTagOutputWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestInboxMessageTagsOutputFailureNamesRecoverableReceipt(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[asJSON], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backend := &cliInboxTagsBackend{}
			command := newMessageTagsCommand(backend)
			command.SetOut(inboxTagOutputWriter{})
			command.SetErr(io.Discard)
			command.SilenceUsage, command.SilenceErrors = true, true
			args := []string{"7", "--add", "Next"}
			if asJSON {
				args = append(args, "--json")
			}
			command.SetArgs(args)
			err := command.Execute()
			requirements.ErrorContains(err, "receipt-fixture")
			failure, ok := errors.AsType[*emailtags.MessageTagError](err)
			requirements.True(ok)
			assertions.Equal("remote_unknown", failure.Code)
			requirements.NotNil(failure.Result)
			assertions.Equal("receipt-fixture", failure.Result.ReceiptID)
			assertions.Equal(1, backend.calls)
		})
	}
}
