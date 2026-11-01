package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportReplayPreservesGenericStoreReplyEdit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "provider child"), *comment("100_202", "100_200", "generic reply target")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	child, target := message(t, st, src.ID, "100_201"), message(t, st, src.ID, "100_202")
	before, err := st.GetMessageMetadata(child.ID)
	require.NoError(err)
	require.NoError(st.SetMessageReplyContext(t.Context(), child.ID, target.ID))
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	after := message(t, st, src.ID, "100_201")
	assert.Equal(target.ID, after.ReplyToMessageID.Int64)
	metadata, err := st.GetMessageMetadata(child.ID)
	require.NoError(err)
	assert.JSONEq(before.String, metadata.String, "generic edge edit must not reparent immutable provider metadata")
}
