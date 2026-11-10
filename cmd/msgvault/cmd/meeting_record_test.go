package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

type meetingRecordClient struct {
	mcp.MeetingBackend

	refs    []string
	version string
	record  *store.MeetingRecord
}

func (c *meetingRecordClient) APISchemaVersion(context.Context) (string, error) {
	return c.version, nil
}

func (c *meetingRecordClient) MeetingRecord(_ context.Context, ref string) (store.MeetingRecord, error) {
	c.refs = append(c.refs, ref)
	if c.record != nil {
		return *c.record, nil
	}
	return store.MeetingRecord{Links: []store.MeetingLink{}, MessageIDs: []int64{42}, Unexpandable: []store.CalendarUnexpandable{}}, nil
}

func TestMeetingLinksCLIPartialRecord(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	record := store.MeetingRecord{
		Links: []store.MeetingLink{
			{Ref: "evt:1", Start: at, End: at.Add(time.Hour), Status: "confirmed"},
			{Ref: "evt:2", Start: at, End: at.Add(time.Hour), Status: "cancelled"},
			{Ref: "evt:3", Start: at, End: at.Add(time.Hour), Status: "confirmed", DeletedFromSource: true},
		},
		MessageIDs: []int64{42}, Unexpandable: []store.CalendarUnexpandable{{MessageID: 7, Reason: "unsupported recurrence"}},
	}
	for _, jsonOutput := range []bool{false, true} {
		client := &meetingRecordClient{version: "5.1.0", record: &record}
		command := newMeetingLinksCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
		var out, stderr bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&stderr)
		args := []string{"evt:1"}
		if jsonOutput {
			args = append(args, "--json")
		}
		command.SetArgs(args)
		require.NoError(command.Execute())
		if jsonOutput {
			assert.Contains(out.String(), `"unexpandable":[{"message_id":7,"reason":"unsupported recurrence"}]`)
			assert.Empty(stderr.String())
		} else {
			assert.Contains(out.String(), "STATUS")
			assert.Contains(out.String(), "confirmed")
			assert.Contains(out.String(), "cancelled")
			assert.Contains(out.String(), "deleted")
			assert.Contains(stderr.String(), "message 7 (unsupported recurrence)")
		}
	}
}

func TestMeetingLinksCLIEventRecord(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"evt:23", "evt:23"}, {"23", "msg:23"}} {
		t.Run(tc.input, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			client := &meetingRecordClient{version: "5.1.0"}
			command := newMeetingLinksCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
			var out bytes.Buffer
			command.SetOut(&out)
			command.SetArgs([]string{tc.input, "--json"})
			require.NoError(command.Execute())
			assert.Equal([]string{tc.want}, client.refs)
		})
	}
	t.Run("previous schema", func(t *testing.T) {
		client := &meetingRecordClient{version: "5.0.0"}
		command := newMeetingLinksCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
		command.SetArgs([]string{"evt:23", "--json"})
		require.ErrorContains(t, command.Execute(), "daemon API schema 5.1.0 or newer")
		assert.Empty(t, client.refs)
	})
}
