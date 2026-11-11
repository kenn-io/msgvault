//go:build sqlite_vec

package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type discoveryQueryEmbedder struct{}

func (discoveryQueryEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func TestPersonDiscoveryMCPRealArchive(t *testing.T) {
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	var organizations []*store.Organization
	for i := range 2 {
		org, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{Name: fmt.Sprintf("Company %d", i), Kind: store.OrganizationKindCompany})
		require.NoError(t, err)
		_, err = st.ReplaceOrganizationProfileContext(ctx, org.ID, org.Revision, store.OrganizationProfileInput{Names: []store.OrganizationNameInput{
			{Name: "Shared Coop", NameKind: store.OrganizationNameKindAlias, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
		}})
		require.NoError(t, err)
		organizations = append(organizations, org)
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	const fingerprint = "discovery-test"
	generation, err := backend.CreateGeneration(ctx, "test", 2, fingerprint)
	require.NoError(t, err)
	var ids []int64
	var embeddings []vector.PersonEmbedding
	for i := range 3 {
		participant, err := st.EnsureParticipantByIdentifier("email", fmt.Sprintf("person-%d@example.test", i), "Same Name")
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
		require.NoError(t, err)
		name := "Same Name"
		person, err = st.UpdatePersonDisplayNameContext(ctx, person.ID, person.Revision, &name)
		require.NoError(t, err)
		org := organizations[0]
		if i == 2 {
			org = organizations[1]
		}
		_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: org.ID, Title: new("Engineer"), Source: store.ProvenanceUser})
		require.NoError(t, err)
		doc, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
		require.NoError(t, err)
		ids = append(ids, person.ID)
		embeddings = append(embeddings, vector.PersonEmbedding{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, float32(i) / 10}})
	}
	require.NoError(t, backend.UpsertPersons(ctx, generation, embeddings))
	require.NoError(t, backend.ActivateGeneration(ctx, generation, true))
	search := personsearch.NewEngine(backend, st, discoveryQueryEmbedder{}, personsearch.Config{ExpectedFingerprint: fingerprint, Gate: vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil })})
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler), VectorStatus: api.VectorStatusReady, PersonSearchEngine: search})
	server := httptest.NewServer(daemon.Router())
	t.Cleanup(server.Close)
	engine, err := daemonclient.NewEngine(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	people := daemonclient.NewPeopleBrowser(engine)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PeopleBackend: people, DirectoryBackend: people, SemanticPeopleBackend: people, OrganizationBackend: people}
	call := func(name string, args map[string]any) map[string]any {
		return toolStructuredContent(t, rawCallTool(t, opts, name, args))
	}
	matches := call("list_organizations", map[string]any{"query": "Shared", "limit": 1})
	assert.InDelta(t, 2, matches["total"], 0)
	companies, ok := matches["organizations"].([]any)
	require.True(t, ok)
	require.Len(t, companies, 1)
	selectedID := int64(companies[0].(map[string]any)["id"].(float64))
	assert.Equal(t, organizations[0].ID, selectedID)
	first := rawCallTool(t, opts, "list_directory_people", map[string]any{"organization_id": selectedID, "sort": "name"})
	page := decodeDirectoryToolPage(t, first)
	require.Len(t, page.People, 2)
	assert.ElementsMatch(t, ids[:2], []int64{page.People[0].ID, page.People[1].ID})
	results := call("search_semantic_people", map[string]any{"query": "engineer", "organization_id": selectedID})
	rows, ok := results["results"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Contains(t, ids[:2], int64(row.(map[string]any)["person_id"].(float64)))
	}
}
