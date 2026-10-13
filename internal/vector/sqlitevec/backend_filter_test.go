//go:build sqlite_vec

package sqlitevec

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestVectorDateFiltersCompareInstants(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		timestamps []string
	}{
		{"utc", []string{"2026-01-15 12:00:00", "2026-01-15 14:00:00", "2026-01-15 15:00:00", "2026-01-15T14:00:00Z"}},
		{"offsets", []string{"2026-01-15T15:00:00+03:00", "2026-01-15T09:00:00-05:00", "2026-01-15 10:00:00-05:00", "2026-01-15T19:30:00+05:30"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			b, ctx := newFusedBackendForTest(t)
			gen := seedAndEmbed(t, b, map[int64][]float32{
				1: unitVec(4, 0), 2: unitVec(4, 0), 3: unitVec(4, 0), 4: unitVec(4, 0),
			})
			for i, timestamp := range fixture.timestamps {
				id := int64(i + 1)
				_, err := b.mainDB.ExecContext(ctx, `UPDATE messages SET sent_at = ? WHERE id = ?`, timestamp, id)
				require.NoError(t, err)
				_, err = b.mainDB.ExecContext(ctx, `INSERT OR REPLACE INTO messages_fts (rowid, subject, body) VALUES (?, 'topic', 'topic')`, id)
				require.NoError(t, err)
			}

			for _, bound := range []struct {
				name      string
				timestamp string
			}{
				{"utc", "2026-01-15T14:00:00Z"},
				{"negative_offset", "2026-01-15T09:00:00-05:00"},
				{"positive_offset", "2026-01-15T19:30:00+05:30"},
			} {
				t.Run(bound.name, func(t *testing.T) {
					boundary, err := time.Parse(time.RFC3339, bound.timestamp)
					require.NoError(t, err)
					end := boundary.Add(time.Hour)
					for _, tc := range []struct {
						name   string
						filter vector.Filter
						want   []int64
					}{
						{"after_inclusive", vector.Filter{After: &boundary}, []int64{2, 3, 4}},
						{"before_exclusive", vector.Filter{Before: &boundary}, []int64{1}},
						{"range", vector.Filter{After: &boundary, Before: &end}, []int64{2, 4}},
					} {
						t.Run(tc.name, func(t *testing.T) {
							t.Run("standalone", func(t *testing.T) {
								hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, tc.filter)
								require.NoError(t, err)
								var ids []int64
								for _, hit := range hits {
									ids = append(ids, hit.MessageID)
								}
								assert.ElementsMatch(t, tc.want, ids)
							})
							for _, mode := range []string{"fts", "vector", "hybrid"} {
								t.Run(mode, func(t *testing.T) {
									req := vector.FusedRequest{Generation: gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: tc.filter}
									if mode != "vector" {
										req.FTSTerms = []string{"topic"}
									}
									if mode != "fts" {
										req.QueryVec = unitVec(4, 0)
									}
									hits, _, err := b.FusedSearch(ctx, req)
									require.NoError(t, err)
									var ids []int64
									for _, hit := range hits {
										ids = append(ids, hit.MessageID)
									}
									assert.ElementsMatch(t, tc.want, ids)
								})
							}
						})
					}
				})
			}
		})
	}
}

func TestVectorFilterMessageIDsBeforeRanking(t *testing.T) {
	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{
		1: unitVec(768, 0), 2: unitVec(768, 0), 3: unitVec(768, 1),
	})

	hits, err := b.Search(ctx, gen, unitVec(768, 0), 10, vector.Filter{MessageIDs: []int64{2}})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(2), hits[0].MessageID)

	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		FTSTerms: []string{"meeting"}, QueryVec: unitVec(768, 0), Generation: gen,
		KPerSignal: 10, Limit: 10, RRFK: 60, Filter: vector.Filter{MessageIDs: []int64{2}},
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.Equal(t, int64(2), fused[0].MessageID)

	tooMany := make([]int64, vector.MaxFilterMessageIDs+1)
	_, err = b.Search(ctx, gen, unitVec(768, 0), 10, vector.Filter{MessageIDs: tooMany})
	require.ErrorIs(t, err, vector.ErrFilterTooLarge)
	_, _, err = b.FusedSearch(ctx, vector.FusedRequest{
		FTSTerms: []string{"meeting"}, Generation: gen, KPerSignal: 10, Limit: 10,
		Filter: vector.Filter{MessageIDs: tooMany},
	})
	require.ErrorIs(t, err, vector.ErrFilterTooLarge)
}
func TestVectorFilterConversationIDsBeforeRanking(t *testing.T) {
	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{
		1: unitVec(768, 0), 2: unitVec(768, 0), 3: unitVec(768, 0),
	})
	_, err := b.mainDB.ExecContext(ctx, `
		UPDATE messages
		SET conversation_id = CASE id WHEN 2 THEN 22 ELSE 11 END
	`)
	require.NoError(t, err)

	filter := vector.Filter{ConversationIDs: []int64{22}}
	hits, err := b.Search(ctx, gen, unitVec(768, 0), 10, filter)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(2), hits[0].MessageID)

	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: unitVec(768, 0), Generation: gen,
		KPerSignal: 10, Limit: 10, RRFK: 60, Filter: filter,
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.Equal(t, int64(2), fused[0].MessageID)
}

// TestVectorFilterListIDsBeforeRanking catches semantic and fused candidate
// queries admitting messages whose List-Id does not satisfy every literal term.
func TestVectorFilterListIDsBeforeRanking(t *testing.T) {
	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{
		1: unitVec(768, 0), 2: unitVec(768, 0), 3: unitVec(768, 0), 4: unitVec(768, 0),
	})
	_, err := b.mainDB.ExecContext(ctx, `UPDATE messages SET list_id = CASE id
		WHEN 1 THEN '<Announce.Shared.example.org>'
		WHEN 2 THEN '<announce.example.net>'
		WHEN 3 THEN '<Ops%_Team\Archive.example.org>'
		WHEN 4 THEN '<ÉCOLE.example.org>'
	END WHERE id IN (1, 2, 3, 4)`)
	require.NoError(t, err, "seed list ids")

	filter := vector.Filter{ListIDSubstrings: []string{"ANNOUNCE", "shared"}}
	hits, err := b.Search(ctx, gen, unitVec(768, 0), 10, filter)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].MessageID)

	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: filter,
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.Equal(t, int64(1), fused[0].MessageID)

	exactFilter := vector.Filter{ListID: "<ANNOUNCE.EXAMPLE.NET>"}
	exactHits, err := b.Search(ctx, gen, unitVec(768, 0), 10, exactFilter)
	require.NoError(t, err)
	require.Len(t, exactHits, 1)
	assert.Equal(t, int64(2), exactHits[0].MessageID)

	exactFused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: exactFilter,
	})
	require.NoError(t, err)
	require.Len(t, exactFused, 1)
	assert.Equal(t, int64(2), exactFused[0].MessageID)

	exactGroups := vector.Filter{ListIDExactGroups: [][]string{
		{"<Announce.Shared.example.org>", "<announce.example.net>"},
		{"<ANNOUNCE.SHARED.EXAMPLE.ORG>", "<missing.example.org>"},
	}}
	groupHits, err := b.Search(ctx, gen, unitVec(768, 0), 10, exactGroups)
	require.NoError(t, err)
	require.Len(t, groupHits, 1)
	assert.Equal(t, int64(1), groupHits[0].MessageID)

	groupFused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: exactGroups,
	})
	require.NoError(t, err)
	require.Len(t, groupFused, 1)
	assert.Equal(t, int64(1), groupFused[0].MessageID)

	literalHits, err := b.Search(ctx, gen, unitVec(768, 0), 10,
		vector.Filter{ListIDSubstrings: []string{"ops%_team"}})
	require.NoError(t, err)
	require.Len(t, literalHits, 1)
	assert.Equal(t, int64(3), literalHits[0].MessageID)

	backslashHits, err := b.Search(ctx, gen, unitVec(768, 0), 10,
		vector.Filter{ListIDSubstrings: []string{"ops%_team\\archive"}})
	require.NoError(t, err)
	require.Len(t, backslashHits, 1)
	assert.Equal(t, int64(3), backslashHits[0].MessageID)
	backslashFused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 10, Limit: 10, RRFK: 60,
		Filter: vector.Filter{ListIDSubstrings: []string{"ops%_team\\archive"}},
	})
	require.NoError(t, err)
	require.Len(t, backslashFused, 1)
	assert.Equal(t, int64(3), backslashFused[0].MessageID)

	unicodeFilter := vector.Filter{ListIDSubstrings: []string{"école"}}
	unicodeHits, err := b.Search(ctx, gen, unitVec(768, 0), 10, unicodeFilter)
	require.NoError(t, err)
	require.Len(t, unicodeHits, 1)
	assert.Equal(t, int64(4), unicodeHits[0].MessageID)

	unicodeFused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 10, Limit: 10, RRFK: 60, Filter: unicodeFilter,
	})
	require.NoError(t, err)
	require.Len(t, unicodeFused, 1)
	assert.Equal(t, int64(4), unicodeFused[0].MessageID)
}

func TestVectorFilenameFilterBeforeRanking(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0), 2: unitVec(768, 1)})
	_, err := b.mainDB.ExecContext(ctx, `CREATE TABLE attachments (id INTEGER PRIMARY KEY, message_id INTEGER, filename TEXT)`)
	require.NoError(err)
	_, err = b.mainDB.ExecContext(ctx, `INSERT INTO attachments(message_id,filename) VALUES (2,'Budget%_Plan.PDF'),(2,'界.csv'),(1,'BudgetXXPlan.pdf'),(1,'界.csv')`)
	require.NoError(err)
	filter, err := hybrid.BuildFilter(ctx, b.mainDB, nil, search.Parse(`filename:budget%_ filename:界`))
	require.NoError(err)
	hits, err := b.Search(ctx, gen, unitVec(768, 0), 1, filter)
	require.NoError(err)
	require.Len(hits, 1)
	assert.Equal(int64(2), hits[0].MessageID)
	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 10, Limit: 1, RRFK: 60, Filter: filter})
	require.NoError(err)
	require.Len(fused, 1)
	assert.Equal(int64(2), fused[0].MessageID)

	lexical, _, err := b.FusedSearch(ctx, vector.FusedRequest{FTSTerms: []string{"lunch"}, Generation: gen, KPerSignal: 10, Limit: 1, RRFK: 60, Filter: filter})
	require.NoError(err)
	assert.Empty(lexical, "filename filters apply to the lexical leg before fusion")
}
