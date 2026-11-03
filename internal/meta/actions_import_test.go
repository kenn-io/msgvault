package meta

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionsDeleteSurvivesPausedImportAndGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	fixture, client := newGraphFixture(t)
	fixture.comments["100_200"] = []Object{
		*comment("100_200_299", "100_200", "another comment"),
		*comment("100_200_300", "100_200", "deleted snapshot needle"),
	}
	fixture.comments["100_200_300"] = []Object{*comment("100_200_301", "100_200_300", "deleted subtree needle")}
	importer := NewImporter(st, client)
	paused, err := importer.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	assert.True(paused.Partial)
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	var saved syncState
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &saved))
	require.NotEmpty(saved.Phases[Posts].Stack)
	require.NotEmpty(saved.Phases[Posts].Stack[0].Pending)
	assert.Equal("100_200_300", saved.Phases[Posts].Stack[0].Pending[0].ID)
	_, err = actionRun(t, actions, id, ActionDelete)
	require.NoError(err)
	plan, err := st.PlanGCContext(t.Context())
	require.NoError(err)
	deleted, err := st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Equal(int64(1), deleted)
	fixture.comments["100_200"] = fixture.comments["100_200"][:1]
	resumed, err := importer.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(resumed.Partial)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_200_299", "100_200_300", "100_200_301"})
	require.NoError(err)
	assert.NotZero(ids["100_200_299"])
	assert.Zero(ids["100_200_300"], "buffered provider data must not recreate our deleted comment")
	assert.Zero(ids["100_200_301"], "a deleted traversal frame must not recreate its descendants")
	var matches int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, "deleted snapshot needle").Scan(&matches))
	assert.Zero(matches)
	assert.Equal(1, graph.count())
}

func TestActionsDeleteAncestorWhilePausedInsideDescendant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, _ := actionFixture(t)
	fixture, client := newGraphFixture(t)
	fixture.posts = append(fixture.posts, Object{ID: "100_400", Message: "Later post"})
	fixture.comments["100_200"] = []Object{*comment("100_200_300", "100_200", "Ancestor")}
	fixture.comments["100_200_300"] = []Object{*comment("100_200_301", "100_200_300", "Descendant")}
	fixture.comments["100_200_301"] = []Object{*comment("100_200_302", "100_200_301", "Unseen descendant")}
	fixture.comments["100_400"] = []Object{*comment("100_400_401", "100_400", "Later comment")}
	importer := NewImporter(st, client)
	paused, err := importer.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
	require.NoError(err)
	require.True(paused.Partial)
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	var saved syncState
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &saved))
	require.Len(saved.Phases[Posts].Stack, 3)
	assert.Equal("100_200_301", saved.Phases[Posts].Stack[2].Parent)
	_, err = actionRun(t, actions, id, ActionDelete)
	require.NoError(err)
	fixture.denied["100_200_300/comments"] = true
	fixture.denied["100_200_301/comments"] = true
	resumed, err := importer.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "a deleted ancestor must discard the whole paused subtree before any reads")
	assert.False(resumed.Partial)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_200_302", "100_400_401"})
	require.NoError(err)
	assert.Zero(ids["100_200_302"])
	assert.NotZero(ids["100_400_401"], "later posts must keep progressing")
}
