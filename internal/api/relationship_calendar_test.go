package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type relationshipCalendarAPIEngine struct {
	*querytest.MockEngine

	canonicalID int64
	resolveErr  error
	result      *query.RelationshipCalendarResponse
	calendarErr error
	request     query.RelationshipCalendarRequest
}

func (e *relationshipCalendarAPIEngine) ResolveCanonicalParticipant(
	_ context.Context, participantID int64,
) (int64, error) {
	if e.resolveErr != nil {
		return 0, e.resolveErr
	}
	if e.canonicalID != 0 {
		return e.canonicalID, nil
	}
	return participantID, nil
}

func (e *relationshipCalendarAPIEngine) RelationshipCalendar(
	_ context.Context, request query.RelationshipCalendarRequest,
) (*query.RelationshipCalendarResponse, error) {
	e.request = request
	return e.result, e.calendarErr
}

func TestRelationshipCalendarResolvesAliasAndReturnsTypedCalendar(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := &relationshipCalendarAPIEngine{
		MockEngine:  &querytest.MockEngine{},
		canonicalID: 7,
		result: &query.RelationshipCalendarResponse{
			CanonicalID: 7, Year: 2026, Timezone: "America/New_York",
			Days:            []query.RelationshipCalendarDay{{Date: "2026-01-01", Total: 3}},
			PeakTemperature: 97, PeakYear: 2018, CacheRevision: "cache-24",
		},
	}
	srv := newTestServerWithEngine(t, engine)

	response := postExploreJSON(t, srv, "/api/v1/relationships/42/calendar",
		`{"year":2026,"timezone":"America/New_York"}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body RelationshipCalendarHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(int64(42), body.ParticipantID)
	assert.Equal(int64(7), body.CanonicalID)
	assert.Equal(2026, body.Year)
	assert.Equal("America/New_York", body.Timezone)
	assert.Equal(97, body.PeakTemperature)
	assert.Equal(query.RelationshipCalendarRequest{
		CanonicalID: 7, Year: 2026, Timezone: "America/New_York",
	}, engine.request)
}

func TestRelationshipCalendarMatchesPublishedPeopleTemperatureEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	srv := newTestServerWithEngine(t, newRelationshipsDuckDBFixture(t, now))

	directoryResponse := postExploreJSON(t, srv, "/api/v1/participants/search",
		`{"predicate":{},"limit":25}`)
	require.Equal(http.StatusOK, directoryResponse.Code, directoryResponse.Body.String())
	var directory ParticipantSearchHTTPResponse
	require.NoError(json.Unmarshal(directoryResponse.Body.Bytes(), &directory))
	var person *query.PersonSummary
	for index := range directory.Rows {
		if directory.Rows[index].ID == relAliceID {
			person = &directory.Rows[index]
			break
		}
	}
	require.NotNil(person, "the published people cache includes the canonical relationship")
	require.Positive(person.CurrentRelationshipTemperature)

	calendarResponse := postExploreJSON(t, srv,
		"/api/v1/relationships/3/calendar",
		`{"year":2026,"timezone":"America/Chicago"}`)
	require.Equal(http.StatusOK, calendarResponse.Code, calendarResponse.Body.String())
	var calendar RelationshipCalendarHTTPResponse
	require.NoError(json.Unmarshal(calendarResponse.Body.Bytes(), &calendar))
	assert.Equal(relAlice2ID, calendar.ParticipantID)
	assert.Equal(relAliceID, calendar.CanonicalID, "an alias resolves before the cache point read")
	assert.Equal(person.CurrentRelationshipTemperature, calendar.Current.Temperature)
	assert.Equal(person.PeakRelationshipTemperature, calendar.PeakTemperature)
	assert.Equal(person.PeakRelationshipYear, calendar.PeakYear)
	assert.Equal(directory.CacheRevision, calendar.CacheRevision)
	assert.NotEmpty(calendar.Days)
	assert.Equal("2026-01-09", calendar.EffectiveDate,
		"the score is pegged to the latest committed relationship event")
}

func TestRelationshipCalendarReturnsStableInputAndPersonErrors(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       string
		calendar   error
		statusCode int
		code       string
	}{
		{name: "invalid participant", path: "/api/v1/relationships/0/calendar",
			body: `{}`, statusCode: http.StatusBadRequest, code: "invalid_participant_id"},
		{name: "invalid year", path: "/api/v1/relationships/7/calendar",
			body: `{"year":1969}`, calendar: query.ErrInvalidRelationshipYear,
			statusCode: http.StatusBadRequest, code: "invalid_year"},
		{name: "invalid timezone", path: "/api/v1/relationships/7/calendar",
			body:       `{"year":2026,"timezone":"Mars/Olympus_Mons"}`,
			calendar:   query.ErrInvalidRelationshipTimezone,
			statusCode: http.StatusBadRequest, code: "invalid_timezone"},
		{name: "missing person", path: "/api/v1/relationships/999/calendar",
			body: `{"year":2026}`, calendar: query.ErrRelationshipPersonNotFound,
			statusCode: http.StatusNotFound, code: "participant_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := &relationshipCalendarAPIEngine{
				MockEngine: &querytest.MockEngine{}, calendarErr: test.calendar,
			}
			response := postExploreJSON(t, newTestServerWithEngine(t, engine), test.path, test.body)
			require.Equal(t, test.statusCode, response.Code, response.Body.String())
			var apiErr ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &apiErr))
			assert.Equal(t, test.code, apiErr.Error)
		})
	}
}

func TestRelationshipCalendarReportsAnalyticsUnavailable(t *testing.T) {
	response := postExploreJSON(t,
		newTestServerWithEngine(t, &querytest.MockEngine{}),
		"/api/v1/relationships/7/calendar", `{"year":2026,"timezone":"UTC"}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
}

func TestRelationshipCalendarReportsCacheReadiness(t *testing.T) {
	engine := &relationshipCalendarAPIEngine{
		MockEngine: &querytest.MockEngine{},
		calendarErr: &query.CacheUnavailableError{
			Readiness: query.CacheStaleSchema,
		},
	}
	response := postExploreJSON(t, newTestServerWithEngine(t, engine),
		"/api/v1/relationships/7/calendar", `{"year":2026,"timezone":"UTC"}`)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ExploreCacheUnavailableResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, query.CacheStaleSchema, body.Readiness)
}

func TestRelationshipCalendarRequestRejectsTrailingJSON(t *testing.T) {
	engine := &relationshipCalendarAPIEngine{MockEngine: &querytest.MockEngine{}}
	response := postExploreJSON(t, newTestServerWithEngine(t, engine),
		"/api/v1/relationships/7/calendar", `{"year":2026} {}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}

func TestRelationshipCalendarOpenAPIContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.Equal("2.10.0", APISchemaVersion)
	document := OpenAPIDocument()
	path := document.Paths["/api/v1/relationships/{id}/calendar"]
	require.NotNil(path)
	require.NotNil(path.Post)
	assert.Equal("getRelationshipCalendar", path.Post.OperationID)
	require.Len(path.Post.Parameters, 1)
	assert.Equal("id", path.Post.Parameters[0].Name)
	assert.Equal("path", path.Post.Parameters[0].In)
	assert.True(path.Post.Parameters[0].Required)
	assert.Contains(path.Post.Responses, "200")
	assert.Contains(path.Post.Responses, "404")

	person := document.Components.Schemas.Map()["PersonSummary"]
	require.NotNil(person)
	for _, property := range []string{
		"current_relationship_temperature", "peak_relationship_temperature", "peak_relationship_year",
	} {
		assert.Contains(person.Properties, property)
	}
	day := document.Components.Schemas.Map()["RelationshipCalendarDay"]
	require.NotNil(day)
	require.NotNil(day.Properties["level"])
	assert.ElementsMatch([]any{
		"NONE", "FIRST_QUARTILE", "SECOND_QUARTILE", "THIRD_QUARTILE", "FOURTH_QUARTILE",
	}, day.Properties["level"].Enum)
}
