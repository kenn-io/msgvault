package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStructuredProfileHidesRoomOnlyAndMixedRoomPoints(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	server, st := newProfileTestServer(t)
	participant, err := st.EnsureParticipantByIdentifier("email", "room@resource.calendar.google.com", "Example Room")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
	requirements.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "room@resource.calendar.google.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	for _, suffix := range []string{"", "/history"} {
		response := doRequest(t, server, http.MethodGet, personProfilePath(person.ID)+suffix, nil, nil)
		assertions.Equal(http.StatusNotFound, response.Code, response.Body.String())
	}
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "human@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	response := doRequest(t, server, http.MethodGet, personProfilePath(person.ID), nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var profile store.PersonProfile
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
	requirements.Len(profile.ContactPoints, 1)
	assertions.Equal("human@example.org", profile.ContactPoints[0].NormalizedValue)
	raw, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Len(raw.Profile.ContactPoints, 2)
}

// The analytical result is already sanitized. This exercises the live Store
// hydration appended by the real HTTP handler to that result.
func TestPersonDetailIdentityHydrationDoesNotRestoreRoomFields(t *testing.T) {
	for _, tc := range []struct {
		name, email string
		curated     bool
	}{
		{name: "observed room", email: "room@resource.calendar.google.com"},
		{name: "curated room", curated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			room, err := st.EnsureParticipant(tc.email, "Example Room", "")
			requirements.NoError(err)
			if tc.curated {
				profile, _, err := st.CreatePersonFromParticipantContext(t.Context(), room)
				requirements.NoError(err)
				_, err = st.AddPersonContactPointContext(t.Context(), profile.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "room@resource.calendar.google.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				requirements.NoError(err)
			}
			human, err := st.EnsureParticipant("human@example.org", "Example Human", "example.org")
			requirements.NoError(err)
			_, err = st.LinkParticipants(room, human)
			requirements.NoError(err)
			engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{ID: room, DisplayLabel: "Example Human", Identifiers: []query.PersonIdentifier{{Type: "email", Value: "human@example.org", ParticipantID: human}}}}
			server := newPeopleAPIServerWithStore(engine, st)
			response := doRequest(t, server.Router(), http.MethodGet, fmt.Sprintf("/api/v1/participants/%d", room), nil, nil)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var person query.PersonSummary
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &person))
			requirements.NotNil(person.Cluster)
			assertions.ElementsMatch([]int64{room, human}, person.Cluster.MemberIDs)
			requirements.Len(person.Cluster.Edges, 1)
			requirements.Len(person.Cluster.Members, 2)
			for _, member := range person.Cluster.Members {
				if member.ParticipantID == room {
					assertions.Empty(member.Email)
					assertions.Empty(member.DisplayName)
				} else {
					assertions.Equal("human@example.org", member.Email)
					assertions.Equal("Example Human", member.DisplayName)
				}
			}
			raw, err := st.GetParticipantIdentityContext(t.Context(), []int64{room, human})
			requirements.NoError(err)
			for _, member := range raw.Members {
				if member.ParticipantID == room {
					assertions.Equal("Example Room", member.DisplayName, "raw context preserves the stored name")
				}
			}
		})
	}
}
