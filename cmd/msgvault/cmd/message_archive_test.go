package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

type cliArchiveBackend struct {
	id         int64
	change     emailtags.ArchiveChange
	compatible bool
	failure    error
}

func (b *cliArchiveBackend) SupportsAPISchemaVersion(context.Context, string) (bool, error) {
	return b.compatible, nil
}
func (b *cliArchiveBackend) ArchiveMessage(_ context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error) {
	b.id, b.change = id, change
	if b.failure != nil {
		return nil, b.failure
	}
	return &emailtags.ArchiveResult{MessageID: id, Archived: *change.Archived, Changed: true, DryRun: change.DryRun}, nil
}
func TestArchiveMessageCommands(t *testing.T) {
	for _, archived := range []bool{true, false} {
		backend := &cliArchiveBackend{compatible: true}
		command := newMessageArchiveCommand(archived, backend)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetArgs([]string{"--message", "7", "--dry-run", "--json"})
		require.NoError(t, command.ExecuteContext(t.Context()))
		assert.Equal(t, int64(7), backend.id)
		assert.Equal(t, archived, *backend.change.Archived)
		assert.True(t, backend.change.DryRun)
		assert.Contains(t, output.String(), `"dry_run":true`)
		assert.True(t, agentDelegatedCommand(command.Name()))
	}
	for _, tc := range []struct {
		args       []string
		compatible bool
	}{
		{nil, true},
		{[]string{"--message", "0"}, true},
		{[]string{"--message", "-1"}, true},
		{[]string{"--message", "x"}, true},
		{[]string{"--message", "7", "extra"}, true},
		{[]string{"--message", "7"}, false},
	} {
		backend := &cliArchiveBackend{compatible: tc.compatible}
		command := newMessageArchiveCommand(true, backend)
		command.SetArgs(tc.args)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		require.Error(t, command.ExecuteContext(t.Context()))
		assert.Zero(t, backend.id)
	}
}

func TestArchiveMessageCommandPartialErrorJSON(t *testing.T) {
	failure := emailtags.Failure("remote_unknown", "Read current tags", &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"UNREAD"}}, nil)
	backend := &cliArchiveBackend{compatible: true, failure: failure}
	command := newMessageArchiveCommand(true, backend)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--message", "7", "--json"})
	require.ErrorIs(t, command.ExecuteContext(t.Context()), failure)
	assert.Contains(t, output.String(), `"error":"remote_unknown"`)
	assert.Contains(t, output.String(), `"tags":["UNREAD"]`)
}
