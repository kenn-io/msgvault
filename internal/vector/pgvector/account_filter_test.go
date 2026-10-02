//go:build pgvector

package pgvector

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAccountFiltersRunBeforeVectorAndFusedRanking(t *testing.T) {
	f := newFusedFixture(t)
	for _, id := range []int64{1, 2, 3, 4} {
		f.seedMsg(t, id, "meeting update", "meeting details", 10, time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC), false)
	}
	f.embedAll(t, map[int64][]float32{
		1: unitVec(4, 1), 2: unitVec(4, 2), 3: unitVec(4, 3), 4: unitVec(4, 0),
	})
	_, err := f.db.ExecContext(f.ctx, `
		ALTER TABLE messages ADD COLUMN account_address TEXT;
		ALTER TABLE messages ADD COLUMN account_path TEXT;
		ALTER TABLE messages ADD COLUMN account_attribution_basis TEXT DEFAULT 'not-derived';
		UPDATE messages SET account_address=CASE id
			WHEN 1 THEN 'work@example.org' WHEN 2 THEN 'mask@example.org' WHEN 4 THEN 'work@example.org' END,
			account_path=CASE id WHEN 4 THEN 'sent' ELSE 'inbound' END;
		CREATE TABLE account_identity_group_memberships(source_id BIGINT,group_key TEXT,address_key TEXT);
		INSERT INTO account_identity_group_memberships VALUES (10,'fastmail-masked:inbox@example.net','mask@example.org');`)
	require.NoError(t, err)
	for _, tc := range []struct {
		selector string
		wantIDs  []int64
	}{
		{"account:work@example.org", []int64{1, 4}},
		{"received:work@example.org", []int64{1}},
		{"account:fastmail-masked:inbox@example.net", []int64{2}},
		{"account:unattributed", []int64{3}},
		{"account:missing@example.org", nil},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			parsed := search.Parse(tc.selector)
			require.NoError(parsed.Err())
			for i := range parsed.AccountScopes {
				for j, address := range parsed.AccountScopes[i].Addresses {
					parsed.AccountScopes[i].Addresses[j] = " \t" + strings.ToUpper(address) + " \n"
				}
				for j, group := range parsed.AccountScopes[i].Groups {
					parsed.AccountScopes[i].Groups[j] = " \t" + strings.ToUpper(group) + " \n"
				}
			}
			require.NoError(search.ValidateAccountScopes(parsed.AccountScopes))
			filter := vector.Filter{AccountScopes: parsed.AccountScopes, SourceIDs: []int64{10}}
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
