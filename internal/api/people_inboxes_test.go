package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type participantInboxAPIEngine struct {
	*peopleAPIEngine

	canonicalID    int64
	resolvedID     int64
	inboxRequest   query.PersonInboxRequest
	inboxResult    *query.PersonInboxResponse
	inboxErr       error
	inboxCallCount int
}

func (e *participantInboxAPIEngine) ResolveCanonicalParticipant(_ context.Context, participantID int64) (int64, error) {
	e.resolvedID = participantID
	if e.inboxErr != nil {
		return 0, e.inboxErr
	}
	return e.canonicalID, nil
}

func (e *participantInboxAPIEngine) ListPersonInboxes(_ context.Context, request query.PersonInboxRequest) (*query.PersonInboxResponse, error) {
	e.inboxCallCount++
	e.inboxRequest = request
	return e.inboxResult, e.inboxErr
}

func TestParticipantInboxesResolveAliasAndReturnSourceRollups(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	aliasID := int64(22)
	canonicalID := int64(11)
	receivedAt := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	sentAt := time.Date(2026, 8, 19, 11, 0, 0, 0, time.UTC)
	engine := &participantInboxAPIEngine{
		peopleAPIEngine: &peopleAPIEngine{
			MockEngine: &querytest.MockEngine{},
			person:     &query.PersonSummary{ID: canonicalID, DisplayLabel: "Subject"},
		},
		canonicalID: canonicalID,
		inboxResult: &query.PersonInboxResponse{
			Rows: []query.PersonInboxRow{{
				SourceID: 7, SourceType: "beeper", SourceIdentifier: "whatsapp",
				ConversationCount: 2, ReceivedCount: 3, SentCount: 4,
				LatestReceivedAt: &receivedAt, LatestSentAt: &sentAt, LatestAt: sentAt,
			}},
			CacheRevision: "cache-inboxes", IdentityRevision: 9,
		},
	}

	response := httptest.NewRecorder()
	newTestServerWithEngine(t, engine).Router().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/v1/participants/22/inboxes", nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	assert.Equal(aliasID, engine.resolvedID)
	assert.Equal(query.PersonInboxRequest{CanonicalID: canonicalID}, engine.inboxRequest)

	var body query.PersonInboxResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	require.Len(body.Rows, 1)
	assert.Equal("cache-inboxes", body.CacheRevision)
	assert.Equal(int64(9), body.IdentityRevision)
	assert.Equal("beeper", body.Rows[0].SourceType)
	assert.Equal("whatsapp", body.Rows[0].SourceIdentifier)
	assert.Equal(int64(2), body.Rows[0].ConversationCount)
	assert.Equal(int64(3), body.Rows[0].ReceivedCount)
	assert.Equal(int64(4), body.Rows[0].SentCount)
	assert.Equal(&receivedAt, body.Rows[0].LatestReceivedAt)
	assert.Equal(&sentAt, body.Rows[0].LatestSentAt)
	assert.Equal(sentAt, body.Rows[0].LatestAt)
}

func TestParticipantInboxesValidationCapabilityAndMissingParticipant(t *testing.T) {
	t.Run("invalid ID", func(t *testing.T) {
		response := httptest.NewRecorder()
		newTestServerWithEngine(t, &peopleAPIEngine{MockEngine: &querytest.MockEngine{}}).Router().ServeHTTP(
			response, httptest.NewRequest(http.MethodGet, "/api/v1/participants/0/inboxes", nil))
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	})

	t.Run("inbox analyzer unavailable", func(t *testing.T) {
		response := httptest.NewRecorder()
		newTestServerWithEngine(t, &peopleAPIEngine{
			MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{ID: 7},
		}).Router().ServeHTTP(response, httptest.NewRequest(
			http.MethodGet, "/api/v1/participants/7/inboxes", nil))
		assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "analytical_cache_unavailable")
	})

	t.Run("participant detail absent", func(t *testing.T) {
		engine := &participantInboxAPIEngine{
			peopleAPIEngine: &peopleAPIEngine{MockEngine: &querytest.MockEngine{}},
			canonicalID:     7,
			inboxResult:     &query.PersonInboxResponse{Rows: []query.PersonInboxRow{}},
		}
		response := httptest.NewRecorder()
		newTestServerWithEngine(t, engine).Router().ServeHTTP(response, httptest.NewRequest(
			http.MethodGet, "/api/v1/participants/7/inboxes", nil))
		assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		assert.Zero(t, engine.inboxCallCount)
	})

	t.Run("empty inboxes are not missing participant", func(t *testing.T) {
		engine := &participantInboxAPIEngine{
			peopleAPIEngine: &peopleAPIEngine{
				MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{ID: 7},
			},
			canonicalID: 7,
			inboxResult: &query.PersonInboxResponse{
				Rows: []query.PersonInboxRow{}, CacheRevision: "cache-empty", IdentityRevision: 3,
			},
		}
		response := httptest.NewRecorder()
		newTestServerWithEngine(t, engine).Router().ServeHTTP(response, httptest.NewRequest(
			http.MethodGet, "/api/v1/participants/7/inboxes", nil))
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	})
}

func TestParticipantInboxesMapsCommittedCacheFailure(t *testing.T) {
	engine := &participantInboxAPIEngine{
		peopleAPIEngine: &peopleAPIEngine{
			MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{ID: 7},
		},
		canonicalID: 7,
		inboxErr:    &query.CacheUnavailableError{Readiness: query.CacheStaleSchema},
	}
	response := httptest.NewRecorder()
	newTestServerWithEngine(t, engine).Router().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/v1/participants/7/inboxes", nil))

	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	assert.Contains(t, response.Body.String(), "stale_schema")
}
