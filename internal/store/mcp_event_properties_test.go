package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Recorded times can arrive out of sequence after buffered ingestion. Hard
// retention must preserve recent receipts even below the highest deleted seq.
func FuzzMCPEventsRetentionPreservesRecentReceipts(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{255})
	f.Add([]byte{0})
	f.Add([]byte{255, 0, 255})
	f.Add([]byte{0, 255, 0, 255, 0})
	f.Fuzz(func(t *testing.T, ages []byte) {
		fixture := storetest.New(t)
		now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
		clock, err := fixture.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
		require.NoError(t, err)
		var wantFloor int64
		wantSurvivors := make([]int64, 0)
		// Materialize a bounded number of rows from the unrestricted byte
		// domain so database cost remains finite in a fuzz iteration.
		for i, age := range ages[:min(len(ages), 64)] {
			seq := int64(i + 1)
			recorded := now
			if age < 128 {
				recorded = now.Add(-8 * 24 * time.Hour)
				wantFloor = seq
			} else {
				wantSurvivors = append(wantSurvivors, seq)
			}
			_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,'msgvault.message_archived','message','conversation',?,?,?, ?,?,FALSE,?,?,?)`), seq, clock.Epoch, fixture.ConvID, fmt.Sprintf("message:%d", seq), seq, fixture.ConvID, fixture.Source.ID, now.Format(time.RFC3339Nano), recorded.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
			require.NoError(t, err)
		}
		_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), min(len(ages), 64))
		require.NoError(t, err)
		require.NoError(t, fixture.Store.PruneMCPEvents(t.Context(), now, 7*24*time.Hour))
		rows, err := fixture.Store.DB().Query(`SELECT seq FROM mcp_event_log ORDER BY seq`)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		gotSurvivors := make([]int64, 0)
		for rows.Next() {
			var seq int64
			require.NoError(t, rows.Scan(&seq))
			gotSurvivors = append(gotSurvivors, seq)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, wantSurvivors, gotSurvivors)
		var gotFloor int64
		require.NoError(t, fixture.Store.DB().QueryRow(`SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&gotFloor))
		assert.Equal(t, wantFloor, gotFloor)
	})
}
