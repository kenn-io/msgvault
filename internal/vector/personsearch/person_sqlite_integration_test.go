//go:build sqlite_vec

package personsearch_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// The real backend owns candidate ranking and exhaustion. This synthetic Store
// supplies only the optional current-revision/membership contract, avoiding an
// unrelated archive fixture while exercising production Engine + vec0 together.
type sqliteScopeStore struct {
	revisions map[int64]string
	members   map[int64]map[int64]bool
}

func (s *sqliteScopeStore) ResolvePersonSemanticCandidatesContext(context.Context, []store.PersonSemanticCandidate) ([]store.Person, error) {
	panic("organization search must always use the scoped resolver")
}

func (s *sqliteScopeStore) ResolvePersonSemanticCandidatesInOrganizationContext(ctx context.Context, candidates []store.PersonSemanticCandidate, organizationID int64) ([]store.Person, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var people []store.Person
	for _, candidate := range candidates {
		if s.members[organizationID][candidate.PersonID] && s.revisions[candidate.PersonID] == candidate.Revision {
			people = append(people, store.Person{ID: candidate.PersonID, VCardUID: fmt.Sprintf("synthetic-person-%d", candidate.PersonID)})
		}
	}
	return people, nil
}

type sqliteScopeEmbedder struct{ calls int }

func (e *sqliteScopeEmbedder) EmbedQuery(ctx context.Context, _ string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.calls++
	return []float32{1, 0, 0, 0}, nil
}

func TestEngineOrganizationScopeRealSQLiteBeyondVec0Ceiling(t *testing.T) {
	must := require.New(t)

	ctx := t.Context()
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 4})
	must.NoError(err)
	t.Cleanup(func() { must.NoError(backend.Close()) })
	const fingerprint = "synthetic-person-scope-4"
	gen, err := backend.CreateGeneration(ctx, "synthetic-model", 4, fingerprint)
	must.NoError(err)
	const count = 5000
	data := &sqliteScopeStore{revisions: make(map[int64]string, count), members: map[int64]map[int64]bool{
		11: {4500: true},
		22: {10: true, 4500: true},
	}}
	persons := make([]vector.PersonEmbedding, 0, count)
	for id := int64(1); id <= count; id++ {
		revision := fmt.Sprintf("revision-%d", id)
		data.revisions[id] = revision
		// Cosine distance from [1,0,0,0] increases with the positive second
		// component, placing person4500 strictly beyond the vec0 ceiling.
		persons = append(persons, vector.PersonEmbedding{PersonID: id, Revision: revision, Vector: []float32{1, float32(id) / 1000, 0, 0}})
	}
	must.NoError(backend.UpsertPersons(ctx, gen, persons))
	must.NoError(backend.ActivateGeneration(ctx, gen, true))
	for _, tc := range []struct {
		name           string
		organizationID int64
		limit          int
		want           []int64
	}{
		{"late member", 11, 1, []int64{4500}},
		{"underfilled scope", 22, 20, []int64{10, 4500}},
		{"empty scope", 33, 20, []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)

			embedder := &sqliteScopeEmbedder{}
			engine := personsearch.NewEngine(backend, data, embedder, personsearch.Config{
				ExpectedFingerprint: fingerprint,
				Gate:                vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil }),
				PersonCoverage: func(context.Context, vector.GenerationID) (personsearch.Coverage, error) {
					return personsearch.Coverage{}, nil
				},
			})
			results, err := engine.SearchInOrganization(t.Context(), "synthetic query", tc.limit, tc.organizationID)
			must.NoError(err)
			got := make([]int64, 0, len(results))
			for _, result := range results {
				got = append(got, result.Person.ID)
			}
			check.Equal(tc.want, got)
			check.Equal(1, embedder.calls, "widening must reuse the query embedding")
		})
	}
}
