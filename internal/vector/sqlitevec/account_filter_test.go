//go:build sqlite_vec

package sqlitevec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAccountScopesFilterVectorAndFusedSearch(t *testing.T) {
	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0), 2: unitVec(768, 0), 3: unitVec(768, 0)})
	// The fixture's minimal messages table predates attribution.
	_, err := b.mainDB.ExecContext(ctx, `
		ALTER TABLE messages ADD COLUMN account_address TEXT;
		ALTER TABLE messages ADD COLUMN account_path TEXT;
		UPDATE messages SET
			account_address = CASE id WHEN 1 THEN 'work@example.org' WHEN 2 THEN 'mask@example.org' END,
			account_path = CASE id WHEN 3 THEN NULL ELSE 'inbound' END`)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		scopes []search.AccountScope
		id     int64
	}{
		"received":     {search.AccountScopesFromQuery(search.Parse("received:work@example.org")), 1},
		"account":      {[]search.AccountScope{{Addresses: []string{"mask@example.org"}}}, 2},
		"unattributed": {[]search.AccountScope{{Unattributed: true}}, 3},
	} {
		t.Run(name, func(t *testing.T) {
			filter := vector.Filter{AccountScopes: tc.scopes}
			hits, err := b.Search(ctx, gen, unitVec(768, 0), 1, filter)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, tc.id, hits[0].MessageID)
			fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
				FTSTerms: []string{"meeting"}, QueryVec: unitVec(768, 0), Generation: gen,
				KPerSignal: 1, Limit: 1, RRFK: 60, Filter: filter,
			})
			require.NoError(t, err)
			require.Len(t, fused, 1)
			assert.Equal(t, tc.id, fused[0].MessageID)
		})
	}
}
