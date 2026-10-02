package notionmeetings

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUserSource struct {
	pages     map[string]*UserPage
	users     map[string]*User
	errs      map[string]error
	listed    []string
	retrieved []string
}

func (f *fakeUserSource) ListUsers(_ context.Context, cursor string) (*UserPage, error) {
	f.listed = append(f.listed, cursor)
	return f.pages[cursor], f.errs["list"]
}

func (f *fakeUserSource) RetrieveUser(_ context.Context, id string) (*User, error) {
	f.retrieved = append(f.retrieved, id)
	return f.users[id], f.errs[id]
}

func TestHydratorSeparateUsersCredentialAndGuestCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := completeHydrationSource()
	source.usersErr = ErrUserInformation // PAT cannot list workspace users.
	users := &fakeUserSource{
		pages: map[string]*UserPage{
			"":     {HasMore: true, NextCursor: "next"},
			"next": {Results: []User{{ID: "user-1", Name: "Member", Type: "person", Person: UserPerson{Email: "member@example.com", EmailVerified: true}}}},
		},
		users: map[string]*User{"guest": {Object: "user", ID: "guest", Name: "Guest", Type: "person", Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}},
		errs:  map[string]error{"blocked": ErrUserInformation},
	}
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"blocked", "user-1", "guest"}
	h := NewHydrator(source).WithUserSource(users)
	for range 2 {
		got, err := h.Hydrate(t.Context(), meeting)
		require.NoError(err)
		require.Len(got.Attendees, 2)
		assert.Equal("member@example.com", got.Attendees[0].Email)
		assert.Equal("guest@example.com", got.Attendees[1].Email)
		assert.Equal(userAnchor("guest"), got.Attendees[1].Anchor)
		assert.Equal([]string{"blocked"}, got.UnresolvedAttendeeIDs)
		assert.True(got.AttendeeResolutionDegraded)
	}
	assert.Zero(source.usersCalls)
	assert.Equal([]string{"", "next"}, users.listed)
	assert.Equal([]string{"blocked", "guest"}, users.retrieved)
}

func TestHydratorRetrievesUsersWhenListingIsUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	users := &fakeUserSource{
		users: map[string]*User{
			"user-1": {Object: "user", ID: "user-1", Name: "Test Attendee", Type: "person", Person: UserPerson{
				Email: "attendee@example.com", EmailVerified: true,
			}},
		},
		errs: map[string]error{"list": ErrUserInformation, "user-2": ErrUserInformation},
	}
	result, err := NewHydrator(completeHydrationSource()).WithUserSource(users).Hydrate(
		t.Context(), hydrationMeeting(),
	)
	require.NoError(err)
	require.Len(result.Attendees, 1)
	assert.Equal("attendee@example.com", result.Attendees[0].Email)
	assert.Equal(userAnchor("user-1"), result.Attendees[0].Anchor)
	assert.Equal([]string{"user-2"}, result.UnresolvedAttendeeIDs)
	assert.Equal([]string{""}, users.listed)
	assert.Equal([]string{"user-1", "user-2"}, users.retrieved)
	assert.Equal(map[string]bool{"user-2": true}, result.failedAttendeeIDs)
	assert.True(result.AttendeeResolutionDegraded)
	assert.Contains(result.Warnings, "Notion User Information access unavailable; attempting per-attendee retrieval where supported")
	assert.Contains(result.Warnings, "Notion attendee user lookup unavailable; retained display-only identity")
}

func TestRetrieveUserValidatesIdentityAndPATRestriction(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"guest", `{"object":"user","id":"guest","type":"person","person":{"email":"guest@example.com","email_verified":true}}`, 200, nil},
		{"PAT", `{"object":"error","code":"restricted_resource","message":"Personal access tokens can only retrieve their own authorized user"}`, 403, ErrUserInformation},
		{"wrong ID", `{"object":"user","id":"other"}`, 200, ErrMalformedResponse},
		{"wrong object", `{"object":"block","id":"guest"}`, 200, ErrMalformedResponse},
		{"null", `null`, 200, ErrMalformedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/users/guest", r.URL.Path)
				assert.Equal(t, "Bearer users-secret", r.Header.Get("Authorization"))
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			got, err := NewClient(srv.URL, "users-secret").RetrieveUser(t.Context(), "guest")
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "guest@example.com", got.Person.Email)
		})
	}
}

func TestImporterPerUserFailureDoesNotRestoreHealthyUnverifiedEmail(t *testing.T) {
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[1].Person.EmailVerified = true
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
	require.NoError(err)
	users := &fakeUserSource{pages: map[string]*UserPage{"": {Results: []User{{ID: "user-2", Name: "Unverified Attendee", Type: "person", Person: UserPerson{Email: "unverified@example.com"}}}}}, errs: map[string]error{"user-1": ErrUserInformation}}
	imp.WithUserSource(users)
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true})
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_id=? AND source_message_id=?"), first.SourceID, "meeting-1").Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 1)
	assert.Equal(t, "attendee@example.com", recipients[0].EmailAddress)
}

func TestPATListUsersRestrictionHasSafeDiagnostic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v1/users", r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"object":"error","code":"restricted_resource","message":"Personal access tokens cannot list users; secret-token"}`)
	}))
	t.Cleanup(server.Close)
	_, err := NewClient(server.URL, "secret-token").ListUsers(t.Context(), "")
	require.ErrorIs(err, ErrUserInformation)
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.True(apiErr.PersonalAccessToken)
	assert.NotContains(err.Error(), "secret-token")
}

// A single-token integration can resolve guests without WithUserSource.
type guestHydrationSource struct {
	*fakeHydrationSource

	retrieval *fakeUserSource
}

func (s *guestHydrationSource) RetrieveUser(ctx context.Context, id string) (*User, error) {
	return s.retrieval.RetrieveUser(ctx, id)
}

func TestHydratorMeetingTokenRetrievesGuestsWithoutSeparateCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	users := &fakeUserSource{
		users: map[string]*User{"guest": {Object: "user", ID: "guest", Name: "Guest", Type: "person", Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}},
		errs:  map[string]error{"blocked": ErrUserInformation},
	}
	source := &guestHydrationSource{fakeHydrationSource: completeHydrationSource(), retrieval: users}
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"guest", "blocked"}
	hydrator := NewHydrator(source)
	for range 2 {
		result, err := hydrator.Hydrate(t.Context(), meeting)
		require.NoError(err)
		require.Len(result.Attendees, 1)
		assert.Equal("guest@example.com", result.Attendees[0].Email)
		assert.Equal(userAnchor("guest"), result.Attendees[0].Anchor)
		assert.Equal([]string{"blocked"}, result.UnresolvedAttendeeIDs)
		assert.True(result.AttendeeResolutionDegraded)
		assert.Equal(map[string]bool{"blocked": true}, result.failedAttendeeIDs)
	}
	assert.Equal(1, source.usersCalls)
	assert.Equal([]string{"guest", "blocked"}, users.retrieved)
}

func TestHydratorGuestFailuresHaveOneWarningPerMeeting(t *testing.T) {
	users := &fakeUserSource{pages: map[string]*UserPage{"": {}}, errs: map[string]error{"blocked-1": ErrUserInformation, "blocked-2": ErrUserInformation}}
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"blocked-1", "blocked-2"}
	result, err := NewHydrator(completeHydrationSource()).WithUserSource(users).Hydrate(t.Context(), meeting)
	require.NoError(t, err)
	assert.Equal(t, []string{"blocked-1", "blocked-2"}, result.UnresolvedAttendeeIDs)
	assert.Equal(t, []string{"Notion attendee user lookup unavailable; retained display-only identity"}, result.Warnings)
}
