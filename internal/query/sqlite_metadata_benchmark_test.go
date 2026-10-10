package query

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

// Both engines read the same generated archive. Setup and index writes are
// outside the timed loops; results are checked without imposing latency limits.
func BenchmarkMetadataSubstring(b *testing.B) {
	db := metadataSearchDB(b)
	tx, err := db.Begin()
	require.NoError(b, err)
	insert, err := tx.Prepare(`INSERT INTO messages(conversation_id,source_id,message_type,source_message_id,subject,snippet,sent_at,sender_id) VALUES(1,1,'email',?,?,?,'2023-01-01',1)`)
	require.NoError(b, err)
	defer func() { require.NoError(b, insert.Close()) }()
	for i := range 20000 {
		subject := "common synthetic message"
		if i == 10000 {
			subject += " rare-marker"
		}
		_, err = insert.Exec(fmt.Sprintf("benchmark-%d", i), subject, "generated preview")
		require.NoError(b, err)
	}
	_, err = tx.Exec(`INSERT INTO participants(id,email_address,display_name) VALUES(2,'benchmark@example.org','Rare-Contact');
 UPDATE messages SET sender_id=2 WHERE source_message_id='benchmark-19999';
 INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name)
 SELECT id,1,'to','Generated Alias' FROM messages WHERE source_message_id LIKE 'benchmark-%';
 UPDATE message_recipients SET display_name='Rare-Occurrence' WHERE message_id=(SELECT id FROM messages WHERE source_message_id='benchmark-19999');`)
	require.NoError(b, err)
	require.NoError(b, tx.Commit())
	for _, tc := range []struct{ name, query string }{
		{"rare", "rare-marker"}, {"participant", "rare-contact"}, {"alias", "rare-occurrence"}, {"common", "common"}, {"short", "co"}, {"scoped_common", "after:2024-01-01 common"},
	} {
		for _, optimized := range []bool{false, true} {
			mode := "scan"
			engine := NewEngineWithDialect(db, metadataScanDialect{})
			if optimized {
				mode = "indexed"
				engine = NewSQLiteEngine(db)
			}
			b.Run(tc.name+"/"+mode, func(b *testing.B) {
				q := search.Parse(tc.query)
				want, err := NewEngineWithDialect(db, metadataScanDialect{}).SearchFastCount(b.Context(), q, MessageFilter{})
				require.NoError(b, err)
				_, err = engine.SearchFastCount(b.Context(), q, MessageFilter{})
				require.NoError(b, err)
				b.ResetTimer()
				for b.Loop() {
					got, err := engine.SearchFastCount(b.Context(), q, MessageFilter{})
					require.NoError(b, err)
					require.Equal(b, want, got)
				}
			})
		}
	}
}
