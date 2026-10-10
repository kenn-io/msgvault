package query

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func FuzzMetadataSearchParity(f *testing.F) {
	for _, s := range []string{"needle", "ÉCOLE", "e\u0301cole", "%_\\", "a\"b", "foo-bar", "a b", "ab", "", "a\x00b", string([]byte{0xff, 'a', 'b', 'c'}), "𐐀abc"} {
		for _, flags := range []uint8{0, 15, 31, 64, 160} {
			f.Add([]byte("synthetic|prefix "+s+" suffix|unrelated|contact@example.org|+15550001111"), s, flags)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, term string, flags uint8) {
		// Bound database materialization, not the generated input domain.
		data = data[:min(len(data), 2048)]
		term = term[:min(len(term), 128)]
		db := metadataSearchDB(t)
		_, err := db.Exec(`DELETE FROM messages; DELETE FROM participants; DELETE FROM conversations; DELETE FROM sources; INSERT INTO sources(id,source_type,identifier) VALUES(1,'gmail','archive@example.org'), (2,'gmail','other@example.org');
  INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,1,'synthetic-thread','email_thread'), (2,2,'other-thread','email_thread');`)
		require.NoError(t, err)
		values := strings.Split(string(data), "|")
		at := func(n int) string { return values[n%len(values)] }
		for i := range 12 {
			_, err = db.Exec(`INSERT INTO participants(id,email_address,display_name,phone_number) VALUES(?,?,?,?)`, i+1, fmt.Sprintf("person%d@example.org", i), at(i+2), at(i+3)+fmt.Sprintf("-%d", i))
			require.NoError(t, err)
		}
		for i := range 12 {
			var subject, snippet any = at(i), at(i + 1)
			if i%4 == 0 {
				subject = nil
			}
			if i%5 == 0 {
				snippet = nil
			}
			if i == 1 {
				subject = "prefix " + term + " suffix"
			}
			if i == 2 {
				snippet = term
			}
			_, err = db.Exec(`INSERT INTO messages(id,conversation_id,source_id,message_type,source_message_id,subject,snippet,sent_at,sender_id,deleted_from_source_at)
    VALUES(?,?,?,'email',?,?,?, ?,?,?)`, i+1, i%2+1, i%2+1, fmt.Sprintf("synthetic-%d", i), subject, snippet, fmt.Sprintf("2024-01-%02d", i+1), i+1, func() any {
				if i%3 == 0 {
					return "2024-02-01"
				}
				return nil
			}())
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(?,?,'to',?), (?,?,'cc',?)`, i+1, (i+1)%12+1, at(i+4), i+1, (i+1)%12+1, at(i+5))
			require.NoError(t, err)
		}
		_, err = db.Exec(`UPDATE messages SET deleted_at='2024-02-01' WHERE id%4=0`)
		require.NoError(t, err)
		q := &search.Query{TextTerms: []string{term}, HideDeleted: flags&1 != 0}
		if flags&2 != 0 {
			q.TextTerms = append(q.TextTerms, at(0))
		}
		if flags&4 != 0 {
			q.AccountIDs = []int64{1}
		}
		filter := MessageFilter{HideDeletedFromSource: flags&8 != 0}
		if flags&16 != 0 {
			filter.ConversationID = new(int64(1))
		}
		if flags&32 != 0 {
			filter.SourceIDs = []int64{2}
		}
		if flags&64 != 0 {
			filter.SourceIDs = []int64{}
		}
		if flags&128 != 0 {
			after := time.Date(2024, 1, 6, 0, 0, 0, 0, time.UTC)
			filter.After = &after
		}
		indexed := NewSQLiteEngine(db)
		scan := NewEngineWithDialect(db, metadataScanDialect{})
		want, err := scan.SearchFast(t.Context(), q, filter, 5, int(flags%4))
		require.NoError(t, err)
		got, err := indexed.SearchFast(t.Context(), q, filter, 5, int(flags%4))
		require.NoError(t, err)
		assert.Equal(t, want, got, "page must preserve order, projection, and population")
		wantCount, err := scan.SearchFastCount(t.Context(), q, filter)
		require.NoError(t, err)
		gotCount, err := indexed.SearchFastCount(t.Context(), q, filter)
		require.NoError(t, err)
		assert.Equal(t, wantCount, gotCount)
		wantStats, err := scan.SearchFastWithStats(t.Context(), q, "", filter, ViewSenders, 5, 0)
		require.NoError(t, err)
		gotStats, err := indexed.SearchFastWithStats(t.Context(), q, "", filter, ViewSenders, 5, 0)
		require.NoError(t, err)
		assert.Equal(t, wantStats, gotStats)
	})
}
