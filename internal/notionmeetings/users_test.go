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
	block := *source.blocks["meeting-1"]
	block.ID = meeting.ID
	source.blocks[meeting.ID] = &block
	source.query.Results = append(source.query.Results, meeting)

	users := &fakeUserSource{users: map[string]*User{"user-2": {Person: UserPerson{Email: "unverified@example.com"}}, "guest": {Person: UserPerson{Email: "guest@example.com", EmailVerified: true}}}, errs: map[string]error{"user-1": &APIError{Kind: ErrProvider, Status: http.StatusNotFound}, "malformed": ErrMalformedResponse, "new-user": ErrMalformedResponse}}
	source.query.Results[0].MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "user-2", "guest", "malformed"}
	second, err := imp.WithUserSource(users).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'meeting-1'`).Scan(&messageID))
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
			if budget == "requests" {
				pages := map[string]*BlockPage{}
				for i := range maxHydrationRequests - 5 {
					cursor := ""
					if i > 0 {
						cursor = strconv.Itoa(i)
					}
					pages[cursor] = &BlockPage{HasMore: i < maxHydrationRequests-6, NextCursor: strconv.Itoa(i + 1)}
				}
				source.children["notes-1"] = pages
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var response map[string]any
					padding := 0
					switch r.URL.Path {
					case "/v1/blocks/meeting-1":
						response = map[string]any{"object": "block", "id": "meeting-1", "type": "meeting_notes", "has_children": false, "meeting_notes": map[string]any{}}
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
		transport := &usersRetryTransport{delay: func() { time.Sleep(30 * time.Second) }}
		client.http.Transport = transport
		h := NewHydrator(completeHydrationSource()).WithUserSource(client)
		meeting := hydrationMeeting()
		meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1", "user-2", "user-3"}
		start := time.Now()
		first, err := h.Hydrate(t.Context(), meeting)
		require.NoError(err)
		require.Len(first.Attendees, 3)
		assert.Equal(90*time.Second, time.Since(start))
		meeting.MeetingNotes.CalendarEvent.Attendees = append(meeting.MeetingNotes.CalendarEvent.Attendees, "timeout")
		start = time.Now()
		second, err := h.Hydrate(t.Context(), meeting)
		require.NoError(err)
		assert.Equal("Test Speaker: Ready to ship.", second.Transcript)
		require.Len(second.Attendees, 3)
		require.ErrorIs(h.usersFailure, context.DeadlineExceeded)
		assert.Equal(UserLookupTimeout, time.Since(start))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = h.Hydrate(ctx, hydrationMeeting())
		require.ErrorIs(err, context.Canceled)
	})
}
