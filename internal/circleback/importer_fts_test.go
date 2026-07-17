//go:build fts5

package circleback

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestImport_FTSIndexed verifies imported meetings are searchable, including
// archived transcript text recovered while current notes are refreshed.
func TestImport_FTSIndexed(t *testing.T) {
	testutil.SkipIfPostgres(t, "directly MATCH-queries the SQLite FTS5 vtable; PG uses a tsvector column")

	for _, archive := range archivedTranscriptFixtures {
		for _, refresh := range transcriptRefreshFixtures {
			t.Run(archive.name+"/"+refresh.name, func(t *testing.T) {
				require := require.New(t)
				f := &fakeSource{
					meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
					transcripts: map[string]json.RawMessage{"42": archive.payload},
				}
				imp, st := newTestImporter(t, f)
				require.True(st.FTS5Available(), "FTS5 build tag set but FTS5 not available")

				_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)
				f.meetings["42"] = json.RawMessage(refreshedMeeting42)
				if refresh.payload == nil {
					delete(f.transcripts, "42")
				} else {
					f.transcripts["42"] = refresh.payload
				}
				_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)

				for _, term := range []string{
					archive.ftsTerm, "refreshedsignal", "reviewbudgetdelta",
					"pineapplemetric", "forecasttag",
				} {
					var hits int
					require.NoError(st.DB().QueryRow(
						`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, term).Scan(&hits),
						"MATCH %q", term)
					require.Equal(1, hits, "expected recovered/current FTS hit for %q", term)
				}
				var staleHits int
				require.NoError(st.DB().QueryRow(
					`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'layout'`).Scan(&staleHits))
				require.Zero(staleHits, "the canonical FTS row must replace stale meeting notes")
			})
		}
	}
}
