package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type meetingLinkFakeClient struct {
	version, event, decision, ref string
	id                            int64
	calls                         int
}

func (f *meetingLinkFakeClient) APISchemaVersion(context.Context) (string, error) {
	return f.version, nil
}
func (f *meetingLinkFakeClient) DecideMeetingLink(_ context.Context, id int64, event, decision string) error {
	f.calls++
	f.id, f.event, f.decision = id, event, decision
	return nil
}
func (f *meetingLinkFakeClient) SuggestMeetingLinks(_ context.Context, ref string) ([]store.MeetingSuggestion, error) {
	f.calls++
	f.ref = ref
	return []store.MeetingSuggestion{{Ref: "evt:81@20270314T100000Z", MemberMessageID: 42, OverlapMinutes: 30}}, nil
}

func (f *meetingLinkFakeClient) MeetingLinks(_ context.Context, id int64) ([]store.MeetingLink, error) {
	f.calls++
	f.id = id
	return []store.MeetingLink{{Ref: "evt:81", Subject: "Planning", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), End: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)}}, nil
}

func TestMeetingLinkCommands(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	for _, tc := range []struct {
		name                   string
		args                   []string
		version, wantErr, want string
	}{
		{"links", []string{"links", "42"}, "", "", "Planning"},
		{"links json", []string{"links", "42", "--json"}, "", "", `"ref":"evt:81"`},
		{"old daemon links", []string{"links", "42"}, "3.24.0", "upgrade the daemon", ""},
		{"invalid links ID", []string{"links", "0"}, "", "positive message ID", ""},
		{"link", []string{"link", "42", "--event", "evt:81@20270314T100000Z"}, "", "", ""},
		{"unlink", []string{"unlink", "42", "--event", "evt:81@20270314T100000Z"}, "", "", ""},
		{"suggest note", []string{"suggest", "msg:42"}, "", "", "evt:81@20270314T100000Z"},
		{"suggest event", []string{"suggest", "evt:81@20270314T100000Z", "--json"}, "", "", `"member_message_id":42`},
		{"old daemon link", []string{"link", "42", "--event", "evt:81"}, "3.24.0", "upgrade the daemon", ""},
		{"old daemon unlink", []string{"unlink", "42", "--event", "evt:81"}, "3.24.0", "upgrade the daemon", ""},
		{"old daemon suggest", []string{"suggest", "msg:42"}, "3.24.0", "upgrade the daemon", ""},
		{"invalid note", []string{"link", "0", "--event", "evt:81"}, "", "positive message ID", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fake := &meetingLinkFakeClient{version: tc.version}
			if fake.version == "" {
				fake.version = meetingLinksMinAPISchemaVersion
			}
			cleaned := false
			open := func(context.Context) (meetingLinkClient, func(), error) { return fake, func() { cleaned = true }, nil }
			root := newMeetingsCommand(meetingCommandDeps{})
			for _, child := range root.Commands() {
				if child.Name() == "link" || child.Name() == "unlink" || child.Name() == "suggest" || child.Name() == "links" {
					root.RemoveCommand(child)
				}
			}
			root.AddCommand(newMeetingLinkDecisionCommand("link", open), newMeetingLinkDecisionCommand("unlink", open), newMeetingSuggestCommand(open), newMeetingLinksCommand(open))
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if tc.wantErr != "" {
				requirements.ErrorContains(err, tc.wantErr)
				assertions.Zero(fake.calls)
				if tc.version != "" {
					assertions.True(cleaned)
				}
				return
			}
			requirements.NoError(err)
			assertions.True(cleaned)
			assertions.Equal(1, fake.calls)
			switch tc.args[0] {
			case "suggest":
				assertions.Equal(tc.args[1], fake.ref)
				assertions.Contains(out.String(), tc.want)
			case "links":
				assertions.Equal(int64(42), fake.id)
				assertions.Contains(out.String(), tc.want)
				assertions.Contains(out.String(), "evt:81")
				assertions.Contains(out.String(), "2027-03-14T10:00:00Z")
			default:
				assertions.Equal(int64(42), fake.id)
				assertions.Equal("evt:81@20270314T100000Z", fake.event)
				assertions.Equal(tc.args[0], fake.decision)
			}
		})
	}
	// The production parent includes all four commands.
	root := newMeetingsCommand(meetingCommandDeps{})
	for _, name := range []string{"link", "unlink", "suggest", "links"} {
		command, _, err := root.Find([]string{name})
		requirements.NoError(err)
		assertions.Equal(name, command.Name())
	}
}
