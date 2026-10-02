//go:build sqlite_vec

package sqlitevec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAccountFiltersRunBeforeVectorAndFusedRanking(t *testing.T) {
	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0), 2: unitVec(768, 0), 3: unitVec(768, 0)})
	_, err := b.mainDB.ExecContext(ctx, `
 ALTER TABLE messages ADD COLUMN account_address TEXT;
 ALTER TABLE messages ADD COLUMN account_path TEXT;
 ALTER TABLE messages ADD COLUMN account_attribution_basis TEXT DEFAULT 'not-derived';
 UPDATE messages SET source_id=1,account_address=CASE id WHEN 1 THEN 'work@example.org' WHEN 2 THEN 'mask@example.org' END, account_path='inbound',account_attribution_basis='original-recipient';
 CREATE TABLE account_identity_group_memberships(source_id INTEGER,group_key TEXT,address_key TEXT);
 INSERT INTO account_identity_group_memberships SELECT source_id,'fastmail-masked:inbox@example.net','mask@example.org' FROM messages WHERE id=2;`)
	require.NoError(t, err)
	for _, tc := range []struct {
		selector string
		id       int64
	}{
		{"received:work@example.org", 1}, {"account:fastmail-masked:inbox@example.net", 2}, {"account:unattributed", 3},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			parsed := search.Parse(tc.selector)
			require.NoError(t, parsed.Err())
			filter := vector.Filter{AccountScopes: parsed.AccountScopes}
			hits, err := b.Search(ctx, gen, unitVec(768, 0), 1, filter)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, tc.id, hits[0].MessageID)
			fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{FTSTerms: []string{"meeting"}, QueryVec: unitVec(768, 0), Generation: gen, KPerSignal: 1, Limit: 1, RRFK: 60, Filter: filter})
			require.NoError(t, err)
			require.Len(t, fused, 1)
			assert.Equal(t, tc.id, fused[0].MessageID)
		})
	}
}
