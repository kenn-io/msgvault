package cmd

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func assertZoomNeedsToken(t *testing.T, name string) {
	t.Helper()
	opened := false
	cmd := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { opened = true; return nil, func() {}, nil }})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	args := []string{name, "--account", "example", "--owner-id", "owner"}
	if name == "import-zoom-meeting" {
		args = append(args, "--uuid", "instance")
	}
	cmd.SetArgs(args)
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "token-command")
	assert.False(t, opened)
}
func TestZoomFetchValidation(t *testing.T) { assertZoomNeedsToken(t, "import-zoom-meeting") }
