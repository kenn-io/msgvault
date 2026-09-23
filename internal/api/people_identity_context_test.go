package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEnrichIdentityContextInParticipantDetail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	root, err := st.EnsureParticipant("root@example.com", "Root Example", "example.com")
	require.NoError(err)
	alias, err := st.EnsureParticipant("alias@example.com", "Alias Example", "example.com")
	require.NoError(err)
	bare, err := st.EnsureParticipant("bare@example.com", "Bare Example", "example.com")
	require.NoError(err)

	var serviceID int64
	require.NoError(st.DB().QueryRowContext(ctx,
		st.Rebind(`SELECT id FROM communication_services WHERE slug = ?`), "whatsapp",
	).Scan(&serviceID))
	const key = "beeper:8:whatsapp:9:@user:x.y"
	require.NoError(st.SetParticipantIdentifier(alias, "beeper", key))
	require.NoError(st.ClassifyParticipantIdentifierServiceContext(
		ctx, "beeper", key, &serviceID, new("account"), new("local-whatsapp_ba_example")))
	_, err = st.LinkParticipants(root, alias)
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: root,
		RightKind: store.IdentityMatchParticipant, RightID: bare,
		Basis: store.IdentityMatchStableProviderID, Source: store.ProvenanceArchiveObservation,
		NormalizedValue: new(key), State: store.IdentityMatchStateCandidate,
	})
	require.NoError(err)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err)

	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{
		ID: root, DisplayLabel: "Root Example", Identifiers: []query.PersonIdentifier{{
			Type: "beeper", Value: key, DisplayValue: key, ParticipantID: alias,
			Provenance: "participant_identifiers",
		}},
	}}
	srv := newPeopleAPIServerWithStore(engine, st)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/participants/%d", root), nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body query.PersonSummary
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	require.Len(body.Identifiers, 1)
	assert.Equal("whatsapp", body.Identifiers[0].ServiceSlug)
	assert.Equal("WhatsApp", body.Identifiers[0].ServiceLabel)
	assert.Equal("account", body.Identifiers[0].ScopeKind)
	assert.Equal("local-whatsapp_ba_example", body.Identifiers[0].ScopeValue)
	assert.Equal("Alias Example", body.Identifiers[0].ParticipantDisplayName)
	require.NotNil(body.Cluster)
	require.Len(body.Cluster.Members, 3)
	assert.Equal(bare, body.Cluster.Members[2].ParticipantID)
	assert.Equal("Bare Example", body.Cluster.Members[2].DisplayName)
	assert.Equal("bare@example.com", body.Cluster.Members[2].Email)
	require.Len(body.Cluster.Edges, 2)
	origins := make(map[int64]string)
	for _, edge := range body.Cluster.Edges {
		require.NotNil(edge.LinkOrigin)
		other := edge.ParticipantA
		if other == root {
			other = edge.ParticipantB
		}
		origins[other] = edge.LinkOrigin.Kind
		if other == bare {
			assert.Equal("archive_observation", edge.LinkOrigin.Source)
			assert.Equal("stable_provider_id", edge.LinkOrigin.Basis)
		}
	}
	assert.Equal(map[int64]string{alias: "manual", bare: "candidate"}, origins)
}

func TestParticipantDetailContinuesWhenIdentityContextFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// Exercise an unavailable database through the real store read path.
	require.NoError(st.Close())
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{
		ID: 42, DisplayLabel: "Example Person", Identifiers: []query.PersonIdentifier{{
			Type: "email", Value: "person@example.com", ParticipantID: 42,
		}},
	}}
	srv := newPeopleAPIServerWithStore(engine, st)
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewTextHandler(&logs, nil))
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/participants/42", nil))

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body query.PersonSummary
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	assert.Equal("Example Person", body.DisplayLabel)
	assert.Equal([]query.PersonIdentifier{{
		Type: "email", Value: "person@example.com", ParticipantID: 42,
	}}, body.Identifiers)
	assert.Contains(logs.String(), "participant identity context lookup failed")
}
