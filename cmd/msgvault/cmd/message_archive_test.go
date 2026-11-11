package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

type cliArchiveBackend struct {
	id      int64
	change  emailtags.ArchiveChange
	failure error
}

func TestArchiveCommandRefuses50Daemon(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"5.0.0"}`))
			return
		}
		posts++
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "fixture", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	command := newMessageArchiveCommand(true, client)
	command.SetArgs([]string{"--message", "7"})
	command.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "upgrade the daemon")
	assert.Zero(t, posts)
}

func (b *cliArchiveBackend) SupportsAPISchemaVersion(context.Context, string) (bool, error) {
	return true, nil
}
func (b *cliArchiveBackend) ArchiveMessage(_ context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error) {
	b.id, b.change = id, change
	if b.failure != nil {
		return nil, b.failure
	}
	return &emailtags.ArchiveResult{MessageID: id, Archived: *change.Archived, Changed: true, DryRun: change.DryRun}, nil
}
func TestArchiveMessageCommands(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	for _, archived := range []bool{true, false} {
		backend := &cliArchiveBackend{}
		command := newMessageArchiveCommand(archived, backend)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetArgs([]string{"--message", "7", "--dry-run", "--json"})
		requirementsForTest.NoError(command.ExecuteContext(t.Context()))
		assertionsForTest.Equal(int64(7), backend.id)
		assertionsForTest.Equal(archived, *backend.change.Archived)
		assertionsForTest.True(backend.change.DryRun)
		assertionsForTest.Contains(output.String(), `"dry_run":true`)
	}
	for _, args := range [][]string{{"--message", "0"}, {"--message", "-1"}, {"--message", "7", "extra"}} {
		backend := &cliArchiveBackend{}
		command := newMessageArchiveCommand(true, backend)
		command.SetArgs(args)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		requirementsForTest.Error(command.ExecuteContext(t.Context()))
		assertionsForTest.Zero(backend.id)
	}

	failure := emailtags.Failure("remote_unknown", "Read current tags", &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"UNREAD"}}, nil)
	backend := &cliArchiveBackend{failure: failure}
	command := newMessageArchiveCommand(true, backend)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--message", "7", "--json"})
	requirementsForTest.ErrorIs(command.ExecuteContext(t.Context()), failure)
	assertionsForTest.Contains(output.String(), `"error":"remote_unknown"`)
	assertionsForTest.Contains(output.String(), `"tags":["UNREAD"]`)
}
