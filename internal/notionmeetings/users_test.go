package notionmeetings

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestImporterOrdinarySyncUpdatesAndPreservesOnlyFailedAttendees(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[1].Person.EmailVerified = true
	opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	meeting := hydrationMeeting()
	meeting.ID = "meeting-2"
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "new-user"}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	source.query.Results = append(source.query.Results, meeting)

	users := &fakeUserSource{users: map[string]*User{"user-2": {Person: UserPerson{Email: "unverified@example.com"}}, "guest": {Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}}, errs: map[string]error{"user-1": &APIError{Kind: ErrProvider, Status: http.StatusNotFound}, "malformed": ErrMalformedResponse, "new-user": ErrMalformedResponse}}
	source.query.Results[0].MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "user-2", "guest", "malformed"}
	installMeetingHeader(t, source.fakeHydrationSource, source.query.Results[0])
	second, err := imp.WithUserSource(users).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id = 'meeting-1'`).Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 2)
	assert.ElementsMatch([]string{"attendee@example.com", "guest@example.com"}, []string{recipients[0].EmailAddress, recipients[1].EmailAddress})
	users = &fakeUserSource{errs: map[string]error{"user-1": ErrUserInformation}}
	summary, err := imp.WithUserSource(users).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(2), summary.MeetingsProcessed)
	assert.Equal([]string{"user-1"}, users.retrieved)
	users.errs = map[string]error{"malformed": ErrMalformedResponse, "new-user": ErrMalformedResponse}
	users.users = map[string]*User{"user-1": {Person: UserPerson{Email: "recovered@example.com", EmailVerified: true}}, "user-2": {Person: UserPerson{Email: "unverified@example.com"}}, "guest": {Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}}
	summary, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(2), summary.MeetingsUpdated)
	assert.Equal([]string{"user-1", "user-1", "user-2", "guest", "malformed", "new-user"}, users.retrieved)
}

func TestHydratorCachesSuccessfulAndMissingUsersAcrossServiceFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	users := &fakeUserSource{users: map[string]*User{"user-1": {Person: UserPerson{Email: "member@example.com", EmailVerified: true}}, "unverified": {}}, errs: map[string]error{"missing": &APIError{Kind: ErrProvider, Status: http.StatusNotFound}, "malformed": ErrMalformedResponse, "blocked": ErrUserInformation}}
	source := completeHydrationSource()
	h := NewHydrator(source).WithUserSource(users)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"missing", "malformed", "user-1", "unverified", "blocked", "new-user"}
	installMeetingHeader(t, source, meeting)
	for range 2 {
		result, err := h.Hydrate(t.Context(), meeting)
		require.NoError(err)
		require.Len(result.Attendees, 1)
		assert.Equal("member@example.com", result.Attendees[0].Email)
		assert.Equal(map[string]bool{"missing": true, "malformed": true, "blocked": true, "new-user": true}, result.failedAttendeeIDs)
		assert.Len(result.Warnings, 1)
	}
	assert.Zero(source.usersCalls)
	assert.Equal([]string{"missing", "malformed", "user-1", "unverified", "blocked"}, users.retrieved)
}

func TestHydratorOptionalLocalBudgetsDoNotPoisonCache(t *testing.T) {
	for _, budget := range []string{"requests", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := completeHydrationSource()
			var content hydrationSource = source
			users := &fakeUserSource{users: map[string]*User{"user-1": {Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}}}
			var userSource UserSource = users
			var large atomic.Bool
			large.Store(true)
			var userRequests atomic.Int32
			meeting := hydrationMeeting()
			meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1"}
			installMeetingHeader(t, source, meeting)
			if budget == "requests" {
				pages := map[string]*BlockPage{}
				for i := range maxHydrationRequests - 4 {
					cursor := ""
					if i > 0 {
						cursor = strconv.Itoa(i)
					}
					pages[cursor] = &BlockPage{HasMore: i < maxHydrationRequests-5, NextCursor: strconv.Itoa(i + 1)}
				}
				source.children["notes-1"] = pages
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var response map[string]any
					padding := 0
					switch r.URL.Path {
					case "/v1/blocks/meeting-1":
						raw, err := json.Marshal(meeting)
						assert.NoError(err)
						assert.NoError(json.Unmarshal(raw, &response))
					case "/v1/pages/page-1/markdown":
						response = map[string]any{"object": "page_markdown", "id": "page-1", "markdown": "# Transcript\nTest Speaker: Ready to ship.", "truncated": false, "unknown_block_ids": []string{}}
					case "/v1/users/user-1":
						userRequests.Add(1)
						padding = 2 << 20
						response = map[string]any{"object": "user", "id": "user-1", "type": "person", "person": map[string]any{"email": "guest@example.com", "email_verified": true}}
					default:
						id := strings.TrimPrefix(r.URL.Path, "/v1/blocks/")
						text := "Meeting notes."
						if large.Load() {
							padding = 12 << 20
						}
						if id == "transcript-1" {
							text = "Test Speaker: Ready to ship."
							if large.Load() {
								padding = 7 << 20
							}
						}
						response = map[string]any{"object": "block", "id": id, "type": "paragraph", "has_children": false, "paragraph": map[string]any{"rich_text": []map[string]string{{"plain_text": text}}}}
					}
					response["padding"] = strings.Repeat("x", padding)
					raw, err := json.Marshal(response)
					assert.NoError(err)
					assert.Less(int64(len(raw)), maxResponseSize)
					_, err = w.Write(raw)
					assert.NoError(err)
				}))
				t.Cleanup(server.Close)
				content = NewClient(server.URL, "meeting-example")
				userSource = NewClient(server.URL, "users-example")
			}
			h := NewHydrator(content).WithUserSource(userSource)
			first, err := h.Hydrate(t.Context(), meeting)
			require.NoError(err)
			assert.Equal("Test Speaker: Ready to ship.", first.Transcript)
			assert.True(first.failedAttendeeIDs["user-1"])
			assert.Empty(h.users)
			assert.False(h.usersUnavailable)
			source.children["notes-1"] = map[string]*BlockPage{"": {}}
			large.Store(false)
			if budget == "bytes" {
				assert.Equal(int32(1), userRequests.Load())
				assert.Greater(h.bytes, maxHydrationBytes)
			}
			second, err := h.Hydrate(t.Context(), meeting)
			require.NoError(err)
			require.Len(second.Attendees, 1)
			assert.Equal("guest@example.com", second.Attendees[0].Email)
			if budget == "bytes" {
				assert.Equal(int32(2), userRequests.Load())
			}
		})
	}
}

type usersRetryTransport struct {
	requested []string
	delay     func()
}

func (f *usersRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.requested = append(f.requested, r.URL.Path)
	if r.URL.Path != "/v1/users/timeout" {
		f.delay()
		id := strings.TrimPrefix(r.URL.Path, "/v1/users/")
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"object":"user","id":"` + id + `","person":{"email":"guest@example.com","email_verified":true}}`))}, nil
	}
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"object":"error","code":"service_unavailable"}`))}, nil
}

func TestHydratorOptionalDeadlineAndParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		client := NewClient("https://notion.example", "users-example")
		transport := &usersRetryTransport{delay: func() { time.Sleep(3 * time.Second) }}
		client.http.Transport = transport
		source := completeHydrationSource()
		h := NewHydrator(source).WithUserSource(client)
		meeting := hydrationMeeting()
		meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "user-2", "user-3"}
		installMeetingHeader(t, source, meeting)
		start := time.Now()
		first, err := h.Hydrate(t.Context(), meeting)
		require.NoError(err)
		require.Len(first.Attendees, 3)
		assert.Equal(9*time.Second, time.Since(start))
		meeting.MeetingNotes.CalendarEvent.Attendees = append(meeting.MeetingNotes.CalendarEvent.Attendees, "timeout")
		installMeetingHeader(t, source, meeting)
		start = time.Now()
		second, err := h.Hydrate(t.Context(), meeting)
		require.NoError(err)
		assert.Equal("Test Speaker: Ready to ship.", second.Transcript)
		require.Len(second.Attendees, 3)
		require.ErrorIs(h.usersFailure, context.DeadlineExceeded)
		assert.Equal(optionalLookupBudget, time.Since(start))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = h.Hydrate(ctx, hydrationMeeting())
		require.ErrorIs(err, context.Canceled)
	})
}

func TestImporterIncompleteDirectoryRespectsFreshUnverifiedAttendee(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := newImporterFixture(t)
	source.users[""].Results[1].Person.Email = "second@example.com"
	source.users[""].Results[1].Person.EmailVerified = true
	_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	source.users[""].Results = source.users[""].Results[:1]
	source.users[""].Results[0].Person.EmailVerified = false
	source.users[""].HasMore = true
	source.users[""].NextCursor = "failed-tail"
	source.userErrs = map[string]error{"failed-tail": ErrRateLimited}
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	recipients, err := st.GetMessageRecipientsContext(t.Context(), messageID, "to")
	require.NoError(err)
	require.Len(recipients, 1)
	assert.Equal("second@example.com", recipients[0].EmailAddress, "only the absent user's previously verified email survives the failed directory tail")
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.NotContains(string(raw), "attendee@example.com")
}

type slowOptionalUserSource struct{}

func (slowOptionalUserSource) RetrieveUser(ctx context.Context, id string) (*User, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(50 * time.Second):
		return &User{ID: id, Person: UserPerson{Email: id + "@example.com", EmailVerified: true}}, nil
	}
}

func TestImporterPublishesContentWhenOptionalLookupBudgetExpires(t *testing.T) {
	st, source, imp := newImporterFixture(t)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "user-2", "user-3"}
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	imp.WithUserSource(slowOptionalUserSource{})
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		result, err := imp.Import(ctx, ImportOptions{Identifier: "work"})
		require.NoError(err)
		assert.Equal(int64(1), result.MeetingsAdded)
		var messageID int64
		require.NoError(st.DB().QueryRowContext(ctx, `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
		body, err := st.GetMessageBodyText(ctx, messageID)
		require.NoError(err)
		assert.Contains(body, "Test Speaker: Ready to ship.")
		assert.NoError(ctx.Err())
	})
}

type slowOptionalDirectorySource struct{ *fakeHydrationSource }

func (s slowOptionalDirectorySource) ListUsers(ctx context.Context, _ string) (*UserPage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHydratorRetainsContentOnDirectoryBudgetExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		source := slowOptionalDirectorySource{completeHydrationSource()}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		result, err := NewHydrator(source).Hydrate(ctx, hydrationMeeting())
		require.NoError(err)
		assert.Equal("Test Speaker: Ready to ship.", result.Transcript)
		assert.True(result.AttendeeResolutionDegraded)
		assert.NoError(ctx.Err())
	})
}
