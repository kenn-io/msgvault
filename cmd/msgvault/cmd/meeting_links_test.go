package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type meetingLinkFakeClient struct {
	version, event, decision, ref string
	id                            int64
	calls                         int
	unexpandable                  bool
}

func (f *meetingLinkFakeClient) APISchemaVersion(context.Context) (string, error) {
	return f.version, nil
}
func (f *meetingLinkFakeClient) DecideMeetingLink(_ context.Context, id int64, event, decision string) error {
	f.calls++
	f.id, f.event, f.decision = id, event, decision
	return nil
}
func (f *meetingLinkFakeClient) SuggestMeetingLinks(_ context.Context, ref string) (store.MeetingSuggestionPage, error) {
	f.calls++
	f.ref = ref
	page := store.MeetingSuggestionPage{Suggestions: []store.MeetingSuggestion{{Ref: "evt:81@20270314T100000Z", MemberMessageID: 42, OverlapMinutes: 30}}}
	if f.unexpandable {
		page.Unexpandable = []store.CalendarUnexpandable{{MessageID: 99, Reason: "unsupported recurrence"}}
	}
	return page, nil
}

func (f *meetingLinkFakeClient) MeetingRecord(_ context.Context, ref string) (store.MeetingRecord, error) {
	f.calls++
	f.ref = ref
	return store.MeetingRecord{MessageIDs: []int64{42}, Links: []store.MeetingLink{{Ref: "evt:81", Subject: "Planning", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), End: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)}}}, nil
}

func TestMeetingLinkCommands(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	for _, tc := range []struct {
		name                   string
		args                   []string
		version, wantErr, want string
		unexpandable, warning  bool
	}{
		{"links", []string{"links", "42"}, "", "", "Planning", false, false},
		{"event record", []string{"links", "evt:81@20270314T100000Z"}, "", "", "Notes: 42", false, false},
		{"links json", []string{"links", "42", "--json"}, "", "", `"ref":"evt:81"`, false, false},
		{"old daemon links", []string{"links", "42"}, "3.45.0", "upgrade the daemon", "", false, false},
		{"main daemon links", []string{"links", "42"}, "4.0.0", "upgrade the daemon", "", false, false},
		{"invalid links ID", []string{"links", "0"}, "", "positive message ID", "", false, false},
		{"link", []string{"link", "42", "--event", "evt:81@20270314T100000Z"}, "", "", "", false, false},
		{"unlink", []string{"unlink", "42", "--event", "evt:81@20270314T100000Z"}, "", "", "", false, false},
		{"suggest note", []string{"suggest", "msg:42"}, "", "", "evt:81@20270314T100000Z", false, false},
		{"suggest warning", []string{"suggest", "msg:42"}, "", "", "evt:81@20270314T100000Z", true, true},
		{"suggest event", []string{"suggest", "evt:81@20270314T100000Z", "--json"}, "", "", `"unexpandable":[{"message_id":99,"reason":"unsupported recurrence"}]`, true, false},
		{"old daemon link", []string{"link", "42", "--event", "evt:81"}, "3.24.0", "upgrade the daemon", "", false, false},
		{"old daemon suggest", []string{"suggest", "msg:42"}, "3.24.0", "upgrade the daemon", "", false, false},
		{"invalid note", []string{"link", "0", "--event", "evt:81"}, "", "positive message ID", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			fake := &meetingLinkFakeClient{version: tc.version, unexpandable: tc.unexpandable}
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
			var errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
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
				if tc.args[1] == "msg:42" {
					assertions.Contains(out.String(), "TITLE WORDS")
				}
				if tc.warning {
					assertions.Contains(errOut.String(), "some calendar entries could not be fully expanded")
					assertions.Contains(errOut.String(), "event 99: unsupported recurrence")
				} else {
					assertions.Empty(errOut.String())
				}
			case "links":
				wantRef := "msg:42"
				if tc.name == "event record" {
					wantRef = "evt:81@20270314T100000Z"
				}
				assertions.Equal(wantRef, fake.ref)
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

func TestMeetingLinksCLIEventRecord(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	event, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "event", MessageType: "calendar_event"}, Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"}, RawMIME: []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json"})
	requirements.NoError(err)
	noteSource, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	note, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: noteSource.ID, SourceMessageID: "note", MessageType: "meeting_transcript"}, Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"}, RawMIME: []byte(`{"schema_version":1,"recording":{}}`), RawFormat: "twenty_json"})
	requirements.NoError(err)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	logger := slog.New(slog.DiscardHandler)
	server := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg, logger: logger}, nil, logger).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	command := newMeetingLinksCommand(func(context.Context) (meetingLinkClient, func(), error) { return client, func() {}, nil })
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{fmt.Sprintf("evt:%d", event), "--json"})
	requirements.NoError(command.Execute())
	var record store.MeetingRecord
	requirements.NoError(json.Unmarshal(out.Bytes(), &record))
	assertions.Equal([]int64{note}, record.MessageIDs)
}
