package export

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMediaSlots(t *testing.T) {
	st := testutil.NewTestStore(t)
	fresh, err := LoadMediaSlots(t.Context(), st, 0)
	require.NoError(t, err)
	assert.True(t, fresh.Pending("missing"))
	assert.True(t, fresh.Keep(store.AttachmentWrite{}))
	old := &MediaSlots{Existing: map[string]store.AttachmentRef{
		"pending": {}, "stored": {ContentHash: "abc"},
	}}
	assert.True(t, old.Pending("pending"))
	assert.False(t, old.Pending("stored"))
	assert.False(t, old.Pending("new"))
	assert.False(t, old.Keep(store.AttachmentWrite{}))
	assert.True(t, old.Keep(store.AttachmentWrite{ContentHash: "abc"}))
}
