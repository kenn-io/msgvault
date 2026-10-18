package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

func TestMeetingRecordsCLIJSONThroughDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	note, _ := meetingRecordAdapterNote(t, f)
	ref := fmt.Sprintf("evt:%d", f.eventID)
	deps := meetingRecordsCommandDeps{open: func(context.Context) (meetingRecordsCommandClient, func(), error) { return f.owner, func() {}, nil }}
	for _, tc := range []struct {
		name    string
		command *cobra.Command
		args    []string
		count   int
	}{
		{"show", newMeetingShowCommand(deps), []string{ref, "--json"}, 1},
		{"unlink", newMeetingLinkCommand(deps, true), []string{ref, strconv.FormatInt(note, 10), "--json"}, 0},
		{"link", newMeetingLinkCommand(deps, false), []string{ref, strconv.FormatInt(note, 10), "--json"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var output bytes.Buffer
			tc.command.SetOut(&output)
			tc.command.SetArgs(tc.args)
			requirements.NoError(tc.command.ExecuteContext(t.Context()))
			var record store.MeetingRecord
			requirements.NoError(json.Unmarshal(output.Bytes(), &record))
			assertions.Equal(ref, record.Ref)
			assertions.Len(record.Items, tc.count)
		})
	}
	suggest := newMeetingSuggestCommand(deps)
	var output bytes.Buffer
	suggest.SetOut(&output)
	suggest.SetArgs([]string{ref, "--json"})
	requirements.NoError(suggest.ExecuteContext(t.Context()))
	var suggestions []store.MeetingSuggestion
	requirements.NoError(json.Unmarshal(output.Bytes(), &suggestions))
	participant, err := f.st.EnsureParticipant("guest@example.com", "Synthetic Guest", "example.com")
	requirements.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), participant, note)
	requirements.NoError(err)
	person, _, err := f.st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	command := newPersonMeetingsCommand(deps)
	output.Reset()
	command.SetOut(&output)
	command.SetArgs([]string{fmt.Sprintf("person:%d", person.ID), "--from", "2027-03-01T00:00:00Z", "--to", "2027-03-02T00:00:00Z", "--json"})
	requirements.NoError(command.ExecuteContext(t.Context()))
	assertions.Contains(output.String(), `"item_count":1`)
}

func TestMeetingRecordsHiddenForT4OnlyDaemon(t *testing.T) {
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	owner, handler, src := f.owner, f.handler, f.sourceID
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"message.read"}, []int64{src}, nil, time.Time{})
	requirements.NoError(err)
	for _, version := range []string{"3.24.0", "3.25.0"} {
		t.Run(version, func(t *testing.T) {
			// A real daemon serves every route; overriding health models the supported
			// compatibility boundary with an older daemon.
			requirements := require.New(t)
			assertions := assert.New(t)
			calendarAvailable := daemonclient.APISchemaVersionAtLeast(version, "3.24.0")
			meetingRecordsAvailable := daemonclient.APISchemaVersionAtLeast(version, "3.25.0")

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			for _, delegated := range []bool{false, true} {
				cfg := daemonclient.Config{URL: server.URL, AllowInsecure: true, APIKey: "owner"}
				if delegated {
					cfg.APIKey = ""
					cfg.AgentToken = grant.Secret
				}
				client, err := daemonclient.New(cfg)
				requirements.NoError(err)
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				if delegated {
					opts, err := delegatedMCPServeOptions(t.Context(), client)
					requirements.NoError(err)
					assertions.Equal(meetingRecordsAvailable, opts.MeetingRecords != nil)
					assertions.NotNil(opts.Calendar)
					assertions.Equal(calendarAvailable, opts.ArchiveCalendarEvents != nil)
				} else {
					opts := daemonMCPServeOptions(t.Context(), client, nil)
					assertions.Equal(meetingRecordsAvailable, opts.MeetingRecords != nil)
					assertions.NotNil(opts.Calendar)
					assertions.Equal(calendarAvailable, opts.ArchiveCalendarEvents != nil)
				}
			}
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, APIKey: "owner"})
			requirements.NoError(err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			deps := meetingRecordsCommandDeps{open: func(context.Context) (meetingRecordsCommandClient, func(), error) { return client, func() {}, nil }}
			cmd := newMeetingShowCommand(deps)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{fmt.Sprintf("evt:%d", f.eventID), "--json"})
			err = cmd.ExecuteContext(t.Context())
			if !meetingRecordsAvailable {
				requirements.ErrorContains(err, "upgrade the daemon")
			} else {
				requirements.NoError(err)
			}
		})
	}
}
