package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type meetingDecisionClient struct {
	meetingRecordClient

	args []string
}

func (c *meetingDecisionClient) DecideMeetingLink(_ context.Context, left, right, decision string) (store.MeetingLinkDecisionResult, error) {
	c.args = []string{left, right, decision}
	state := map[string]string{"link": "link", "unlink": "split", "clear": "automatic"}[decision]
	return store.MeetingLinkDecisionResult{Left: left, Right: right, State: state}, nil
}

func TestMeetingDecisionCLIOutputAndSchemaGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct{ action, decision, output string }{
		{"link", "link", "Saved link: msg:3 evt:7@20270314\n"},
		{"unlink", "unlink", "Saved split: msg:3 evt:7@20270314\n"},
		{"reset-link", "clear", "No saved choice: msg:3 evt:7@20270314\n"},
	} {
		client := &meetingDecisionClient{version: "5.3.0"}
		command := newMeetingsCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetArgs([]string{tc.action, "msg:3", "evt:7@20270314"})
		require.NoError(command.Execute())
		assert.Equal(tc.output, out.String())
		assert.Equal([]string{"msg:3", "evt:7@20270314", tc.decision}, client.args)
	}
	client := &meetingDecisionClient{version: "5.1.0"}
	command := newMeetingsCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
	command.SetArgs([]string{"link", "msg:3", "msg:4"})
	require.ErrorContains(command.Execute(), "5.3.0")
	assert.Empty(client.args)
}
