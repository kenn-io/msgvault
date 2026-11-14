package cmd

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDraftForwardEmptyNoteEdit(t *testing.T) {
	require := require.New(t)

	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	events, err := lintRun(t, a, nil, "draft-forward", strconv.FormatInt(f.parentID, 10), "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "alex@example.com", "--body", "original", "--json")
	require.NoError(err)
	id, ok := lintObject(t, events)["draft_id"].(string)
	require.True(ok)
	args := []string{"draft-edit", id, "--revision", "1", "--body", "", "--strict", "--json"}
	_, err = lintRun(t, a, nil, args...)
	require.NoError(err)
	after, err := f.store.GetIMAPDraftContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(t, int64(2), after.Revision)
}
