//go:build sqlite_vec

package personsearch_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

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
	f := storetest.New(t)
	data := f.Store
	first, err := data.CreateOrganizationContext(ctx, store.OrganizationInput{Name: "First Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	second, err := data.CreateOrganizationContext(ctx, store.OrganizationInput{Name: "Second Company", Kind: store.OrganizationKindCompany})
	must.NoError(err)
	persons := make([]vector.PersonEmbedding, 0, count)
	var memberIDs []int64
	for i := range 2 {
		participant := f.EnsureParticipant(fmt.Sprintf("scope-%d@example.invalid", i), "Person", "example.invalid")
		person, _, err := data.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		memberIDs = append(memberIDs, person.ID)
		_, err = data.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: second.ID, Source: store.ProvenanceUser})
		must.NoError(err)
		if i == 1 {
			_, err = data.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: first.ID, Source: store.ProvenanceUser})
			must.NoError(err)
		}
		doc, err := data.LoadPersonSemanticDocumentContext(ctx, person.ID)
		must.NoError(err)
		rank := 10
		if i == 1 {
			rank = 4500
		}
		persons = append(persons, vector.PersonEmbedding{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, float32(rank) / 1000, 0, 0}})
	}
	for id := int64(1); id <= count; id++ {
		participant := f.EnsureParticipant(fmt.Sprintf("outsider-%d@example.invalid", id), "Outsider", "example.invalid")
		person, _, err := data.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		doc, err := data.LoadPersonSemanticDocumentContext(ctx, person.ID)
		must.NoError(err)
		persons = append(persons, vector.PersonEmbedding{PersonID: person.ID, Revision: doc.Revision, Vector: []float32{1, float32(id) / 1000, 0, 0}})
	}
	must.NoError(backend.UpsertPersons(ctx, gen, persons))
	must.NoError(backend.ActivateGeneration(ctx, gen, true))
	for _, tc := range []struct {
		name           string
		organizationID int64
		limit          int
		want           []int64
	}{
		{"late member", first.ID, 1, []int64{memberIDs[1]}},
		{"underfilled scope", second.ID, 20, memberIDs},
		{"empty scope", 999999, 20, []int64{}},
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
			start := time.Now()
			results, err := engine.Search(t.Context(), "synthetic query", tc.limit, tc.organizationID)
			t.Logf("scoped search: %s", time.Since(start))
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
