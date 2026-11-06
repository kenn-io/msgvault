package signalandroid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestSignalRerunPreservesArchiveLabels(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	first := runImport(t, st, dir, storage)
	labelID, err := st.EnsureLabel(first.SourceID, "kept", "Kept", "user")
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages ORDER BY id LIMIT 1`).Scan(&messageID))
	require.NoError(st.AddMessageLabels(messageID, []int64{labelID}))
	runImport(t, st, dir, storage)
	labels, err := st.MessageLabelIDsContext(t.Context(), messageID)
	require.NoError(err)
	assert.Equal([]int64{labelID}, labels)
}
