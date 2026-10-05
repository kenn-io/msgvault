package importer

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

func TestPstThreadingPipeline(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "owner@example.test")
			require.NoError(err)
			for _, i := range order {
				mid := fmt.Sprintf("<message-%d@example.test>", i)
				parent := ""
				if i > 0 {
					parent = fmt.Sprintf("<message-%d@example.test>", i-1)
				}
				headers := "From: sender@example.test\r\nSubject: Thread\r\n"
				body := "Synthetic body"
				entry := pstreader.ExtractMessage(&pstlib.Message{Identifier: pstlib.Identifier(i + 1), Properties: &properties.Message{InternetMessageId: &mid, InReplyToId: &parent, TransportMessageHeaders: &headers, Body: &body}}, "Inbox")
				require.NotNil(entry)
				raw, err := pstreader.BuildRFC5322(entry, nil)
				require.NoError(err)
				sourceID := fmt.Sprintf("pst-synthetic-%d", i)
				require.NoError(IngestRawMessage(t.Context(), st, src.ID, "owner@example.test", "", nil, sourceID, strconv.Itoa(i), raw, time.Time{}, slog.Default()))
				require.NoError(recordPstMessageHeaders(t.Context(), st, src.ID, sourceID, raw))
			}
			require.NoError(st.ResolveEmailReplyParentsContext(t.Context(), src.ID, 0, nil))
			require.NoError(st.ReconcilePstEmailThreadsContext(t.Context(), src.ID))
			rows, err := st.DB().Query(`SELECT conversation_id, rfc822_message_id, reply_to_message_id FROM messages ORDER BY source_message_id`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			var conv int64
			count := 0
			for i := 0; rows.Next(); i++ {
				count++
				var gotConv int64
				var rfcID string
				var parent sql.NullInt64
				require.NoError(rows.Scan(&gotConv, &rfcID, &parent))
				if i == 0 {
					conv = gotConv
				}
				assert.Equal(conv, gotConv)
				assert.Equal(fmt.Sprintf("message-%d@example.test", i), rfcID)
				assert.Equal(i > 0, parent.Valid)
			}
			require.NoError(rows.Err())
			assert.Equal(3, count)
		})
	}
}

func TestImportPst_RerunRepairsMissingIdentifiers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openIntegrationStore(t)
	path := filepath.Join(pstTestdataDir, "support.pst")
	opts := PstImportOptions{Identifier: "owner@example.test", NoResume: true}
	first, err := ImportPst(t.Context(), st, path, opts)
	require.NoError(err)
	require.Zero(first.Errors)
	// Query only metadata, then simulate the historical importer losing IDs.
	rows, err := st.DB().Query(`SELECT id, rfc822_message_id FROM messages WHERE rfc822_message_id IS NOT NULL AND rfc822_message_id <> ''`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	original := map[int64]string{}
	for rows.Next() {
		var id int64
		var rfcID string
		require.NoError(rows.Scan(&id, &rfcID))
		original[id] = rfcID
	}
	require.NoError(rows.Err())
	require.NotEmpty(original)
	rawBefore := map[int64][32]byte{}
	for id := range original {
		raw, readErr := st.GetMessageRaw(id)
		require.NoError(readErr)
		rawBefore[id] = sha256.Sum256(raw)
	}
	_, err = st.DB().Exec(`UPDATE messages SET rfc822_message_id = NULL, metadata = NULL`)
	require.NoError(err)
	// Earlier imports without identifiers keyed conversations by content hash.
	_, err = st.DB().Exec(`UPDATE conversations SET source_conversation_id = 'legacy-' || id`)
	require.NoError(err)
	var conversationsBefore int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&conversationsBefore))
	second, err := ImportPst(t.Context(), st, path, opts)
	require.NoError(err)
	require.Zero(second.Errors)
	assert.Equal(first.MessagesAdded, second.MessagesSkipped)
	assert.Zero(second.MessagesAdded)
	var conversationsAfter int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&conversationsAfter))
	assert.Equal(conversationsBefore, conversationsAfter, "repair must not add placeholder conversations")
	for id, want := range original {
		var got string
		require.NoError(st.DB().QueryRow(`SELECT COALESCE(rfc822_message_id,'') FROM messages WHERE id = ?`, id).Scan(&got))
		assert.Equal(sha256.Sum256([]byte(want)), sha256.Sum256([]byte(got)))
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		assert.Equal(rawBefore[id], sha256.Sum256(raw))
	}
}
