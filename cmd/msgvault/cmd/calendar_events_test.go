package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestCalendarEventsCLIJSON(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := calendarEventsAPIFixture(t)
	owner, id := f.owner, f.eventID
	deps := calendarEventsCommandDeps{open: func(context.Context) (calendarEventsCommandClient, func(), error) { return owner, func() {}, nil }}
	cmd := newCalendarEventsCommand(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--from", "2027-03-01T00:00:00Z", "--to", "2027-03-02T00:00:00Z", "--json"})
	requirements.NoError(cmd.ExecuteContext(t.Context()))
	assertions.Contains(out.String(), `"items":[`)
	assertions.Contains(out.String(), `"subject":"Planning"`)
	get := newCalendarEventCommand(deps)
	out.Reset()
	get.SetOut(&out)
	get.SetArgs([]string{fmt.Sprintf("evt:%d", id), "--json"})
	requirements.NoError(get.ExecuteContext(t.Context()))
	assertions.JSONEq(fmt.Sprintf(`{"ref":"evt:%d","message_id":%d,"series_message_id":0,"source_id":1,"subject":"Planning","original":{"Instant":"2027-03-01T10:00:00Z","Date":"","AllDay":false},"start":"2027-03-01T10:00:00Z","end":"2027-03-01T11:00:00Z","bounds_known":true,"all_day":false,"status":"confirmed","cancelled":false,"deleted_from_source":false,"attendees":[]}`, id, id), out.String())
}

func TestArchiveCalendarEventsDaemonVersionGate(t *testing.T) {
	f := calendarEventsAPIFixture(t)
	owner, handler, src := f.owner, f.handler, f.sourceID
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"message.read"}, []int64{src}, nil, time.Time{})
	require.NoError(t, err)
	for _, version := range []string{"3.23.0", "3.24.0", "3.25.0"} {
		t.Run(version, func(t *testing.T) {
			// A real daemon serves every route; overriding health models the supported
			// compatibility boundary with an older daemon.
			requirements := require.New(t)
			assertions := assert.New(t)
			available := daemonclient.APISchemaVersionAtLeast(version, "3.24.0")

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
					assertions.Equal(available, opts.ArchiveCalendarEvents != nil)
					assertions.NotNil(opts.Calendar)
				} else {
					opts := daemonMCPServeOptions(t.Context(), client, nil)
					assertions.Equal(available, opts.ArchiveCalendarEvents != nil)
					assertions.NotNil(opts.Calendar)
				}
			}
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, APIKey: "owner"})
			requirements.NoError(err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			deps := calendarEventsCommandDeps{open: func(context.Context) (calendarEventsCommandClient, func(), error) { return client, func() {}, nil }}
			cmd := newCalendarEventsCommand(deps)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--from", "2027-03-01T00:00:00Z", "--to", "2027-03-02T00:00:00Z", "--json"})
			err = cmd.ExecuteContext(t.Context())
			if !available {
				requirements.ErrorContains(err, "upgrade the daemon")
			} else {
				requirements.NoError(err)
			}
		})
	}
}

func TestCalendarEventsFractionalWindow(t *testing.T) {
	owner := calendarEventsAPIFixture(t).owner
	deps := calendarEventsCommandDeps{open: func(context.Context) (calendarEventsCommandClient, func(), error) { return owner, func() {}, nil }}
	cmd := newCalendarEventsCommand(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--from", "2027-03-01T11:00:00.001Z", "--to", "2027-03-01T11:00:00.002Z", "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"items":[],"unexpandable":[],"unexpandable_more":false}`, out.String())
}
