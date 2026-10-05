package calcontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
)

type controlFake struct {
	*gcal.MockAPI

	calls         []string
	calendar      string
	input         gcal.EventInput
	options       gcal.MutationOptions
	patchOptions  []gcal.MutationOptions
	patchInputs   []gcal.EventInput
	result        gcal.Event
	instances     []gcal.Event
	instanceCalls []gcal.EventsListParams
	instancePages map[string]gcal.EventsPage
	busy          gcal.FreeBusyResponse
	busyRequest   gcal.FreeBusyRequest
}

func (f *controlFake) InsertEvent(_ context.Context, cal string, in gcal.EventInput, opt gcal.MutationOptions) (*gcal.Event, error) {
	f.calls = append(f.calls, "insert")
	f.calendar, f.input, f.options = cal, in, opt
	return &f.result, nil
}
func (f *controlFake) PatchEvent(_ context.Context, cal, id string, in gcal.EventInput, opt gcal.MutationOptions) (*gcal.Event, error) {
	f.calls = append(f.calls, "patch:"+id)
	f.patchOptions = append(f.patchOptions, opt)
	f.patchInputs = append(f.patchInputs, in)
	f.calendar, f.input, f.options = cal, in, opt
	return &f.result, nil
}
func (f *controlFake) DeleteEvent(_ context.Context, cal, id string, opt gcal.MutationOptions) error {
	f.calls = append(f.calls, "delete:"+id)
	f.calendar, f.options = cal, opt
	return nil
}
func (f *controlFake) MoveEvent(_ context.Context, cal, id, dest string, opt gcal.MutationOptions) (*gcal.Event, error) {
	f.calls = append(f.calls, "move:"+id)
	f.calendar, f.options = dest, opt
	return &f.result, nil
}
func (f *controlFake) ListInstances(_ context.Context, _, _ string, params gcal.EventsListParams) (*gcal.EventsPage, error) {
	f.instanceCalls = append(f.instanceCalls, params)
	if f.instancePages != nil {
		page := f.instancePages[params.PageToken]
		return &page, nil
	}
	return &gcal.EventsPage{Items: f.instances}, nil
}
func (f *controlFake) FreeBusy(_ context.Context, request gcal.FreeBusyRequest) (*gcal.FreeBusyResponse, error) {
	f.calls = append(f.calls, "freebusy")
	f.busyRequest = request
	return &f.busy, nil
}
func fixture(t *testing.T) (*Service, *controlFake, Request) {
	t.Helper()
	f := &controlFake{MockAPI: gcal.NewMockAPI(), result: gcal.Event{ID: "created", Summary: "Planning"}}
	f.Calendars = []gcal.Calendar{{ID: "team@example.com", AccessRole: "owner", TimeZone: "UTC"}, {ID: "person@example.com", AccessRole: "owner", Primary: true}, {ID: "other@example.com", AccessRole: "writer"}}
	cfg := config.GCalSource{Email: "person@example.com", Enabled: true, WriteCalendars: []string{"team@example.com", "other@example.com"}, InviteCalendars: []string{"team@example.com"}, CalendarAliases: map[string]string{"team": "team@example.com"}}
	s := &Service{Source: cfg, Client: f, Persist: func(context.Context, gcal.Calendar, gcal.Event) (int64, error) { return 42, nil }}
	summary := "Planning"
	start := gcal.EventDateTime{DateTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	end := gcal.EventDateTime{DateTime: start.DateTime.Add(time.Hour), TimeZone: "UTC"}
	return s, f, Request{Action: "create", Account: cfg.Email, CalendarID: "team", Event: gcal.EventInput{Summary: &summary, Start: &start, End: &end}}
}
func TestCreateSharedOwnedCalendar(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	got, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	assertions.Equal("team@example.com", f.calendar)
	assertions.Equal("none", f.options.SendUpdates)
	assertions.Equal(int64(42), got.Writes[0].MessageID)
	assertions.True(got.Writes[0].Archived)
	assertions.Equal("person@example.com", got.Account)
}

func TestCreateRecurrenceUsesTargetCalendarTimeZone(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	f.Calendars[0].TimeZone = "Europe/London"
	r.Event.Start.TimeZone, r.Event.End.TimeZone = "", ""
	r.Event.Recurrence = &[]string{"RRULE:FREQ=WEEKLY;COUNT=4"}
	_, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	assertions.Equal("Europe/London", f.input.Start.TimeZone)
	assertions.Equal("Europe/London", f.input.End.TimeZone)
	assertions.Empty(r.Event.Start.TimeZone)
	assertions.Empty(r.Event.End.TimeZone)
}
func TestWriteChecksFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Service, *controlFake, *Request)
		grant  *agentgrant.Grant
	}{
		{"reader", func(_ *Service, f *controlFake, _ *Request) { f.Calendars[0].AccessRole = "reader" }, nil},
		{"not listed", func(_ *Service, f *controlFake, _ *Request) { f.Calendars = f.Calendars[1:] }, nil},
		{"disabled", func(s *Service, _ *controlFake, _ *Request) { s.Source.Enabled = false }, nil},
		{"no opt in", func(s *Service, _ *controlFake, _ *Request) { s.Source.WriteCalendars = nil }, nil},
		{"read only", func(_ *Service, _ *controlFake, r *Request) { r.ReadOnly = true }, nil},
		{"wrong account", func(_ *Service, _ *controlFake, r *Request) { r.Account = "other@example.com" }, nil},
		{"invite opt in", func(s *Service, _ *controlFake, r *Request) {
			s.Source.InviteCalendars = nil
			r.Event.Attendees = &[]gcal.Attendee{{Email: "guest@example.com"}}
		}, nil},
		{"wrong calendar grant", func(_ *Service, _ *controlFake, _ *Request) {}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, Sources: []agentgrant.SourceRef{{Type: "gcal", Identifier: "person@example.com/other@example.com"}}}},
		{"invite grant", func(_ *Service, _ *controlFake, r *Request) {
			r.Event.Attendees = &[]gcal.Attendee{{Email: "guest@example.com"}}
		}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, Sources: []agentgrant.SourceRef{{Type: "gcal", Identifier: "person@example.com/team@example.com"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, r := fixture(t)
			tc.change(s, f, &r)
			_, err := s.Execute(context.Background(), r, tc.grant)
			require.Error(t, err)
			assert.Empty(t, f.calls)
		})
	}
}
func TestDryRunStillChecksPermissionAndNeverWrites(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.DryRun = true
	got, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	assertions.True(got.DryRun)
	assertions.Empty(f.calls)
	assertions.Empty(got.Writes)
	f.Calendars[0].AccessRole = "reader"
	_, err = s.Execute(context.Background(), r, nil)
	requirements.Error(err)
}

func TestAvailabilityWithExplicitCalendarIDsIgnoresPositionalCalendar(t *testing.T) {
	for _, calendarID := range []string{"", "other@example.com"} {
		t.Run(map[string]string{"": "omitted", "other@example.com": "unused unauthorized calendar"}[calendarID], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, _ := fixture(t)
			f.busy = gcal.FreeBusyResponse{Calendars: map[string]gcal.CalendarBusy{"team@example.com": {Busy: []gcal.BusyPeriod{}}}}
			r := Request{
				Action: "freebusy", Account: "person@example.com", CalendarID: calendarID,
				CalendarIDs: []string{"team@example.com"},
				TimeMin:     time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
				TimeMax:     time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
			}
			grant := &agentgrant.Grant{
				Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarRead},
				Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
			}

			result, err := s.Execute(context.Background(), r, grant)
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.Equal("team@example.com", result.CalendarID)
			assertions.Equal("freebusy", r.Action)
			requirements.Len(f.calls, 1)
			assertions.Equal("freebusy", f.calls[0])
			requirements.Len(f.busyRequest.Items, 1)
			assertions.Equal("team@example.com", f.busyRequest.Items[0].ID)
		})
	}
}
func TestAvailabilityRejectsUnauthorizedExplicitCalendarBeforeListing(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, _ := fixture(t)
	r := Request{
		Action: "freebusy", Account: "person@example.com", CalendarIDs: []string{"unlisted@example.com"},
		TimeMin: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
		TimeMax: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
	}
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarRead},
		Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
	}

	_, err := s.Execute(context.Background(), r, grant)

	requirements.ErrorIs(err, ErrDenied)
	assertions.Contains(err.Error(), "grant lacks calendar.read")
	assertions.Zero(f.ListCalendarsCalls(), "an ungranted target must be rejected before provider lookup")
}

func TestMoveRejectsUnauthorizedDestinationBeforeListing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		destination string
		wantError   string
	}{
		{name: "missing delegated grant", destination: "other@example.com", wantError: "grant lacks calendar.write"},
		{name: "ungranted and not in write policy", destination: "private@example.com", wantError: "grant lacks calendar.write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			r.Action, r.CalendarID, r.EventID, r.Destination = "move", "team@example.com", "event", tc.destination
			r.Event = gcal.EventInput{}
			f.Calendars = append(f.Calendars, gcal.Calendar{ID: "private@example.com", AccessRole: "reader"})
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event", ETag: `"v1"`}}
			grant := &agentgrant.Grant{
				Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite},
				Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
			}

			_, err := s.Execute(context.Background(), r, grant)

			requirements.ErrorIs(err, ErrDenied)
			assertions.Contains(err.Error(), tc.wantError)
			assertions.Zero(f.ListCalendarsCalls(), "an unauthorized move destination must be rejected before provider lookup")
		})
	}
}

func TestPrimaryMoveDestinationAuthorizesCanonicalIDBeforeRoleDisclosure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.Action, r.EventID, r.Destination = "move", "event", "primary"
	r.Event = gcal.EventInput{}
	s.Source.WriteCalendars = append(s.Source.WriteCalendars, "person@example.com")
	f.Calendars[1].AccessRole = "reader"
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event", ETag: `"v1"`}}
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite},
		Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
	}

	_, err := s.Execute(context.Background(), r, grant)

	requirements.ErrorIs(err, ErrDenied)
	assertions.Contains(err.Error(), "calendar access denied")
	assertions.NotContains(err.Error(), "accessRole")
	assertions.Equal(1, f.ListCalendarsCalls(), "primary must be resolved before its canonical ID can be authorized")
}

func TestOrganizerAttendeeDoesNotRequireInviteGrantForUpdate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.Action, r.EventID = actionUpdate, "event"
	updatedSummary := "Updated planning"
	r.Event = gcal.EventInput{Summary: &updatedSummary}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {
		ID: "event", ETag: `"v1"`, Attendees: []gcal.Attendee{{Email: "person@example.com", Organizer: true, ResponseStatus: "accepted"}},
	}}
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite},
		Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
	}

	_, err := s.Execute(context.Background(), r, grant)

	requirements.NoError(err)
	assertions.Equal([]string{"patch:event"}, f.calls)
}

func TestWriteReceiptSurvivesArchiveFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	s.Persist = func(context.Context, gcal.Calendar, gcal.Event) (int64, error) { return 0, errors.New("disk full") }
	got, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.Len(f.calls, 1)
	requirements.Len(got.Writes, 1)
	assertions.False(got.Writes[0].Archived)
	assertions.Contains(got.Writes[0].ArchiveError, "disk full")
	assertions.Equal("created", got.Writes[0].Event.ID)
}

func TestDelegatedWriteReceiptsRequireEventContentGrantForDetails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		wantDetails bool
	}{
		{
			name:        "write and invite without read",
			permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite},
		},
		{
			name:        "availability read does not expose event details",
			permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite, agentgrant.PermissionCalendarRead},
		},
		{
			name:        "event content permission exposes event details",
			permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite, agentgrant.PermissionCalendarEventRead},
			wantDetails: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			r.Action, r.EventID = actionUpdate, "event"
			updatedSummary := "Updated planning"
			r.Event = gcal.EventInput{Summary: &updatedSummary}
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {
				ID: "event", ETag: `"v1"`, Attendees: []gcal.Attendee{{Email: "guest@example.com", ResponseStatus: "accepted"}},
			}}
			f.result = gcal.Event{
				ID: "event", ETag: `"v2"`, Status: gcal.StatusConfirmed, Summary: "Private planning",
				Description: "Private agenda", Location: "Private room",
				Attendees: []gcal.Attendee{{Email: "guest@example.com", ResponseStatus: "accepted"}},
			}
			var archived gcal.Event
			s.Persist = func(_ context.Context, _ gcal.Calendar, event gcal.Event) (int64, error) {
				archived = event
				return 42, nil
			}
			grant := &agentgrant.Grant{
				Permissions: tc.permissions,
				Sources:     []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}},
			}

			result, err := s.Execute(context.Background(), r, grant)
			requirements.NoError(err)
			requirements.Len(result.Writes, 1)
			receipt := result.Writes[0]
			assertions.Equal("event", receipt.Event.ID)
			assertions.Equal(gcal.StatusConfirmed, receipt.Event.Status)
			assertions.Equal("Private planning", archived.Summary)
			assertions.Equal("Private agenda", archived.Description)
			requirements.Len(archived.Attendees, 1)
			assertions.Equal("guest@example.com", archived.Attendees[0].Email)
			if tc.wantDetails {
				assertions.Equal(`"v2"`, receipt.Event.ETag)
				assertions.Equal("Private planning", receipt.Event.Summary)
				assertions.Equal("Private agenda", receipt.Event.Description)
				requirements.Len(receipt.Event.Attendees, 1)
				assertions.Equal("guest@example.com", receipt.Event.Attendees[0].Email)
			} else {
				assertions.Empty(receipt.Event.ETag)
				assertions.Empty(receipt.Event.Summary)
				assertions.Empty(receipt.Event.Description)
				assertions.Empty(receipt.Event.Location)
				assertions.Empty(receipt.Event.Attendees)
			}
		})
	}
}

func TestAddAttendeesPreservesExistingAndUsesETag(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.Action = "update"
	r.EventID = "event"
	r.Event = gcal.EventInput{}
	r.AddAttendees = []string{"guest@example.com", "new@example.com"}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event", ETag: `"v1"`, Attendees: []gcal.Attendee{
		{Email: "person@example.com", Self: true, ResponseStatus: "accepted"},
		{Email: "organizer@example.com", Organizer: true},
		{Email: "guest@example.com", ResponseStatus: "accepted"},
	}}}
	_, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	assertions.Equal(`"v1"`, f.options.IfMatch)
	requirements.NotNil(f.input.Attendees)
	requirements.Len(*f.input.Attendees, 4)
	assertions.False((*f.input.Attendees)[0].Self)
	assertions.False((*f.input.Attendees)[0].Organizer)
	assertions.Equal("accepted", (*f.input.Attendees)[0].ResponseStatus)
	assertions.False((*f.input.Attendees)[1].Self)
	assertions.False((*f.input.Attendees)[1].Organizer)
	assertions.Equal("accepted", (*f.input.Attendees)[2].ResponseStatus)
}
func TestRecurringScopes(t *testing.T) {
	for _, scope := range []string{"single", "all"} {
		t.Run(scope, func(t *testing.T) {
			s, f, r := fixture(t)
			r.Action = "delete"
			r.Event = gcal.EventInput{}
			r.EventID = "instance"
			r.Scope = scope
			f.EventsByID["team@example.com"] = map[string]gcal.Event{"instance": {ID: "instance", RecurringEventID: "series"}, "series": {ID: "series", Recurrence: []string{"RRULE:FREQ=DAILY"}}}
			_, err := s.Execute(context.Background(), r, nil)
			require.NoError(t, err)
			want := "delete:instance"
			if scope == "all" {
				want = "delete:series"
			}
			assert.Equal(t, []string{want}, f.calls)
		})
	}
}
func TestRespondRejectsSelfOrganizer(t *testing.T) {
	s, f, r := fixture(t)
	r.Action, r.EventID, r.Response = "respond", "event", "declined"
	r.Event = gcal.EventInput{}
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {
		ID: "event", Attendees: []gcal.Attendee{{Email: "team@example.com", Self: true, Organizer: true}},
	}}
	_, err := s.Execute(context.Background(), r, nil)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "organizer")
	assert.Empty(t, f.calls)
}

func TestRespondOnlySelf(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, f, r := fixture(t)
	r.Action = "respond"
	r.EventID = "event"
	r.Event = gcal.EventInput{}
	r.Response = "accepted"
	f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event", ETag: `"v1"`, Attendees: []gcal.Attendee{{Email: "person@example.com", Self: true, ResponseStatus: "declined"}, {Email: "guest@example.com"}}}}
	_, err := s.Execute(context.Background(), r, nil)
	requirements.NoError(err)
	requirements.NotNil(f.input.Attendees)
	requirements.Len(*f.input.Attendees, 1)
	assertions.Equal("person@example.com", (*f.input.Attendees)[0].Email)
	assertions.False((*f.input.Attendees)[0].Self)
	assertions.False((*f.input.Attendees)[0].Organizer)
	assertions.Equal("accepted", (*f.input.Attendees)[0].ResponseStatus)
	requirements.NotNil(f.input.AttendeesOmitted)
	assertions.True(*f.input.AttendeesOmitted)
}
func TestFreeBusyDoesNotHideProviderErrors(t *testing.T) {
	s, f, r := fixture(t)
	r.Action = "freebusy"
	r.Event = gcal.EventInput{}
	r.TimeMin = time.Now()
	r.TimeMax = r.TimeMin.Add(time.Hour)
	f.busy.Calendars = map[string]gcal.CalendarBusy{"team@example.com": {Errors: []gcal.CalendarError{{Reason: "notFound"}}}}
	_, err := s.Execute(context.Background(), r, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notFound")
}

func TestGrantChecksPrecedeConfiguredAllowlists(t *testing.T) {
	for _, tc := range []struct {
		name         string
		invite, move bool
	}{
		{name: "source write"}, {name: "source invite", invite: true}, {name: "destination before source policy", move: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, Sources: []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}}}
			s.Source.WriteCalendars = nil
			s.Source.InviteCalendars = nil
			if tc.invite {
				r.Event.Attendees = &[]gcal.Attendee{{Email: "guest@example.com"}}
			} else if tc.move {
				r.Action, r.EventID, r.Destination = "move", "event", "other@example.com"
				r.Event = gcal.EventInput{}
			} else {
				grant.Sources = nil
			}
			_, err := s.Execute(t.Context(), r, grant)
			requirements.ErrorIs(err, ErrDenied)
			assertions.Contains(err.Error(), "grant lacks")
			assertions.NotContains(err.Error(), "_calendars")
			assertions.Zero(f.ListCalendarsCalls())
		})
	}
}

func TestDelegatedPrimaryLookupHidesSetupUntilExactAuthorization(t *testing.T) {
	for _, target := range []string{"primary", "alias", "destination", "availability"} {
		for _, state := range []string{"provider failure", "missing primary", "ungranted primary", "valid primary"} {
			t.Run(target+"/"+state, func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				s, f, r := fixture(t)
				s.Source.WriteCalendars = append(s.Source.WriteCalendars, "person@example.com")
				s.Source.CalendarAliases["self"] = "primary"
				r.CalendarID = "primary"
				grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarRead}, Sources: []agentgrant.SourceRef{{Type: gcal.SourceType, Identifier: "person@example.com/team@example.com"}}}
				switch target {
				case "alias":
					r.CalendarID = "self"
				case "destination":
					r.Action, r.CalendarID, r.EventID, r.Destination = "move", "team", "event", "primary"
					r.Event = gcal.EventInput{}
					f.EventsByID["team@example.com"] = map[string]gcal.Event{"event": {ID: "event"}}
				case "availability":
					r.Action = "freebusy"
					r.CalendarID = ""
					r.CalendarIDs = []string{"self"}
					r.Event = gcal.EventInput{}
					r.TimeMin = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
					r.TimeMax = r.TimeMin.Add(time.Hour)
					f.busy = gcal.FreeBusyResponse{Calendars: map[string]gcal.CalendarBusy{"person@example.com": {}}}
				}
				switch state {
				case "provider failure":
					f.ListCalendarsErr = errors.New("OAuth setup detail")
				case "missing primary":
					f.Calendars = f.Calendars[:1]
				case "valid primary":
					grant.Sources = append(grant.Sources, agentgrant.SourceRef{Type: gcal.SourceType, Identifier: "person@example.com/person@example.com"})
				}
				_, err := s.Execute(t.Context(), r, grant)
				if state == "valid primary" {
					requirements.NoError(err)
					return
				}
				requirements.ErrorIs(err, ErrDenied)
				assertions.Equal("calendar operation denied: calendar access denied", err.Error())
				assertions.Zero(f.GetEventCalls())
				assertions.Empty(f.calls)
			})
		}
	}
}

func TestWriteGateRunsAfterPlanningAndReleasesAfterPersistence(t *testing.T) {
	for _, stage := range []string{"dry run", "invalid plan", "changed plan", "gate denied", "successful write"} {
		t.Run(stage, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, f, r := fixture(t)
			acquired, released := false, false
			gateErr := errors.New("write gate busy")
			s.AcquireWrite = func(context.Context) (func(), error) {
				acquired = true
				assertions.Empty(f.calls)
				if stage == "gate denied" {
					return nil, gateErr
				}
				return func() { released = true }, nil
			}
			s.Persist = func(context.Context, gcal.Calendar, gcal.Event) (int64, error) {
				assertions.True(acquired)
				assertions.False(released)
				return 42, nil
			}
			switch stage {
			case "dry run":
				r.DryRun = true
			case "invalid plan":
				r.Event.End = r.Event.Start
			case "changed plan":
				r.ExpectedPlanFingerprint = "stale"
			}
			_, err := s.Execute(t.Context(), r, nil)
			switch stage {
			case "invalid plan":
				requirements.ErrorIs(err, ErrInvalid)
			case "changed plan":
				requirements.ErrorIs(err, ErrPlanChanged)
			case "gate denied":
				requirements.ErrorIs(err, gateErr)
			default:
				requirements.NoError(err)
			}
			assertions.Equal(stage == "gate denied" || stage == "successful write", acquired)
			assertions.Equal(stage == "successful write", released)
			if stage != "successful write" {
				assertions.Empty(f.calls)
			}
		})
	}
}
