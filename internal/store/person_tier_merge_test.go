package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMergePersons_ReconcilesTierOptOuts(t *testing.T) {
	tests := []struct {
		name          string
		survivorLanes []string
		absorbedLanes []string
		wantLanes     []string
		wantActions   map[string]string
	}{
		{
			name:          "absorbed-only opt-outs move to survivor",
			absorbedLanes: []string{"briefs", "tracking"},
			wantLanes:     []string{"briefs", "tracking"},
			wantActions:   map[string]string{"briefs": "moved", "tracking": "moved"},
		},
		{
			name:          "overlapping opt-outs deduplicate by lane",
			survivorLanes: []string{"tracking"},
			absorbedLanes: []string{"briefs", "tracking"},
			wantLanes:     []string{"briefs", "tracking"},
			wantActions:   map[string]string{"briefs": "moved", "tracking": "deduplicated"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			survivor := mustPromotedPerson(t, f.Store, "tier-survivor@example.com", "Survivor")
			absorbed := mustPromotedPerson(t, f.Store, "tier-absorbed@example.com", "Absorbed")

			insertOptOut := func(personID int64, lane string) int64 {
				var id int64
				err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
					INSERT INTO person_tier_opt_outs (person_id, lane, actor, reason)
					VALUES (?, ?, 'operator', 'manual choice') RETURNING id`), personID, lane).Scan(&id)
				require.NoError(err)
				return id
			}
			survivorOptOutIDs := make(map[string]int64, len(test.survivorLanes))
			for _, lane := range test.survivorLanes {
				survivorOptOutIDs[lane] = insertOptOut(survivor.ID, lane)
			}
			absorbedOptOutIDs := make(map[string]int64, len(test.absorbedLanes))
			for _, lane := range test.absorbedLanes {
				absorbedOptOutIDs[lane] = insertOptOut(absorbed.ID, lane)
			}

			merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
				SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
				ExpectedSurvivorRevision: survivor.Revision,
				ExpectedAbsorbedRevision: absorbed.Revision,
				IdempotencyKey:           "tier-opt-out-merge-" + test.name,
				Actor:                    "test",
			})
			require.NoError(err)
			assert.Equal(survivor.ID, merged.Person.ID)

			rows, err := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(`
				SELECT lane FROM person_tier_opt_outs WHERE person_id = ? ORDER BY lane`),
				merged.Person.ID)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			var gotLanes []string
			for rows.Next() {
				var lane string
				require.NoError(rows.Scan(&lane))
				gotLanes = append(gotLanes, lane)
			}
			require.NoError(rows.Err())
			assert.Equal(test.wantLanes, gotLanes)

			for lane, wantAction := range test.wantActions {
				var action string
				var currentRowID int64
				err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
					SELECT action, current_row_id FROM person_merge_rows
					WHERE merge_id = ? AND table_name = 'person_tier_opt_outs'
					  AND original_row_id = ?`), merged.Merge.ID, absorbedOptOutIDs[lane]).Scan(
					&action, &currentRowID,
				)
				require.NoError(err)
				assert.Equal(wantAction, action)
				wantCurrentRowID := absorbedOptOutIDs[lane]
				if wantAction == "deduplicated" {
					wantCurrentRowID = survivorOptOutIDs[lane]
				}
				assert.Equal(wantCurrentRowID, currentRowID)
			}
		})
	}
}
