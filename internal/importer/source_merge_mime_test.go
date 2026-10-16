package importer

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/dedup"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestMergeSourcesRequiresCompleteMIMEAttachments(t *testing.T) {
	for _, tc := range []struct {
		name                                                                            string
		failed, partial, sameAttachment, changedBody, sameRFC822ID, extraHistoricalPart bool
		hidden                                                                          int64
		dedupHidden                                                                     int
	}{
		{name: "failed storage distinct attachments", failed: true, hidden: 0},
		{name: "partially stored distinct attachments", partial: true, sameRFC822ID: true, hidden: 0},
		{name: "stored distinct attachments", sameRFC822ID: true, hidden: 0},
		{name: "stored identical attachments", sameAttachment: true, hidden: 1},
		{name: "failed storage different body", failed: true, changedBody: true, sameRFC822ID: true, hidden: 0},
		{name: "failed storage matching RFC822 ID", failed: true, sameRFC822ID: true, hidden: 0},
		{name: "failed storage identical attachments", failed: true, sameAttachment: true, sameRFC822ID: true, dedupHidden: 1},
		{name: "failed storage extra identical occurrence", failed: true, sameAttachment: true, sameRFC822ID: true, extraHistoricalPart: true},
		{name: "stored identical attachments matching RFC822 ID", sameAttachment: true, sameRFC822ID: true, hidden: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			history, err := st.GetOrCreateSource("mbox", "history@example.test")
			require.NoError(err)
			live, err := st.GetOrCreateSource("mbox", "live@example.test")
			require.NoError(err)
			dir := t.TempDir()
			if tc.failed {
				// The ingest must retain raw MIME even when attachment writes fail.
				dir = filepath.Join(dir, "attachments")
				require.NoError(os.WriteFile(dir, []byte("not a directory"), 0600))
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			ids := make([]int64, 2)
			attachments := []string{"historical attached bytes", "destination attached bytes"}
			if tc.sameAttachment {
				attachments[1] = attachments[0]
			}
			attachmentCount := 1
			if tc.partial {
				attachmentCount = 2
				commonHash := fmt.Sprintf("%x", sha256.Sum256([]byte("stored common attachment")))
				for _, content := range attachments {
					hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
					require.NotEqual(commonHash[:2], hash[:2])
					// Block just the distinct attachment's hash directory.
					require.NoError(os.WriteFile(filepath.Join(dir, hash[:2]), []byte("not a directory"), 0600))
				}
			}
			for i, src := range []*store.Source{history, live} {
				body := "same body text"
				if tc.changedBody && i == 1 {
					body = "a different body"
				}
				message := email.NewMessage().
					From("sender@example.test").To("recipient@example.test").
					Subject("Same envelope").Date("Mon, 01 Jan 2024 12:00:00 +0000").
					Body(body).WithAttachment("evidence.txt", "text/plain", []byte(attachments[i]))
				if tc.partial {
					message.WithAttachment("common.txt", "text/plain", []byte("stored common attachment"))
				}
				wantParts := attachmentCount
				if tc.extraHistoricalPart && i == 0 {
					message.WithAttachment("evidence.txt", "text/plain", []byte(attachments[i]))
					wantParts++
				}
				if tc.sameRFC822ID {
					message.Header("Message-ID", "<shared@example.test>")
				}
				raw := message.Bytes()
				parsed, err := msgmime.Parse(raw)
				require.NoError(err)
				require.Len(parsed.Attachments, wantParts)
				require.Equal(attachments[i], string(parsed.Attachments[0].Content))
				providerID := fmt.Sprintf("message-%d", i)
				require.NoError(IngestRawMessage(t.Context(), st, src.ID, src.Identifier, dir, nil, providerID, fmt.Sprintf("%x", sha256.Sum256(raw)), raw, time.Time{}, logger))
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), src.ID, providerID).Scan(&ids[i]))
				gotRaw, err := st.GetMessageRaw(ids[i])
				require.NoError(err)
				require.Equal(raw, gotRaw)
				var count, metadataCount int
				var has bool
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), ids[i]).Scan(&count))
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT has_attachments,attachment_count FROM messages WHERE id = ?`), ids[i]).Scan(&has, &metadataCount))
				if tc.failed {
					require.Zero(count)
					require.Zero(metadataCount)
					require.False(has)
				} else {
					require.Equal(1, count)
					require.Equal(1, metadataCount)
					require.True(has)
				}
			}
			if tc.failed || tc.partial {
				require.Contains(logs.String(), "failed to store attachment")
			}
			preview, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID, DryRun: true})
			require.NoError(err)
			assert.Equal(tc.hidden, preview.DuplicatesHidden)
			merged, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
			require.NoError(err)
			assert.Equal(tc.hidden, merged.DuplicatesHidden)
			var visible int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND deleted_at IS NULL`), live.ID).Scan(&visible))
			assert.Equal(2-tc.hidden, visible)
			engine := dedup.NewEngine(st, dedup.Config{
				AccountSourceIDs: []int64{live.ID}, Account: live.Identifier,
				ContentHashFallback: true,
			}, logger)
			report, err := engine.Scan(t.Context())
			require.NoError(err)
			assert.Len(report.Groups, tc.dedupHidden, "dedup must preserve unique MIME attachment evidence")
			if len(report.Groups) > 0 {
				assert.Equal(ids[1], report.Groups[0].Messages[report.Groups[0].Survivor].ID)
			}
			summary, err := engine.Execute(t.Context(), report, "merged-mime-attachments")
			require.NoError(err)
			assert.Equal(tc.dedupHidden, summary.MessagesRemoved)
			if tc.hidden == 0 && tc.dedupHidden == 0 {
				_, err := st.MergeDuplicates(ids[1], []int64{ids[0]}, "direct-merged-mime-attachments")
				require.ErrorContains(err, "would hide historical content")
			}
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND deleted_at IS NULL`), live.ID).Scan(&visible))
			assert.Equal(2-tc.hidden-int64(tc.dedupHidden), visible)
			for i, id := range ids {
				raw, err := st.GetMessageRaw(id)
				require.NoError(err)
				parsed, err := msgmime.Parse(raw)
				require.NoError(err)
				wantParts := attachmentCount
				if tc.extraHistoricalPart && i == 0 {
					wantParts++
				}
				require.Len(parsed.Attachments, wantParts)
				assert.Equal(attachments[i], string(parsed.Attachments[0].Content))
			}
		})
	}
}
