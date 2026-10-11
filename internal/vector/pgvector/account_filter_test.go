//go:build pgvector

package pgvector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAccountScopesFilterVectorAndFusedSearch(t *testing.T) {
	f := newFusedFixture(t)
	for _, id := range []int64{1, 2, 3, 4} {
		f.seedMsg(t, id, "meeting update", "meeting details", 10, time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC), false)
	}
	f.embedAll(t, map[int64][]float32{
		1: unitVec(4, 1), 2: unitVec(4, 2), 3: unitVec(4, 3), 4: unitVec(4, 0),
	})
	// The fixture's minimal messages table predates attribution.
	_, err := f.db.ExecContext(f.ctx, `
		ALTER TABLE messages ADD COLUMN IF NOT EXISTS account_address TEXT;
		ALTER TABLE messages ADD COLUMN IF NOT EXISTS account_path TEXT;
		UPDATE messages SET account_address = CASE id
			WHEN 1 THEN 'work@example.org' WHEN 2 THEN 'mask@example.org' WHEN 4 THEN 'work@example.org' END,
			account_path = CASE id WHEN 4 THEN 'sent' ELSE 'inbound' END;`)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		scopes  []search.AccountScope
		wantIDs []int64
	}{
		"account":      {search.AccountScopesFromQuery(search.Parse("account:work@example.org")), []int64{1, 4}},
		"received":     {search.AccountScopesFromQuery(search.Parse("received:work@example.org")), []int64{1}},
		"unattributed": {[]search.AccountScope{{Unattributed: true}}, []int64{3}},
		"missing":      {[]search.AccountScope{{Addresses: []string{"missing@example.org"}}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			filter := vector.Filter{AccountScopes: tc.scopes, SourceIDs: []int64{10}}
			hits, err := f.b.Search(f.ctx, f.gen, unitVec(4, 0), 1, filter)
			require.NoError(err)
			fused, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
				FTSTerms: []string{"meeting"}, QueryVec: unitVec(4, 0), Generation: f.gen,
				KPerSignal: 1, Limit: 1, RRFK: 60, Filter: filter,
			})
			require.NoError(err)
			if len(tc.wantIDs) == 0 {
				assert.Empty(hits)
				assert.Empty(fused)
				return
			}
			require.Len(hits, 1)
			assert.Contains(tc.wantIDs, hits[0].MessageID)
			require.Len(fused, 1)
			assert.Contains(tc.wantIDs, fused[0].MessageID)
		})
	}
}
