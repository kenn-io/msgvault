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
	return store.MeetingLinkDecisionResult{Left: left, Right: right, State: decision}, nil
}

func TestMeetingDecisionCLIQuietAndVersioned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, action := range []string{"link", "unlink"} {
		client := &meetingDecisionClient{meetingRecordClient: meetingRecordClient{version: "5.2.0"}}
		command := newMeetingsCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetArgs([]string{action, "msg:3", "evt:7@20270314"})
		require.NoError(command.Execute())
		assert.Empty(out.String())
		assert.Equal([]string{"msg:3", "evt:7@20270314", action}, client.args)
	}
	client := &meetingDecisionClient{meetingRecordClient: meetingRecordClient{version: "5.1.0"}}
	command := newMeetingsCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
	command.SetArgs([]string{"link", "msg:3", "msg:4"})
	require.ErrorContains(command.Execute(), "5.2.0")
	assert.Empty(client.args)
}
