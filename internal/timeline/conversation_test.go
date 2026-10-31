package timeline

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestAnchorConversationModeUsesMessageEntries(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	ids := []int64{}
	for i := range 30 {
		ids = append(ids, f.add(f.now.Add(time.Duration(i)*time.Second), i%2 == 0, func(m *store.Message) {
			m.Snippet = sql.NullString{String: strings.Repeat("message ", 30), Valid: true}
		}))
	}
	f.project()
	req := anchorRequest(messageRef(ids[15]))
	req.Conversation = true
	req.Before, req.After = 15, 14
	req.Budget = 2048
	seen := map[string]bool{}
	for page := range 40 {
		got, err := Build(t.Context(), f.src, req)
		require.NoError(err)
		for _, e := range got.History {
			assert.Equal("message", e.Kind)
			assert.True(strings.HasPrefix(e.Key, "msg:"))
			assert.False(seen[e.Key], "duplicate %s", e.Key)
			seen[e.Key] = true
		}
		if got.Cursor == "" {
			break
		}
		req.Cursor = got.Cursor
		require.Less(page, 39)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[fmt.Sprintf("msg:%d", id)] = true
	}
	assert.Equal(want, seen)
}
