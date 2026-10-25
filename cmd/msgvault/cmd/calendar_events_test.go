package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
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

type calendarEventsFakeClient struct {
	err      error
	page     store.CalendarOccurrencePage
	version  string
	from, to time.Time
}

func (f *calendarEventsFakeClient) APISchemaVersion(context.Context) (string, error) {
	return f.version, nil
}
func (f *calendarEventsFakeClient) ListCalendarOccurrencesContext(_ context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	f.from, f.to = from, to
	return f.page, f.err
}
func TestCalendarEventsCommand(t *testing.T) {
	start, err := time.Parse(time.RFC3339, "2027-03-02T17:00:00Z")
	require.NoError(t, err)
	for _, tc := range []struct {
		name                       string
		args                       []string
		page                       store.CalendarOccurrencePage
		version                    string
		want, wantErr, wantWarning string
		err                        error
	}{
		{name: "table", page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{Ref: "evt:377", MessageID: 378, SeriesMessageID: 377, Subject: "Standup", Status: "confirmed", Start: start, End: start.Add(time.Hour)}, {Ref: "evt:412", MessageID: 412, Subject: "Offsite", Status: "confirmed", AllDay: true, StartDate: "2027-03-03", EndDate: "2027-03-04"}}, Unexpandable: []store.CalendarUnexpandable{{MessageID: 500, Reason: "unsupported recurrence"}}}, want: "Standup", wantWarning: "1 event could not be expanded: message 500 (unsupported recurrence)"},
		{name: "empty", want: "No archived calendar events from 2027-03-01 to 2027-04-01."},
		{name: "json", args: []string{"--json"}, page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{Ref: "evt:377", MessageID: 378, SeriesMessageID: 377, Subject: "Standup", Start: start, End: start.Add(time.Hour)}}, Unexpandable: []store.CalendarUnexpandable{}}, want: `"message_id":378`},
		{name: "deleted", page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{Ref: "evt:378", MessageID: 378, Subject: "Removed event", Status: "confirmed", DeletedFromSource: true, Start: start, End: start.Add(time.Hour)}}}, want: "deleted"},
		{name: "terminal controls", page: store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{Ref: "evt:378", MessageID: 378, Subject: "Planning\x1b[31m red\x1b[0m\nmeeting", Status: "confirmed", Start: start, End: start.Add(time.Hour)}}}, want: "Planning red meeting"},
		{name: "too broad", err: &daemonclient.APIError{Status: 400, Code: "query_too_broad", Message: "calendar query is too broad; long-running series count steps from their start; a narrower window may not reduce the work"}, wantErr: "calendar query is too broad; long-running series count steps from their start; a narrower window may not reduce the work"},
		{name: "series cap", err: &daemonclient.APIError{Status: 400, Code: "query_too_broad", Message: "calendar query is too broad: more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series"}, wantErr: "calendar query is too broad: more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series"},
		{name: "old daemon", version: "3.25.0", wantErr: "calendar events require daemon API schema 3.26.0 or newer"},
		{name: "account", args: []string{"--account", "calendar@example.com"}, wantErr: "--account is only valid"},
		{name: "dry run", args: []string{"--dry-run"}, wantErr: "--dry-run is only valid"},
		{name: "read only", args: []string{"--read-only"}, wantErr: "--read-only is only valid"},
		{name: "send updates", args: []string{"--send-updates", "none"}, wantErr: "--send-updates is only valid"},
		{name: "reversed", args: []string{"--to", "2027-02-01"}, wantErr: "--to must be after --from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			fake := &calendarEventsFakeClient{page: tc.page, version: tc.version, err: tc.err}
			if fake.version == "" {
				fake.version = calendarEventsMinAPISchemaVersion
			}
			root := newCalendarControlCmd(nil)
			for _, child := range root.Commands() {
				if child.Name() == "events" {
					root.RemoveCommand(child)
				}
			}
			root.AddCommand(newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return fake, func() {}, nil }))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SilenceErrors = true
			root.SilenceUsage = true
			root.SetArgs(append([]string{"events", "--from", "2027-03-01", "--to", "2027-04-01"}, tc.args...))
			err := root.Execute()
			if tc.wantErr != "" {
				requirements.ErrorContains(err, tc.wantErr)
				if tc.name == "too broad" || tc.name == "series cap" {
					requirements.EqualError(err, tc.wantErr)
					requirements.ErrorIs(err, store.ErrQueryTooBroad)
				}
				return
			}
			requirements.NoError(err)
			assertions.Contains(out.String(), tc.want)
			if tc.wantWarning == "" {
				assertions.Empty(stderr.String())
			} else {
				assertions.Contains(stderr.String(), tc.wantWarning)
			}
			if tc.name == "terminal controls" {
				assertions.NotContains(out.String(), "\x1b")
				assertions.Len(bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")), 2)
			}
			assertions.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.Local), fake.from)
			if tc.name == "table" {
				assertions.Contains(out.String(), "END (EXCL)")
				assertions.Contains(out.String(), "2027-03-03")
				assertions.Contains(out.String(), "2027-03-04")
				assertions.Contains(out.String(), "ID")
				assertions.Regexp(`\s378\s+Standup`, out.String())
			}
			if tc.name == "json" {
				var page store.CalendarOccurrencePage
				requirements.NoError(json.Unmarshal(out.Bytes(), &page))
				requirements.Len(page.Occurrences, 1)
				assertions.Equal("Standup", page.Occurrences[0].Subject)
				assertions.Equal(int64(378), page.Occurrences[0].MessageID)
				assertions.Equal(int64(377), page.Occurrences[0].SeriesMessageID)
			}
		})
	}
}
func TestCalendarEventsRFC3339Bounds(t *testing.T) {
	fake := &calendarEventsFakeClient{version: calendarEventsMinAPISchemaVersion}
	cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return fake, func() {}, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--from", "2027-03-01T12:00:00-05:00", "--to", "2027-03-02T12:00:00-05:00"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "2027-03-01T17:00:00Z", fake.from.UTC().Format(time.RFC3339))
}

func TestCalendarEventsDateBoundsSkippedMidnight(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv("TZ", "America/Sao_Paulo")
	zone, err := time.LoadLocation("America/Sao_Paulo")
	require.NoError(err)
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	fake := &calendarEventsFakeClient{version: calendarEventsMinAPISchemaVersion}
	cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return fake, func() {}, nil })
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--from", "2018-11-04", "--to", "2018-11-05"})
	require.NoError(cmd.Execute())
	assert.Equal("2018-11-04T03:00:00Z", fake.from.UTC().Format(time.RFC3339))
	assert.Equal("2018-11-05T02:00:00Z", fake.to.UTC().Format(time.RFC3339))
	assert.Equal("2018-11-04", fake.from.Format(time.DateOnly))
	assert.Equal("2018-11-05", fake.to.Format(time.DateOnly))
}

func TestCalendarEventsInvalidBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing from", []string{"--to", "2027-03-03"}, `required flag(s) "from" not set`},
		{"missing to", []string{"--from", "2027-03-02"}, `required flag(s) "to" not set`},
		{"local from", []string{"--from", "2027-03-02T09:00", "--to", "2027-03-03"}, "--from: use YYYY-MM-DD or RFC3339 with an offset"},
		{"local to", []string{"--from", "2027-03-02", "--to", "2027-03-03T17:00"}, "--to: use YYYY-MM-DD or RFC3339 with an offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) {
				require.FailNow(t, "invalid bounds reached daemon")
				return nil, nil, nil
			})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			require.ErrorContains(t, cmd.Execute(), tc.want)
		})
	}
}

func TestCalendarEventsSkippedMidnightWindow(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	t.Setenv("TZ", "America/Sao_Paulo")
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	var ids []int64
	for _, event := range []struct{ key, raw string }{
		{"before", `{"id":"before","start":{"date":"2018-11-03"},"end":{"date":"2018-11-04"}}`},
		{"after", `{"id":"after","start":{"date":"2018-11-04"},"end":{"date":"2018-11-05"}}`},
		{"timed", `{"id":"timed","start":{"dateTime":"2018-11-04T02:30:00Z"},"end":{"dateTime":"2018-11-04T02:45:00Z"}}`},
		{"before-start", `{"id":"before-start","start":{"date":"2017-10-14"},"end":{"date":"2017-10-15"}}`},
		{"at-start", `{"id":"at-start","start":{"date":"2017-10-15"},"end":{"date":"2017-10-16"}}`},
	} {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: event.key, MessageType: "calendar_event"},
			Conversation: &store.ConversationPersistData{SourceConversationID: event.key, ConversationType: "calendar"},
			RawMIME:      []byte(event.raw), RawFormat: "gcal_json",
		})
		requirements.NoError(err)
		ids = append(ids, id)
	}
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	logger := slog.New(slog.DiscardHandler)
	daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg, logger: logger}, nil, logger).Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	for _, tc := range []struct {
		name, from, to string
		want           []int64
	}{
		{"exclusive date", "2018-11-03", "2018-11-04", []int64{ids[0], ids[2]}},
		{"inclusive date", "2018-11-04", "2018-11-05", []int64{ids[1]}},
		{"ordinary midnight", "2018-11-02", "2018-11-03", []int64{}},
		{"normal partial day before skipped midnight", "2018-11-02T00:00:00-03:00", "2018-11-03T00:30:00-03:00", []int64{ids[0]}},
		{"both midnights skipped", "2017-10-15", "2018-11-04", []int64{ids[0], ids[2], ids[4]}},
		{"RFC3339 partial day", "2018-11-03T00:00:00-03:00", "2018-11-04T01:00:00-02:00", ids[:3]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return client, func() {}, nil })
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"--from", tc.from, "--to", tc.to, "--json"})
			requirements.NoError(cmd.Execute())
			var page store.CalendarOccurrencePage
			requirements.NoError(json.Unmarshal(out.Bytes(), &page))
			got := make([]int64, 0, len(page.Occurrences))
			for _, occurrence := range page.Occurrences {
				got = append(got, occurrence.MessageID)
			}
			assertions.ElementsMatch(tc.want, got)
			assertions.Empty(page.Unexpandable)
		})
	}
}

func TestCalendarEventsSkippedDateWindow(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	t.Setenv("TZ", "Pacific/Apia")
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	for _, raw := range []string{
		`{"id":"before","summary":"Before","start":{"date":"2011-12-29"},"end":{"date":"2011-12-30"}}`,
		`{"id":"skipped","summary":"Skipped","start":{"date":"2011-12-30"},"end":{"date":"2011-12-31"}}`,
		`{"id":"after","summary":"After","start":{"date":"2011-12-31"},"end":{"date":"2012-01-01"}}`,
		`{"id":"timed","summary":"Timed","start":{"dateTime":"2011-12-30T09:30:00Z"},"end":{"dateTime":"2011-12-30T10:30:00Z"}}`,
	} {
		var event struct {
			ID      string `json:"id"`
			Summary string `json:"summary"`
		}
		requirements.NoError(json.Unmarshal([]byte(raw), &event))
		_, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: event.ID, MessageType: "calendar_event", Subject: sql.NullString{String: event.Summary, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: event.ID, ConversationType: "calendar"},
			RawMIME:      []byte(raw), RawFormat: "gcal_json",
		})
		requirements.NoError(err)
	}
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	logger := slog.New(slog.DiscardHandler)
	daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg, logger: logger}, nil, logger).Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	for _, tc := range []struct {
		name, from, to string
		want           []string
	}{
		{"exclusive skipped date", "2011-12-29", "2011-12-30", []string{"Before", "Timed"}},
		{"inclusive skipped date", "2011-12-30", "2011-12-31", []string{"Skipped"}},
		{"spanning skipped date", "2011-12-29", "2011-12-31", []string{"Before", "Skipped", "Timed"}},
		{"starting skipped date", "2011-12-30", "2012-01-01", []string{"Skipped", "After", "Timed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			cmd := newCalendarEventsCommand(func(context.Context) (calendarEventsClient, func(), error) { return client, func() {}, nil })
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--from", tc.from, "--to", tc.to, "--json"})
			requirements.NoError(cmd.Execute())
			var page store.CalendarOccurrencePage
			requirements.NoError(json.Unmarshal(out.Bytes(), &page))
			var got []string
			for _, occurrence := range page.Occurrences {
				got = append(got, occurrence.Subject)
			}
			assertions.ElementsMatch(tc.want, got)
			assertions.Empty(page.Unexpandable)
		})
	}
}
