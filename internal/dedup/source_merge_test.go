package dedup_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/dedup"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func mergedHistoryCopies(t *testing.T, withMessageID bool) (*store.Store, *dedup.Engine, int64, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	history, err := st.GetOrCreateSource("gmail", "history@example.test")
	require.NoError(t, err)
	live, err := st.GetOrCreateSource("gmail", "live@example.test")
	require.NoError(t, err)
	header := ""
	if withMessageID {
		header = "Message-ID: <merge-content@example.test>\r\n"
	}
	raw := []byte(header + "From: sender@example.test\r\nTo: recipient@example.test\r\n" +
		"Subject: Synthetic message\r\nDate: Tue, 14 Jan 2020 10:00:00 +0000\r\n\r\nBody\r\n")
	date := time.Date(2020, 1, 14, 10, 0, 0, 0, time.UTC)
	historicalID := ingestRawMessage(t, st, history, "history-message", raw, date)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
	require.NoError(t, err)
	liveID := ingestRawMessage(t, st, live, "live-message", raw, date)
	engine := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{live.ID}, Account: live.Identifier,
		ContentHashFallback: !withMessageID, DeleteDupsFromSourceServer: true,
		DeletionsDir: filepath.Join(t.TempDir(), "deletions"),
	}, nil)
	return st, engine, historicalID, liveID
}

func TestDedupPreservesRicherMergedHistory(t *testing.T) {
	for _, grouping := range []string{"message-id", "normalized-hash"} {
		t.Run(grouping, func(t *testing.T) {
			for _, content := range []string{"attachment", "different attachment", "hashless attachment", "text", "html", "equivalent"} {
				t.Run(content, func(t *testing.T) {
					st, engine, historicalID, liveID := mergedHistoryCopies(t, grouping == "message-id")
					reviewed, err := engine.Scan(t.Context())
					require.NoError(t, err)
					require.Len(t, reviewed.Groups, 1)
					assert.Equal(t, grouping, reviewed.Groups[0].KeyType)
					switch content {
					case "text", "html":
						text, html := sql.NullString{}, sql.NullString{}
						if content == "text" {
							text = sql.NullString{String: "Historical text", Valid: true}
							// The survivor has a body, but it lacks this historical content.
						} else {
							html = sql.NullString{String: "<p>Historical HTML</p>", Valid: true}
						}
						require.NoError(t, st.UpsertMessageBody(historicalID, text, html))
					default:
						write := store.AttachmentWrite{
							Filename: "note.txt", MIMEType: "text/plain", StoragePath: "synthetic/note",
							ContentHash: strings.Repeat("a", 64), Size: 20,
							Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
							SourcePartKey: "mime:2",
						}
						if content == "hashless attachment" {
							write.ContentHash = ""
						}
						require.NoError(t, st.UpsertAttachmentRecord(t.Context(), historicalID, write))
						if content == "different attachment" || content == "equivalent" {
							if content == "different attachment" {
								write.ContentHash = strings.Repeat("b", 64)
							}
							require.NoError(t, st.UpsertAttachmentRecord(t.Context(), liveID, write))
						}
					}

					refreshed, err := engine.Scan(t.Context())
					require.NoError(t, err)
					if content == "equivalent" {
						require.Len(t, refreshed.Groups, 1)
						group := refreshed.Groups[0]
						assert.Equal(t, liveID, group.Messages[group.Survivor].ID)
					} else {
						assert.Empty(t, refreshed.Groups, "richer history must stay visible")
						assert.Zero(t, refreshed.DuplicateGroups)
						assert.Zero(t, refreshed.DuplicateMessages)
					}
					// Execute the earlier plan: extraction can finish after the user reviews it.
					summary, err := engine.Execute(t.Context(), reviewed, "history-content")
					require.NoError(t, err)
					assert.Empty(t, summary.StagedManifests)
					assertSoftDeleted(t, st, liveID, false)
					assertSoftDeleted(t, st, historicalID, content == "equivalent")
					if content != "equivalent" {
						assert.Zero(t, summary.GroupsMerged)
						_, err = st.MergeDuplicates(liveID, []int64{historicalID}, "direct-history-content")
						require.ErrorContains(t, err, "would hide historical content", "direct store callers must also preserve richer history")
						assertSoftDeleted(t, st, historicalID, false)
					}
				})
			}
		})
	}
}

func TestDedupPreservesHistoricalAttachmentOccurrences(t *testing.T) {
	type part struct {
		name   string
		stored bool
	}
	for _, test := range []struct {
		name       string
		historical []part
		live       []part
		merge      bool
	}{
		{
			name:       "missing identical occurrence",
			historical: []part{{"note.txt", true}, {"note.txt", true}},
			live:       []part{{"note.txt", true}},
		},
		{
			name:       "equal totals with different occurrence counts",
			historical: []part{{"note.txt", true}, {"note.txt", true}, {"other.txt", true}},
			live:       []part{{"note.txt", true}, {"other.txt", true}, {"other.txt", true}},
		},
		{
			name:       "equal occurrences",
			historical: []part{{"note.txt", true}, {"note.txt", true}},
			live:       []part{{"note.txt", true}, {"note.txt", true}},
			merge:      true,
		},
		{
			name:       "missing stored occurrence",
			historical: []part{{"note.txt", true}, {"note.txt", true}},
			live:       []part{{"note.txt", true}, {"note.txt", false}},
		},
		{
			name:       "live has more stored occurrences",
			historical: []part{{"note.txt", true}, {"note.txt", false}},
			live:       []part{{"note.txt", true}, {"note.txt", true}},
			merge:      true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, engine, historicalID, liveID := mergedHistoryCopies(t, true)
			reviewed, err := engine.Scan(t.Context())
			require.NoError(err)
			require.Len(reviewed.Groups, 1)
			for id, parts := range map[int64][]part{historicalID: test.historical, liveID: test.live} {
				for i, p := range parts {
					path := ""
					if p.stored {
						path = "synthetic/attachment"
					}
					require.NoError(st.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{
						Filename: p.name, MIMEType: "text/plain", ContentHash: strings.Repeat("a", 64),
						StoragePath: path, Size: 20, SourcePartKey: fmt.Sprintf("mime:%d", i+2),
						Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
					}))
				}
				var count int
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), id).Scan(&count))
				require.Equal(len(parts), count, "the fixture must retain separate physical parts")
			}
			refreshed, err := engine.Scan(t.Context())
			require.NoError(err)
			if test.merge {
				assert.Len(refreshed.Groups, 1)
			} else {
				assert.Empty(refreshed.Groups)
			}
			// Execute the plan from before extraction to check current evidence.
			summary, err := engine.Execute(t.Context(), reviewed, "attachment-occurrences")
			require.NoError(err)
			assert.Empty(summary.StagedManifests)
			assertSoftDeleted(t, st, liveID, false)
			assertSoftDeleted(t, st, historicalID, test.merge)
			if !test.merge {
				assert.Zero(summary.GroupsMerged)
				_, err := st.MergeDuplicates(liveID, []int64{historicalID}, "direct-attachment-occurrences")
				require.ErrorContains(err, "would hide historical content")
				assertSoftDeleted(t, st, historicalID, false)
			}
		})
	}
}
