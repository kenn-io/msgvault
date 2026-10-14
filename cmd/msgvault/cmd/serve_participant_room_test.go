package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

type roomMaskingPeopleEngine struct {
	*querytest.MockEngine

	person *query.PersonSummary
}

func (e *roomMaskingPeopleEngine) SearchPeople(context.Context, query.PersonSearchRequest) (*query.PersonSearchResponse, error) {
	return &query.PersonSearchResponse{}, nil
}

func (e *roomMaskingPeopleEngine) GetPerson(context.Context, int64, query.Context, []int64) (*query.PersonSummary, error) {
	return e.person, nil
}

func (e *roomMaskingPeopleEngine) GetPersonSummary(context.Context, int64, query.ExploreRequest, []int64) (*query.PersonSearchResponse, error) {
	return &query.PersonSearchResponse{}, nil
}

func (e *roomMaskingPeopleEngine) SearchDomains(context.Context, query.DomainSearchRequest) (*query.DomainSearchResponse, error) {
	return &query.DomainSearchResponse{}, nil
}

func (e *roomMaskingPeopleEngine) GetDomain(context.Context, string, query.Context) (*query.DomainSummary, error) {
	return &query.DomainSummary{}, nil
}

func (e *roomMaskingPeopleEngine) GetDomainSummary(context.Context, string, query.ExploreRequest) (*query.DomainSearchResponse, error) {
	return &query.DomainSearchResponse{}, nil
}

func TestDaemonParticipantDetailHidesRoomIdentityHydration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	room, err := st.EnsureParticipant("room@resource.calendar.google.com", "Example Room", "")
	requirements.NoError(err)
	human, err := st.EnsureParticipant("human@example.test", "Example Human", "example.test")
	requirements.NoError(err)
	_, err = st.LinkParticipants(room, human)
	requirements.NoError(err)

	engine := &roomMaskingPeopleEngine{
		MockEngine: &querytest.MockEngine{},
		person: &query.PersonSummary{
			ID: room, DisplayLabel: "Example Human",
			Identifiers: []query.PersonIdentifier{
				{Type: "email", Value: "room@resource.calendar.google.com", ParticipantID: room},
				{Type: "email", Value: "human@example.test", ParticipantID: human},
			},
		},
	}
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/participants/%d", room), nil))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var person query.PersonSummary
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &person))
	requirements.NotNil(person.Cluster)
	requirements.Len(person.Cluster.Members, 2)
	for _, member := range person.Cluster.Members {
		if member.ParticipantID == room {
			assertions.Empty(member.Email)
			assertions.Empty(member.DisplayName)
		} else {
			assertions.Equal("human@example.test", member.Email)
			assertions.Equal("Example Human", member.DisplayName)
		}
	}
	requirements.Len(person.Identifiers, 1)
	assertions.Equal("human@example.test", person.Identifiers[0].Value)
	assertions.Equal(human, person.Identifiers[0].ParticipantID)
}

var _ query.PeopleAnalyzer = (*roomMaskingPeopleEngine)(nil)
