package notionmeetings

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUserSource struct {
	users     map[string]*User
	errs      map[string]error
	retrieved []string
}

func (f *fakeUserSource) RetrieveUser(_ context.Context, id string) (*User, error) {
	f.retrieved = append(f.retrieved, id)
	return f.users[id], f.errs[id]
}

func TestRetrieveUserValidatesIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"guest", `{"object":"user","id":"guest","type":"person","person":{"email":"guest@example.com","email_verified":true}}`, 200, nil},
		{"restricted", `{"object":"error","code":"restricted_resource","message":"Personal access tokens can only retrieve their own authorized user"}`, 403, ErrUserInformation},
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

func TestImporterOptionalServiceFailureStopsRunAndRetriesNextSync(t *testing.T) {
	for _, failure := range []error{ErrUnauthorized, ErrUserInformation, &APIError{Kind: ErrRateLimited, Status: 429}, &APIError{Kind: ErrRateLimited, Status: 503}, errors.New("transport failed"), context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			st, source, imp := newImporterFixture(t)
			opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
			_, err := imp.Import(t.Context(), opts)
			require.NoError(t, err)
			meeting := hydrationMeeting()
			meeting.ID = "meeting-2"
			meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "new-user"}
			block := *source.blocks["meeting-1"]
			block.ID = meeting.ID
			source.blocks[meeting.ID] = &block
			source.query.Results = append(source.query.Results, meeting)
			users := &fakeUserSource{errs: map[string]error{"user-1": failure}}
			summary, err := imp.WithUserSource(users).Import(t.Context(), opts)
			require.NoError(t, err)
			assert.Equal(t, int64(2), summary.MeetingsProcessed)
			assert.Equal(t, []string{"user-1"}, users.retrieved)
			var messageID int64
			require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'meeting-1'`).Scan(&messageID))
			body, err := st.GetMessageBodyText(messageID)
			require.NoError(t, err)
			assert.Contains(t, body, "Test Speaker: Ready to ship.")
			recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
			require.NoError(t, err)
			require.Len(t, recipients, 1)
			assert.Equal(t, "attendee@example.com", recipients[0].EmailAddress)
			users.errs = map[string]error{"user-2": ErrMalformedResponse, "new-user": ErrMalformedResponse}
			users.users = map[string]*User{"user-1": {Person: UserPerson{Email: "recovered@example.com", EmailVerified: true}}}
			summary, err = imp.Import(t.Context(), opts)
			require.NoError(t, err)
			assert.Equal(t, int64(2), summary.MeetingsUpdated)
			assert.Equal(t, []string{"user-1", "user-1", "user-2", "new-user"}, users.retrieved)
		})
	}
}

func TestImporterOrdinarySyncUpdatesAndPreservesOnlyFailedAttendees(t *testing.T) {
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[1].Person.EmailVerified = true
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	users := &fakeUserSource{users: map[string]*User{"user-2": {Person: UserPerson{Email: "unverified@example.com"}}, "guest": {Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}}, errs: map[string]error{"user-1": &APIError{Kind: ErrProvider, Status: 404}, "malformed": ErrMalformedResponse}}
	source.query.Results[0].MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "user-2", "guest", "malformed"}
	second, err := imp.WithUserSource(users).Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), second.MeetingsUpdated)
	var messageID int64
	require.NoError(t, st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'meeting-1'`).Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(t, err)
	require.Len(t, recipients, 2)
	assert.ElementsMatch(t, []string{"attendee@example.com", "guest@example.com"}, []string{recipients[0].EmailAddress, recipients[1].EmailAddress})
}

func TestHydratorCachesSuccessfulAndMissingUsersAcrossServiceFailure(t *testing.T) {
	users := &fakeUserSource{users: map[string]*User{"user-1": {Person: UserPerson{Email: "member@example.com", EmailVerified: true}}, "unverified": {}}, errs: map[string]error{"missing": &APIError{Kind: ErrProvider, Status: 404}, "malformed": ErrMalformedResponse, "blocked": ErrUserInformation}}
	source := completeHydrationSource()
	h := NewHydrator(source).WithUserSource(users)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"missing", "malformed", "user-1", "unverified", "blocked", "new-user"}
	for range 2 {
		result, err := h.Hydrate(t.Context(), meeting)
		require.NoError(t, err)
		require.Len(t, result.Attendees, 1)
		assert.Equal(t, "member@example.com", result.Attendees[0].Email)
		assert.Equal(t, map[string]bool{"missing": true, "malformed": true, "blocked": true, "new-user": true}, result.failedAttendeeIDs)
		assert.Len(t, result.Warnings, 1)
	}
	assert.Zero(t, source.usersCalls)
	assert.Equal(t, []string{"missing", "malformed", "user-1", "unverified", "blocked"}, users.retrieved)
}

func TestHydratorOptionalLocalBudgetsDoNotPoisonCache(t *testing.T) {
	for _, budget := range []string{"requests", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			source := completeHydrationSource()
			users := &fakeUserSource{users: map[string]*User{"user-1": {Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}}}
			meeting := hydrationMeeting()
			meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1"}
			if budget == "requests" {
				pages := map[string]*BlockPage{}
				for i := 0; i < maxHydrationRequests-5; i++ {
					cursor := ""
					if i > 0 {
						cursor = fmt.Sprint(i)
					}
					pages[cursor] = &BlockPage{HasMore: i < maxHydrationRequests-6, NextCursor: fmt.Sprint(i + 1)}
				}
				source.children["notes-1"] = pages
			} else {
				source.markdown.Raw = make(jsontext.Value, maxHydrationBytes-(1<<20))
				users.users["user-1"].Raw = make(jsontext.Value, 2<<20)
			}
			h := NewHydrator(source).WithUserSource(users)
			first, err := h.Hydrate(t.Context(), meeting)
			require.NoError(t, err)
			assert.Equal(t, "Test Speaker: Ready to ship.", first.Transcript)
			assert.True(t, first.failedAttendeeIDs["user-1"])
			assert.Empty(t, h.users)
			assert.False(t, h.usersUnavailable)
			source.children["notes-1"] = map[string]*BlockPage{"": {}}
			source.markdown.Raw, users.users["user-1"].Raw = nil, nil
			second, err := h.Hydrate(t.Context(), meeting)
			require.NoError(t, err)
			require.Len(t, second.Attendees, 1)
			assert.Equal(t, "guest@example.com", second.Attendees[0].Email)

		})
	}
}

type usersRetryTransport struct{}

func (usersRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v1/users/user-1" {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"object":"user","id":"user-1","person":{"email":"guest@example.com","email_verified":true}}`))}, nil
	}
	return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"object":"error","code":"service_unavailable"}`))}, nil
}

func TestHydratorOptionalDeadlineAndParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("https://notion.example", "users-example")
		client.http.Transport = usersRetryTransport{}
		h := NewHydrator(completeHydrationSource()).WithUserSource(client)
		meeting := hydrationMeeting()
		meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1"}
		_, err := h.Hydrate(t.Context(), meeting)
		require.NoError(t, err)
		remaining := h.userLookupRemaining
		time.Sleep(time.Hour)
		assert.Equal(t, remaining, h.userLookupRemaining)
		meeting.MeetingNotes.CalendarEvent.Attendees = append(meeting.MeetingNotes.CalendarEvent.Attendees, "user-2")
		second, err := h.Hydrate(t.Context(), meeting)
		require.NoError(t, err)
		assert.Equal(t, "Test Speaker: Ready to ship.", second.Transcript)
		require.Len(t, second.Attendees, 1)
		assert.ErrorIs(t, h.usersFailure, context.DeadlineExceeded)
		assert.LessOrEqual(t, h.userLookupRemaining, time.Duration(0))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = h.Hydrate(ctx, hydrationMeeting())
		require.ErrorIs(t, err, context.Canceled)
	})
}
