package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type suggestionCLIClient struct {
	meetingRecordClient

	called bool
	ref    string
	limit  int
}

func (c *suggestionCLIClient) SuggestMeetingLinks(_ context.Context, ref string, limit int) (store.MeetingSuggestionPage, error) {
	c.called = true
	c.ref, c.limit = ref, limit
	return store.MeetingSuggestionPage{Suggestions: []store.MeetingSuggestion{{Ref: "evt:7", MemberMessageID: 3, Title: "Planning", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), OverlapMinutes: 30, SharedAttendees: 2}}, Unexpandable: []store.CalendarUnexpandable{}}, nil
}
func TestMeetingSuggestionsCLI(t *testing.T) {
	for _, tc := range []struct {
		version string
		json    bool
		want    string
		failure bool
		limit   string
	}{
		{"5.4.0", false, "Planning", false, ""}, {"5.4.0", true, `"title":"Planning"`, false, ""}, {"5.3.0", false, "5.4.0", true, ""},
		{"5.4.0", false, "limit must be between 1 and 100", true, "0"}, {"5.4.0", false, "limit must be between 1 and 100", true, "-1"}, {"5.4.0", false, "limit must be between 1 and 100", true, "101"},
	} {
		t.Run(tc.version+tc.want+tc.limit, func(t *testing.T) {
			r := require.New(t)
			client := &suggestionCLIClient{version: tc.version}
			command := newMeetingsCommand(meetingCommandDeps{open: func(context.Context) (meetingCommandClient, func(), error) { return client, func() {}, nil }})
			var out bytes.Buffer
			command.SetOut(&out)
			args := []string{"suggest", "msg:3"}
			if tc.limit != "" {
				args = append(args, "--limit", tc.limit)
			}
			if tc.json {
				args = append(args, "--json")
			}
			command.SetArgs(args)
			err := command.Execute()
			if tc.failure {
				r.ErrorContains(err, tc.want)
				r.False(client.called)
				return
			}
			r.NoError(err)
			r.Contains(out.String(), tc.want)
			r.True(client.called)
			r.Equal("msg:3", client.ref)
			r.Equal(10, client.limit)
		})
	}
}
