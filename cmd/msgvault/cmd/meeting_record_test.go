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
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	start, end := &at, new(at.Add(time.Hour))
	record := store.MeetingRecord{
		Links: []store.MeetingLink{
			{Ref: "evt:1", Start: start, End: end, Status: "confirmed"},
			{Ref: "evt:2", Start: start, End: end, Status: "cancelled"},
			{Ref: "evt:3", Start: start, End: end, Status: "confirmed", DeletedFromSource: true},
			{Ref: "evt:4", Status: "cancelled"},
		},
		MessageIDs: []int64{42}, Unexpandable: []store.CalendarUnexpandable{{MessageID: 7, Reason: "unsupported recurrence"}},
	}
	for _, tc := range []struct {
		name       string
		jsonOutput bool
		noNotes    bool
	}{{"table", false, false}, {"json", true, false}, {"without notes", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			input := record
			if tc.noNotes {
				input.MessageIDs = []int64{}
			}
			client := &meetingRecordClient{version: "5.4.0", record: &input}
			command := newMeetingLinksCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
			var out, stderr bytes.Buffer
			command.SetOut(&out)
			command.SetErr(&stderr)
			args := []string{"evt:1"}
			if tc.jsonOutput {
				args = append(args, "--json")
			}
			command.SetArgs(args)
			require.NoError(command.Execute())
			if tc.jsonOutput {
				assert.Contains(out.String(), `"unexpandable":[{"message_id":7,"reason":"unsupported recurrence"}]`)
				assert.Empty(stderr.String())
			} else {
				assert.Contains(out.String(), "STATUS")
				assert.Regexp(`evt:4\s+unknown\s+unknown`, out.String())
				assert.Regexp(`evt:3\s+\S+\s+\S+\s+deleted`, out.String())
				if tc.noNotes {
					assert.Contains(out.String(), "Notes: none\n")
				} else {
					assert.Contains(out.String(), "Notes: 42\n")
				}
				assert.Contains(stderr.String(), "message 7 (unsupported recurrence)")
			}
		})
	}
}

func TestMeetingLinksCLIReferenceNormalizationAndSchemaGate(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"evt:23", "evt:23"}, {"23", "msg:23"}} {
		t.Run(tc.input, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			client := &meetingRecordClient{version: "5.4.0"}
			command := newMeetingLinksCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
			var out bytes.Buffer
			command.SetOut(&out)
			command.SetArgs([]string{tc.input, "--json"})
			require.NoError(command.Execute())
			assert.Equal([]string{tc.want}, client.refs)
		})
	}
	t.Run("previous schema", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		client := &meetingRecordClient{version: "5.3.0"}
		command := newMeetingLinksCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
		command.SetArgs([]string{"evt:23", "--json"})
		require.ErrorContains(command.Execute(), "daemon API schema 5.4.0 or newer")
		assert.Empty(client.refs)
	})
}
