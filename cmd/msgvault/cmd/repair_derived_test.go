package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFormatRepairDerivedSummaryReportsMessageMetadata(t *testing.T) {
	got := formatRepairDerivedSummary("discord/example", &rederive.Summary{
		MessagesScanned:          3,
		MessageMetadataRewritten: 2,
		AttachmentsTagged:        4,
		Duration:                 1250 * time.Millisecond,
	})
	assert.Equal(t,
		"discord/example: 3 messages scanned, 2 message metadata rewritten, 0 bodies rewritten, 4 attachments tagged (1s)\n",
		got,
	)
}

func TestRepairDerivedTargetsRejectsUnknownSourceType(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "inbox@example.net")
	require.NoError(err)
	saved := repairDerivedSourceTypes
	t.Cleanup(func() { repairDerivedSourceTypes = saved })

	repairDerivedSourceTypes = []string{"gmial"}
	_, err = repairDerivedTargets(st)
	require.ErrorContains(err, `no source of type "gmial" to repair`)

	repairDerivedSourceTypes = []string{"gmail"}
	targets, err := repairDerivedTargets(st)
	require.NoError(err)
	require.Len(targets, 1)
	var out bytes.Buffer
	require.NoError(repairDerivedSource(t.Context(), &out, st, targets[0]))
	assert.Empty(t, out.String())
}

func TestRepairDerivedSourceRebuildsEmail(t *testing.T) {
	for _, sourceType := range []string{"outlook", "beeper", "discord"} {
		t.Run(sourceType, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			root := filepath.Join(t.TempDir(), "Inbox.mailbox")
			require.NoError(os.Mkdir(root, 0o700))
			require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("From: sender@example.net\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nRebuilt archive"), 0o600))
			_, err := importer.ImportEMLDir(t.Context(), st, root, importer.EMLImportOptions{SourceType: sourceType, Identifier: "repair@example.net"})
			require.NoError(err)
			src, err := st.GetOrCreateSource(sourceType, "repair@example.net")
			require.NoError(err)
			var id int64
			require.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&id))
			// Model an archive imported before MIME repair existed.
			_, err = st.DB().Exec("DELETE FROM applied_migrations WHERE name LIKE 'rederive:%'")
			require.NoError(err)
			if _, version, ok := rederive.Lookup(sourceType); ok {
				require.NoError(st.MarkMigrationApplied(rederive.LedgerKey(sourceType, src.Identifier, version)))
			}
			require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "stale", Valid: true}, sql.NullString{}))
			var out bytes.Buffer
			if sourceType == "outlook" {
				require.NoError(repairDerivedSource(t.Context(), &out, st, src))
				assert.Contains(out.String(), "1 bodies rewritten")
			} else {
				_, _, err := rederive.RunIfStale(t.Context(), st, sourceType, src.Identifier, src.ID, nil)
				require.NoError(err)
			}
			body, err := st.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal("Rebuilt archive", body)
			matches, _, err := st.SearchMessages("Rebuilt", 0, 10)
			require.NoError(err)
			assert.Len(matches, 1)
			_, ran, err := rederive.RunIfStale(t.Context(), st, sourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			assert.False(ran)
			if sourceType == "beeper" {
				older, err := st.GetOrCreateSource(sourceType, "older@example.net")
				require.NoError(err)
				conv, err := st.EnsureConversation(older.ID, "older", "")
				require.NoError(err)
				oldID, err := st.UpsertMessage(&store.Message{SourceID: older.ID, ConversationID: conv, SourceMessageID: "older", MessageType: "email"})
				require.NoError(err)
				require.NoError(st.UpsertMessageBody(oldID, sql.NullString{String: "stale", Valid: true}, sql.NullString{}))
				require.NoError(st.UpsertMessageRaw(oldID, []byte("Content-Type: text/plain\r\n\r\nMerged history")))
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: older.ID, IntoSourceID: src.ID})
				require.NoError(err)
				_, _, err = rederive.RunIfStale(t.Context(), st, sourceType, src.Identifier, src.ID, nil)
				require.NoError(err)
				body, err = st.GetMessageBodyText(oldID)
				require.NoError(err)
				assert.Equal("Merged history", body)
			}
		})
	}
}
