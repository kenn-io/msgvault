package importer

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIngestRawMessageNUL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("mbox", "archive@example.org")
	require.NoError(err)
	raw, err := os.ReadFile("../mime/testdata/nul-projections.eml")
	require.NoError(err)
	err = IngestRawMessage(t.Context(), st, source.ID, source.Identifier, t.TempDir(), nil,
		"nul", fmt.Sprintf("%x", sha256.Sum256(raw)), raw, time.Time{}, slog.Default())
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"nul"})
	require.NoError(err)
	require.Contains(ids, "nul")
	id := ids["nul"]
	stored, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, stored)
	msg, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Report�tail", msg.Subject)
	assert.Equal("beforetoken�aftertoken", msg.Snippet)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal("beforetoken�aftertoken", body)
	for _, term := range []string{"beforetoken", "aftertoken"} {
		_, total, searchErr := st.SearchMessages(term, 0, 10)
		require.NoError(searchErr)
		assert.Equal(int64(1), total)
	}
	var filename string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT filename FROM attachments WHERE message_id = ?`), id).Scan(&filename))
	assert.Equal("file�name.bin", filename)
}
