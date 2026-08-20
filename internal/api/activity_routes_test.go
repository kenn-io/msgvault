package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const (
	activityTestAPIKey          = "activity-test-key"
	activityTestJSONMediaType   = "application/json"
	activityTestDefaultResponse = "default"
)

type activityRouteFixture struct {
	server   *Server
	store    *store.Store
	fixture  *storetest.Fixture
	personID int64
	ownerID  int64
	targetID int64
	date     string
	noteID   int64
}

func newActivityRouteFixture(t *testing.T) activityRouteFixture {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(
		f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	counterpart := f.EnsureParticipant(
		"counterpart@example.com", "Counterpart", "example.com")
	person, created, err := f.Store.CreatePersonFromParticipant(counterpart)
	require.NoError(t, err)
	require.True(t, created)

	occurredAt := time.Date(2026, 7, 30, 9, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	messageID := f.NewMessage().
		WithSourceMessageID("activity-api-message").
		WithSentAt(occurredAt).
		Create(t, f.Store)
	require.NoError(t, f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{counterpart}, []string{"Counterpart"}))
	require.NoError(t, f.Store.ReplaceMessageRecipients(
		messageID, "to", []int64{owner}, []string{"Owner"}))
	projector, err := activity.NewProjector(f.Store, activity.Options{
		Timezone: "America/New_York", BatchSize: 2, MaxDirectCounterparts: 25,
	})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)

	days := int64(2)
	_, err = f.Store.SetPersonAttributeValueContext(
		t.Context(), store.PersonAttributeValueInput{
			PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency,
			Value: store.AttributeValue{
				Type: store.AttributeValueInteger, Integer: &days,
			},
			Source: store.ProvenanceUser,
		})
	require.NoError(t, err)
	note, err := f.Store.CreateDailyNoteEntryContext(
		t.Context(), store.DailyNoteEntryInput{
			LocalDate: "2026-07-30", Body: "Synthetic route note",
			Author: "Test User", PersonIDs: []int64{person.ID},
		})
	require.NoError(t, err)

	srv := NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: activityTestAPIKey}},
		f.Store, nil, testLogger())
	srv.clock = func() time.Time {
		return time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	}
	return activityRouteFixture{
		server: srv, store: f.Store, fixture: f, personID: person.ID,
		ownerID: owner, targetID: counterpart,
		date: "2026-07-30", noteID: note.ID,
	}
}

func activityRequest(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	body []byte,
	authenticated bool,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if authenticated {
		req.Header.Set("X-Api-Key", activityTestAPIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", activityTestJSONMediaType)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	return response
}

func TestActivityHTTPRealProjectionAndIntersectionContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newActivityRouteFixture(t)

	stateResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/contact-state", f.personID), nil, true)
	require.Equal(http.StatusOK, stateResponse.Code, stateResponse.Body.String())
	var state store.ContactState
	require.NoError(json.Unmarshal(stateResponse.Body.Bytes(), &state))
	assert.True(strings.HasPrefix(state.LastContactRef, "message:"))
	assert.Equal(store.ChannelEmail, state.InferredChannel)
	assert.Equal(store.CadenceOverdue, state.CadenceStatus)
	assert.NotContains(stateResponse.Body.String(), "primary_channel")

	daysResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/days?from=%s&to=%s&limit=1&offset=0",
			f.personID, f.date, f.date), nil, true)
	require.Equal(http.StatusOK, daysResponse.Code, daysResponse.Body.String())
	var days store.PersonDaysPage
	require.NoError(json.Unmarshal(daysResponse.Body.Bytes(), &days))
	assert.Equal(int64(1), days.TotalCount)
	require.Len(days.Days, 1)
	assert.Equal(int64(1), days.Days[0].EventCount)
	assert.Equal(int64(1), days.Days[0].EntryCount)

	personDayResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/days/%s?limit=1&offset=0&entry_limit=1&entry_offset=0",
			f.personID, f.date), nil, true)
	require.Equal(http.StatusOK, personDayResponse.Code, personDayResponse.Body.String())
	var personDay store.PersonDayPage
	require.NoError(json.Unmarshal(personDayResponse.Body.Bytes(), &personDay))
	assert.Equal(int64(1), personDay.ActivityTotalCount)
	assert.Equal(int64(1), personDay.EntryTotalCount)
	require.Len(personDay.Activity, 1)
	require.Len(personDay.Entries, 1)

	dayResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/days/%s?limit=1&offset=0&entry_limit=1&entry_offset=0&activity_limit_per_person=1",
			f.date), nil, true)
	require.Equal(http.StatusOK, dayResponse.Code, dayResponse.Body.String())
	var day store.DayPage
	require.NoError(json.Unmarshal(dayResponse.Body.Bytes(), &day))
	assert.Equal(int64(1), day.PersonTotalCount)
	assert.Equal(int64(1), day.EntryTotalCount)
	require.Len(day.Persons, 1)
	require.Len(day.Persons[0].Activity, 1)
	assert.False(day.Persons[0].ActivityTruncated)
	assert.Equal(personDay.Activity[0], day.Persons[0].Activity[0])
	assert.Equal("sent_at", day.Persons[0].Activity[0].DateOrigin)
	assert.Equal("timestamp", day.Persons[0].Activity[0].DatePrecision)
	assert.Equal("America/New_York", day.Persons[0].Activity[0].Timezone)
	assert.Equal(-240, day.Persons[0].Activity[0].UTCOffsetMinutes)
}

func TestDayEntriesHTTPCreateListDeleteAndValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newActivityRouteFixture(t)

	listed := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/days/%s/entries?limit=1&offset=0", f.date), nil, true)
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	var listResponse struct {
		Entries []store.DailyNoteEntry `json:"entries"`
	}
	require.NoError(json.Unmarshal(listed.Body.Bytes(), &listResponse))
	require.Len(listResponse.Entries, 1)
	assert.Equal(f.noteID, listResponse.Entries[0].ID)

	created := activityRequest(t, f.server, http.MethodPost,
		fmt.Sprintf("/api/v1/days/%s/entries", f.date),
		fmt.Appendf(nil, `{"body":"Second synthetic note","author":"Test User","person_ids":[%d,%d]}`,
			f.personID, f.personID), true)
	require.Equal(http.StatusCreated, created.Code, created.Body.String())
	var entry store.DailyNoteEntry
	require.NoError(json.Unmarshal(created.Body.Bytes(), &entry))
	assert.Equal(int64(2), entry.Ordinal)
	assert.Equal([]int64{f.personID}, entry.PersonIDs)

	deleted := activityRequest(t, f.server, http.MethodDelete,
		fmt.Sprintf("/api/v1/days/entries/%d", entry.ID), nil, true)
	assert.Equal(http.StatusNoContent, deleted.Code, deleted.Body.String())
	missing := activityRequest(t, f.server, http.MethodDelete,
		fmt.Sprintf("/api/v1/days/entries/%d", entry.ID), nil, true)
	assert.Equal(http.StatusNotFound, missing.Code, missing.Body.String())

	tests := []struct {
		name        string
		contentType string
		body        []byte
		unknownSize bool
		want        int
		wantMessage string
	}{
		{"malformed", activityTestJSONMediaType, []byte(`{"body":`), false, http.StatusBadRequest, ""},
		{"trailing", activityTestJSONMediaType, []byte(`{"body":"ok","person_ids":[]} {}`), false, http.StatusBadRequest, ""},
		{"unknown field", activityTestJSONMediaType, []byte(`{"body":"ok","extra":true}`), false, http.StatusBadRequest, ""},
		{"non object", activityTestJSONMediaType, []byte(`[]`), false, http.StatusBadRequest, ""},
		{"null", activityTestJSONMediaType, []byte(`null`), false, http.StatusBadRequest, "invalid daily entry request"},
		{"non positive target", activityTestJSONMediaType, []byte(`{"body":"ok","person_ids":[0]}`), false, http.StatusBadRequest, ""},
		{"unknown target", activityTestJSONMediaType, []byte(`{"body":"ok","person_ids":[999999]}`), false, http.StatusNotFound, ""},
		{"wrong media", "text/plain", []byte(`{"body":"ok"}`), false, http.StatusUnsupportedMediaType, ""},
		{"oversized", activityTestJSONMediaType, bytes.Repeat([]byte("x"), 1<<20+1), false, http.StatusRequestEntityTooLarge, ""},
		{"chunked oversized", activityTestJSONMediaType, bytes.Repeat([]byte("x"), 1<<20+1), true, http.StatusRequestEntityTooLarge, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost,
				fmt.Sprintf("/api/v1/days/%s/entries", f.date), bytes.NewReader(test.body))
			req.Header.Set("X-Api-Key", activityTestAPIKey)
			req.Header.Set("Content-Type", test.contentType)
			if test.unknownSize {
				req.ContentLength = -1
			}
			response := httptest.NewRecorder()
			f.server.Router().ServeHTTP(response, req)
			assert.Equal(test.want, response.Code, response.Body.String())
			if test.wantMessage != "" {
				assert.Contains(response.Body.String(), test.wantMessage)
			}
		})
	}
}

func TestActivityHTTPIndependentPagingPreviewAndNoteOnlyTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newActivityRouteFixture(t)
	secondID := f.fixture.NewMessage().
		WithSourceMessageID("activity-api-message-2").
		WithSentAt(time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)).
		Create(t, f.store)
	require.NoError(f.store.ReplaceMessageRecipients(
		secondID, "from", []int64{f.targetID}, []string{"Counterpart"}))
	require.NoError(f.store.ReplaceMessageRecipients(
		secondID, "to", []int64{f.ownerID}, []string{"Owner"}))
	projector, err := activity.NewProjector(f.store, activity.Options{
		Timezone: "America/New_York", BatchSize: 1, MaxDirectCounterparts: 25,
	})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	_, err = f.store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: f.date, Body: "Another synthetic route note",
		Author: "Test User", PersonIDs: []int64{f.personID},
	})
	require.NoError(err)

	dayResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/days/%s?activity_limit_per_person=1&entry_limit=1&entry_offset=1",
			f.date), nil, true)
	require.Equal(http.StatusOK, dayResponse.Code, dayResponse.Body.String())
	var day store.DayPage
	require.NoError(json.Unmarshal(dayResponse.Body.Bytes(), &day))
	require.Len(day.Persons, 1)
	assert.Equal(int64(2), day.Persons[0].EventCount)
	assert.True(day.Persons[0].ActivityTruncated)
	require.Len(day.Persons[0].Activity, 1)
	require.Len(day.Entries, 1)
	assert.Equal(int64(2), day.EntryTotalCount)
	assert.Equal(int64(2), day.Entries[0].Ordinal)

	personDayResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/days/%s?limit=1&offset=1&entry_limit=1&entry_offset=0",
			f.personID, f.date), nil, true)
	require.Equal(http.StatusOK, personDayResponse.Code, personDayResponse.Body.String())
	var personDay store.PersonDayPage
	require.NoError(json.Unmarshal(personDayResponse.Body.Bytes(), &personDay))
	assert.Equal(int64(2), personDay.ActivityTotalCount)
	assert.Equal(int64(2), personDay.EntryTotalCount)
	require.Len(personDay.Activity, 1)
	require.Len(personDay.Entries, 1)
	assert.NotEqual(day.Persons[0].Activity[0].Ref, personDay.Activity[0].Ref)

	noteOnlyParticipant := f.fixture.EnsureParticipant(
		"note-only@example.com", "Note Only", "example.com")
	noteOnly, created, err := f.store.CreatePersonFromParticipant(noteOnlyParticipant)
	require.NoError(err)
	require.True(created)
	_, err = f.store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: f.date, Body: "Note-only target",
		Author: "Test User", PersonIDs: []int64{noteOnly.ID},
	})
	require.NoError(err)
	noteOnlyDay := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/days/%s", noteOnly.ID, f.date), nil, true)
	require.Equal(http.StatusOK, noteOnlyDay.Code, noteOnlyDay.Body.String())
	var noteOnlyPage store.PersonDayPage
	require.NoError(json.Unmarshal(noteOnlyDay.Body.Bytes(), &noteOnlyPage))
	assert.Empty(noteOnlyPage.Activity)
	assert.NotNil(noteOnlyPage.Activity)
	require.Len(noteOnlyPage.Entries, 1)
}

type failingActivityStore struct {
	*store.Store

	err error
}

func (s *failingActivityStore) ContactStateContext(
	context.Context, int64, time.Time,
) (store.ContactState, error) {
	return store.ContactState{}, s.err
}

func TestActivityHTTPUnavailableAndRedactedStoreErrors(t *testing.T) {
	assert := assert.New(t)
	unavailable := NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: activityTestAPIKey}},
		&mockStore{}, nil, testLogger())
	response := activityRequest(t, unavailable, http.MethodGet,
		"/api/v1/people/1/contact-state", nil, true)
	assert.Equal(http.StatusServiceUnavailable, response.Code)

	f := newActivityRouteFixture(t)
	const sensitive = "driver failed for private@example.com id=991"
	failing := NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: activityTestAPIKey}},
		&failingActivityStore{Store: f.store, err: errors.New(sensitive)},
		nil, testLogger())
	response = activityRequest(t, failing, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/contact-state", f.personID), nil, true)
	assert.Equal(http.StatusInternalServerError, response.Code)
	assert.NotContains(response.Body.String(), sensitive)
	assert.NotContains(response.Body.String(), "private@example.com")

	canceled := NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: activityTestAPIKey}},
		&failingActivityStore{Store: f.store, err: context.Canceled},
		nil, testLogger())
	response = activityRequest(t, canceled, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/contact-state", f.personID), nil, true)
	assert.Equal(http.StatusServiceUnavailable, response.Code)
	assert.Contains(response.Body.String(), "query_canceled")

	unknown := activityRequest(t, f.server, http.MethodGet,
		"/api/v1/people/999999/contact-state", nil, true)
	assert.Equal(http.StatusNotFound, unknown.Code)
}

func TestActivityHTTPMissingContactStateAndBeyondTailPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newActivityRouteFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_contact_state WHERE person_id = ?`), f.personID)
	require.NoError(err)

	stateResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/contact-state", f.personID), nil, true)
	require.Equal(http.StatusOK, stateResponse.Code, stateResponse.Body.String())
	var state store.ContactState
	require.NoError(json.Unmarshal(stateResponse.Body.Bytes(), &state))
	assert.True(state.Stale)
	assert.Zero(state.InteractionCount)
	assert.Equal(store.ChannelEmail, state.InferredChannel)
	assert.Equal(store.CadenceUnknown, state.CadenceStatus)

	daysResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/days?offset=99", f.personID), nil, true)
	require.Equal(http.StatusOK, daysResponse.Code, daysResponse.Body.String())
	var days store.PersonDaysPage
	require.NoError(json.Unmarshal(daysResponse.Body.Bytes(), &days))
	assert.Equal(int64(1), days.TotalCount)
	assert.NotNil(days.Days)
	assert.Empty(days.Days)

	personDayResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/days/%s?offset=99&entry_offset=99",
			f.personID, f.date), nil, true)
	require.Equal(http.StatusOK, personDayResponse.Code, personDayResponse.Body.String())
	var personDay store.PersonDayPage
	require.NoError(json.Unmarshal(personDayResponse.Body.Bytes(), &personDay))
	assert.Equal(int64(1), personDay.ActivityTotalCount)
	assert.Equal(int64(1), personDay.EntryTotalCount)
	assert.NotNil(personDay.Activity)
	assert.NotNil(personDay.Entries)
	assert.Empty(personDay.Activity)
	assert.Empty(personDay.Entries)

	dayResponse := activityRequest(t, f.server, http.MethodGet,
		fmt.Sprintf("/api/v1/days/%s?offset=99&entry_offset=99", f.date), nil, true)
	require.Equal(http.StatusOK, dayResponse.Code, dayResponse.Body.String())
	var day store.DayPage
	require.NoError(json.Unmarshal(dayResponse.Body.Bytes(), &day))
	assert.Equal(int64(1), day.PersonTotalCount)
	assert.Equal(int64(1), day.EntryTotalCount)
	assert.NotNil(day.Persons)
	assert.NotNil(day.Entries)
	assert.Empty(day.Persons)
	assert.Empty(day.Entries)
}

func TestActivityHTTPInvalidAPIKeyAndSessionCSRF(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newActivityRouteFixture(t)

	for _, test := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/days/" + f.date},
		{http.MethodDelete, fmt.Sprintf("/api/v1/days/entries/%d", f.noteID)},
	} {
		req := httptest.NewRequest(test.method, test.path, nil)
		req.Header.Set("X-Api-Key", "invalid-activity-key")
		response := httptest.NewRecorder()
		f.server.Router().ServeHTTP(response, req)
		assert.Equal(http.StatusUnauthorized, response.Code, test.path)
	}

	login := performSessionRequest(t, f.server, http.MethodPost, sessionLoginPath,
		[]byte(`{"api_key":"`+activityTestAPIKey+`"}`), nil, false)
	require.Equal(http.StatusOK, login.Code, login.Body.String())
	session := decodeSessionStatus(t, login)
	cookie := requireSessionCookie(t, login)

	readHeaders := make(http.Header)
	readHeaders.Set("Cookie", cookie.String())
	read := performSessionRequest(t, f.server, http.MethodGet,
		"/api/v1/days/"+f.date, nil, readHeaders, false)
	assert.Equal(http.StatusOK, read.Code, read.Body.String())

	missingCSRF := performSessionRequest(t, f.server, http.MethodDelete,
		fmt.Sprintf("/api/v1/days/entries/%d", f.noteID), nil, readHeaders, false)
	assert.Equal(http.StatusForbidden, missingCSRF.Code, missingCSRF.Body.String())

	mutationHeaders := readHeaders.Clone()
	mutationHeaders.Set("Origin", "http://example.com")
	mutationHeaders.Set(csrfHeaderName, session.CSRFToken)
	deleted := performSessionRequest(t, f.server, http.MethodDelete,
		fmt.Sprintf("/api/v1/days/entries/%d", f.noteID), nil, mutationHeaders, false)
	assert.Equal(http.StatusNoContent, deleted.Code, deleted.Body.String())
}

func TestActivityHTTPRejectsInvalidParametersAndAuthentication(t *testing.T) {
	f := newActivityRouteFixture(t)
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/people/0/contact-state"},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?limit=0", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?limit=501", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?offset=-1", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?from=2026-7-30", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?from=2026-07-31&to=2026-07-30", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?from=2026-07-30&from=garbage", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days?to=2026-07-30&to=garbage", f.personID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days/%s?entry_limit=0", f.personID, f.date)},
		{http.MethodGet, fmt.Sprintf("/api/v1/people/%d/days/%s?entry_offset=-1", f.personID, f.date)},
		{http.MethodGet, fmt.Sprintf("/api/v1/days/%s?activity_limit_per_person=501", f.date)},
		{http.MethodGet, fmt.Sprintf("/api/v1/days/%s?entry_limit=nope", f.date)},
		{http.MethodGet, "/api/v1/days/2026-02-29"},
		{http.MethodDelete, "/api/v1/days/entries/0"},
	}
	for _, test := range paths {
		response := activityRequest(t, f.server, test.method, test.path, nil, true)
		assert.Equal(t, http.StatusBadRequest, response.Code,
			test.path+": "+response.Body.String())
	}

	read := activityRequest(t, f.server, http.MethodGet,
		"/api/v1/days/"+f.date, nil, false)
	assert.Equal(t, http.StatusUnauthorized, read.Code)
	mutation := activityRequest(t, f.server, http.MethodDelete,
		fmt.Sprintf("/api/v1/days/entries/%d", f.noteID), nil, false)
	assert.Equal(t, http.StatusUnauthorized, mutation.Code)
}

func TestActivityOpenAPIContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := OpenAPIDocument()
	assertions.Equal(APISchemaVersion, doc.Info.Version)

	tests := []struct {
		path, method, operationID string
		parameters                []string
		responses                 []string
		successStatus             string
		successSchema             string
	}{
		{
			"/api/v1/people/{id}/contact-state", http.MethodGet,
			"getPersonContactState", []string{"id"},
			[]string{"200", "400", "401", "403", "404", "500", "503", activityTestDefaultResponse},
			"200", "ContactState",
		},
		{
			"/api/v1/people/{id}/days", http.MethodGet,
			"listPersonActivityDays",
			[]string{"id", "from", "to", "limit", "offset"},
			[]string{"200", "400", "401", "403", "404", "500", "503", activityTestDefaultResponse},
			"200", "PersonDaysPage",
		},
		{
			"/api/v1/people/{id}/days/{date}", http.MethodGet,
			"getPersonActivityDay",
			[]string{"id", "date", "limit", "offset", "entry_limit", "entry_offset"},
			[]string{"200", "400", "401", "403", "404", "500", "503", activityTestDefaultResponse},
			"200", "PersonDayPage",
		},
		{
			"/api/v1/days/{date}", http.MethodGet, "getActivityDay",
			[]string{
				"date", "limit", "offset", "entry_limit", "entry_offset",
				"activity_limit_per_person",
			},
			[]string{"200", "400", "401", "403", "500", "503", activityTestDefaultResponse},
			"200", "DayPage",
		},
		{
			"/api/v1/days/{date}/entries", http.MethodGet, "listDayEntries",
			[]string{"date", "limit", "offset"},
			[]string{"200", "400", "401", "403", "500", "503", activityTestDefaultResponse},
			"200", "DailyNoteEntriesResponse",
		},
		{
			"/api/v1/days/{date}/entries", http.MethodPost, "createDayEntry",
			[]string{"date"},
			[]string{
				"201", "400", "401", "403", "404", "413", "415", "500", "503",
				activityTestDefaultResponse,
			},
			"201", "DailyNoteEntry",
		},
		{
			"/api/v1/days/entries/{id}", http.MethodDelete, "deleteDayEntry",
			[]string{"id"},
			[]string{"204", "400", "401", "403", "404", "500", "503", activityTestDefaultResponse},
			"204", "",
		},
	}
	for _, test := range tests {
		t.Run(test.operationID, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			item := doc.Paths[test.path]
			require.NotNil(item)
			var operation *huma.Operation
			switch test.method {
			case http.MethodGet:
				operation = item.Get
			case http.MethodPost:
				operation = item.Post
			case http.MethodDelete:
				operation = item.Delete
			}
			require.NotNil(operation)
			assert.Equal(test.operationID, operation.OperationID)
			assert.Equal([]map[string][]string{{apiKeySecurityScheme: {}}},
				operation.Security)

			actualParameters := make([]string, 0, len(operation.Parameters))
			for _, parameter := range operation.Parameters {
				actualParameters = append(actualParameters, parameter.Name)
				switch parameter.Name {
				case "id":
					assertActivityOpenAPIParam(t, operation, parameter.Name,
						"path", nil, 1, 0)
				case "date":
					assertActivityOpenAPIParam(t, operation, parameter.Name,
						"path", nil, 0, 0)
				case "from", "to":
					assertActivityOpenAPIParam(t, operation, parameter.Name,
						"query", nil, 0, 0)
				case "limit", "entry_limit", "activity_limit_per_person":
					assertActivityOpenAPIParam(t, operation, parameter.Name,
						"query", store.ActivityDefaultLimit, 1, store.ActivityMaxLimit)
				case "offset", "entry_offset":
					assertActivityOpenAPIParam(t, operation, parameter.Name,
						"query", 0, 0, 0)
				}
			}
			assert.Equal(test.parameters, actualParameters)

			actualResponses := make([]string, 0, len(operation.Responses))
			for status := range operation.Responses {
				actualResponses = append(actualResponses, status)
			}
			slices.Sort(actualResponses)
			expectedResponses := slices.Clone(test.responses)
			slices.Sort(expectedResponses)
			assert.Equal(expectedResponses, actualResponses)

			success := operation.Responses[test.successStatus]
			require.NotNil(success)
			if test.successSchema != "" {
				media := success.Content[activityTestJSONMediaType]
				require.NotNil(media)
				require.NotNil(media.Schema)
				assert.Equal("#/components/schemas/"+test.successSchema, media.Schema.Ref)
			} else {
				assert.Empty(success.Content)
			}
			for _, status := range test.responses {
				if status == test.successStatus || status == activityTestDefaultResponse {
					continue
				}
				response := operation.Responses[status]
				require.NotNil(response)
				media := response.Content[activityTestJSONMediaType]
				require.NotNil(media, status)
				require.NotNil(media.Schema, status)
				assert.Equal("#/components/schemas/ErrorResponse", media.Schema.Ref, status)
			}
		})
	}

	create := doc.Paths["/api/v1/days/{date}/entries"].Post
	requirements.NotNil(create.RequestBody)
	requirements.Contains(create.RequestBody.Content, activityTestJSONMediaType)

	for name, schemaDoc := range map[string]*huma.OpenAPI{
		"public": doc,
		"client": openAPIClientDocument(),
	} {
		requestSchema := schemaDoc.Components.Schemas.Map()["CreateDailyNoteEntryRequest"]
		requirements.NotNil(requestSchema, name)
		personIDs := requestSchema.Properties["person_ids"]
		requirements.NotNil(personIDs, name)
		requirements.NotNil(personIDs.Items, name)
		requirements.NotNil(personIDs.Items.Minimum, name)
		assertions.InDelta(float64(1), *personIDs.Items.Minimum, 0, name)
	}

	for schemaName, arrayFields := range map[string][]string{
		"PersonDaysPage":           {"days"},
		"PersonDayPage":            {"activity", "entries"},
		"DayPage":                  {"persons", "entries"},
		"DayPerson":                {"activity"},
		"DailyNoteEntriesResponse": {"entries"},
	} {
		schema := doc.Components.Schemas.Map()[schemaName]
		requirements.NotNil(schema, schemaName)
		for _, field := range arrayFields {
			property := schema.Properties[field]
			requirements.NotNil(property, schemaName+"."+field)
			assertions.False(property.Nullable, schemaName+"."+field)
		}
	}
}

func assertActivityOpenAPIParam(
	t *testing.T,
	operation *huma.Operation,
	name, location string,
	defaultValue any,
	minimum, maximum float64,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	require.NotNil(operation)
	for _, parameter := range operation.Parameters {
		if parameter.Name != name {
			continue
		}
		assert.Equal(location, parameter.In)
		require.NotNil(parameter.Schema)
		assert.Equal(defaultValue, parameter.Schema.Default)
		if minimum != 0 || name == "offset" || name == "entry_offset" {
			require.NotNil(parameter.Schema.Minimum)
			assert.InDelta(minimum, *parameter.Schema.Minimum, 0)
		}
		if maximum != 0 {
			require.NotNil(parameter.Schema.Maximum)
			assert.InDelta(maximum, *parameter.Schema.Maximum, 0)
		}
		if name == "from" || name == "to" || name == "date" {
			assert.Equal(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`, parameter.Schema.Pattern)
		}
		return
	}
	require.Fail("missing OpenAPI parameter", name)
}
