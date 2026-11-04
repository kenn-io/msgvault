package rederive

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailNULRederive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, id := archiveEmailSource(t, "mbox", "stale body")
	raw, err := os.ReadFile("../mime/testdata/nul-projections.eml")
	require.NoError(err)
	require.NoError(st.UpsertMessageRaw(id, raw))
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET snippet = ? WHERE id = ?`), "stale body", id)
	require.NoError(err)
	// An archive that completed the prior parser pass must still be refreshed.
	require.NoError(st.MarkMigrationApplied(LedgerKey(source.SourceType, source.Identifier, "email-v1")))
	sum, _, err := RunIfStale(t.Context(), st, source.SourceType, source.Identifier, source.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Equal(int64(1), sum.BodiesRewritten)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("beforetoken�aftertoken", body)
	msg, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("beforetoken�aftertoken", msg.Snippet)
	stored, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, stored)
	_, total, err := st.SearchMessages("aftertoken", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	sum, err = Run(t.Context(), st, source.SourceType, source.Identifier, source.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Zero(sum.BodiesRewritten, "normalization must be idempotent")
}
