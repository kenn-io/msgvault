package store_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationGTDReopenAndSubset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	path := filepath.Join(root, "source.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	source, err := st.GetOrCreateSource("beeper", "synthetic-subset")
	require.NoError(err)
	id, err := st.EnsureConversationWithType(source.ID, "!subset:example.test", "direct_chat", "Subset")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: id, SourceMessageID: "fixture", MessageType: "beeper"})
	require.NoError(err)
	head := jsontext.Value(`{"cursor":"opaque","message_id":"fixture","timestamp":"2026-11-01T00:00:00Z","message_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	state, err := st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: "delegated", ObservedActivity: "2026-11-01T00:00:00Z", ContextStamp: "reviewed-packet", ReviewedHead: head})
	require.NoError(err)
	require.NoError(st.SetConversationGTDHoldContext(t.Context(), id, state.Revision, true))
	require.NoError(st.SetConversationMemberCountContext(t.Context(), id, 3))
	dest := filepath.Join(root, "subset")
	_, err = store.CopySubset(path, dest, 1, false)
	require.NoError(err)
	for _, db := range []string{path, filepath.Join(dest, "msgvault.db")} {
		reopened, err := store.OpenForTest(db)
		require.NoError(err)
		state, err := reopened.GetConversationGTDContext(t.Context(), id)
		require.NoError(err)
		assert.Equal("delegated", state.Category)
		assert.True(state.ArchiveHold)
		assert.Equal("2026-11-01T00:00:00Z", state.ObservedActivity)
		assert.Equal("reviewed-packet", state.ContextStamp)
		assert.Equal(head, state.ReviewedHead)
		require.NoError(reopened.Close())
	}
}

// Losing the revision/hold guard would allow an unattended run to overwrite
// a newer user decision or repeat an archive whose outcome is unknown.
func TestConversationGTDRevisionAndHold(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-signal")
	require.NoError(err)
	id, err := st.EnsureConversationWithType(src.ID, "!gtd:example.test", "direct_chat", "Synthetic")
	require.NoError(err)
	ctx := t.Context()
	before, err := st.GetConversationGTDContext(ctx, id)
	require.NoError(err)
	assert.Zero(before.Revision)
	revision := int64(0)
	saved, err := st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Category: "todo", ExpectedRevision: &revision})
	require.NoError(err)
	assert.True(saved.NeedsAnswer)
	assert.Equal(int64(1), saved.Revision)
	wire, err := json.Marshal(saved)
	require.NoError(err)
	assert.NotContains(string(wire), `"reviewed_head"`)
	_, err = st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Category: "reference", ExpectedRevision: &revision})
	require.ErrorIs(err, store.ErrConversationGTDRevision)
	no, yes := false, true
	for _, change := range []struct {
		category string
		override *bool
		want     bool
	}{
		{"waiting", nil, true},
		{"waiting", &no, false},
		{"todo", nil, false},
		{"reference", &yes, true},
	} {
		revision = saved.Revision
		saved, err = st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Category: change.category, NeedsAnswer: change.override, ExpectedRevision: &revision})
		require.NoError(err)
		assert.Equal(change.want, saved.NeedsAnswer, "category %s must preserve an omitted answer flag", change.category)
	}
	require.NoError(st.SetConversationGTDHoldContext(ctx, id, saved.Revision, true))
	revision = saved.Revision
	saved, err = st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Clear: true, ExpectedRevision: &revision})
	require.NoError(err)
	assert.Empty(saved.Category)
	assert.True(saved.ArchiveHold)
	assert.False(saved.NeedsAnswer)
	wire, err = json.Marshal(saved)
	require.NoError(err)
	assert.NotContains(string(wire), `"reviewed_head"`)
	revision = saved.Revision
	saved, err = st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Category: "todo", ExpectedRevision: &revision})
	require.NoError(err)
	assert.True(saved.NeedsAnswer, "a cleared classification uses the first-time todo default")
	assert.True(saved.ArchiveHold)
	revision = saved.Revision
	saved, err = st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Category: "reference", NeedsAnswer: &yes, ExpectedRevision: &revision})
	require.NoError(err)
	rows, err := st.ListConversationGTDContext(ctx, src.ID, 0, 200, true)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("reference", rows[0].Category)
	assert.True(rows[0].ArchiveHold)
	require.ErrorIs(st.SetConversationGTDHoldContext(ctx, id, 1, false), store.ErrConversationGTDRevision)
	require.NoError(st.SetConversationGTDHoldContext(ctx, id, saved.Revision, false))
	_, err = st.SetConversationGTDContext(ctx, id, store.ConversationGTDChange{Category: "made-up"})
	require.Error(err)
}

func TestConversationGTDPagesAndIndependentAnswer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-telegram")
	require.NoError(err)
	for _, category := range []string{"todo", "waiting", "delegated", "reference"} {
		id, err := st.EnsureConversation(src.ID, category, category)
		require.NoError(err)
		yes := true
		_, err = st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: category, NeedsAnswer: &yes})
		require.NoError(err)
	}
	page, err := st.ListConversationGTDContext(t.Context(), src.ID, 0, 2, true)
	require.NoError(err)
	require.Len(page, 2)
	next, err := st.ListConversationGTDContext(t.Context(), src.ID, page[1].ConversationID, 2, true)
	require.NoError(err)
	require.Len(next, 2)
	assert.Equal("reference", next[1].Category)
	ceiling, err := st.BeeperConversationCeilingContext(t.Context(), src.ID)
	require.NoError(err)
	assert.Equal(next[1].ConversationID, ceiling)
	identity, err := st.BeeperConversationIDsContext(t.Context(), src.ID, []string{"todo"})
	require.NoError(err)
	assert.Equal(map[string]int64{"todo": page[0].ConversationID}, identity, "the batch callback must include its already-current first row")
	unanswered, err := st.EnsureConversation(src.ID, "unanswered", "Unanswered reference")
	require.NoError(err)
	no := false
	_, err = st.SetConversationGTDContext(t.Context(), unanswered, store.ConversationGTDChange{Category: "reference", NeedsAnswer: &no})
	require.NoError(err)
	filtered, err := st.ListConversationGTDContext(t.Context(), src.ID, 0, 200, true)
	require.NoError(err)
	assert.Len(filtered, 4, "answer-needed must exclude the false row")
	all, err := st.ListConversationGTDContext(t.Context(), src.ID, 0, 200, false)
	require.NoError(err)
	assert.Len(all, 5)
	other, err := st.GetOrCreateSource("beeper", "synthetic-other")
	require.NoError(err)
	scoped := sourcescope.WithSourceIDs(t.Context(), []int64{other.ID})
	_, err = st.GetConversationGTDContext(scoped, page[0].ConversationID)
	require.Error(err)
	_, err = st.SetConversationGTDContext(scoped, page[0].ConversationID, store.ConversationGTDChange{Category: "todo"})
	require.Error(err)
	err = st.SetConversationGTDHoldContext(scoped, page[0].ConversationID, page[0].Revision, true)
	require.ErrorIs(err, store.ErrConversationGTDRevision)
	denied, err := st.ListConversationGTDContext(scoped, src.ID, 0, 200, false)
	require.NoError(err)
	assert.Empty(denied)
	ceiling, err = st.BeeperConversationCeilingContext(scoped, src.ID)
	require.NoError(err)
	assert.Zero(ceiling)
	lookup, err := st.BeeperConversationIDsContext(scoped, src.ID, []string{"todo"})
	require.NoError(err)
	assert.Empty(lookup)
	foreign, err := st.GetOrCreateSource("gmail", "synthetic@example.com")
	require.NoError(err)
	id, err := st.EnsureConversation(foreign.ID, "email", "Email")
	require.NoError(err)
	_, err = st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: "todo"})
	require.Error(err)
}

// Clearing a conversation without GTD state must not consume the first revision
// or make a following guarded first classification fail.
func TestConversationGTDClearAbsentKeepsRevisionZero(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-clear-absent")
	require.NoError(err)
	id, err := st.EnsureConversation(source.ID, "!never-classified:example.test", "Unclassified fixture")
	require.NoError(err)
	before, err := st.GetConversationGTDContext(t.Context(), id)
	require.NoError(err)
	require.Zero(before.Revision)
	zero, wrong := int64(0), int64(1)
	_, err = st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Clear: true, ExpectedRevision: &wrong})
	require.ErrorIs(err, store.ErrConversationGTDRevision)
	for range 2 {
		cleared, err := st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Clear: true, ExpectedRevision: &zero})
		require.NoError(err)
		assert.Equal(before, cleared, "an absent clear must not materialize classification or a revision")
		current, err := st.GetConversationGTDContext(t.Context(), id)
		require.NoError(err)
		assert.Equal(before, current, "the durable absence must survive a subsequent read")
	}
	first, err := st.SetConversationGTDContext(t.Context(), id, store.ConversationGTDChange{Category: "todo", ExpectedRevision: &zero})
	require.NoError(err, "an absent clear must not invalidate the first classification guard")
	assert.Equal(int64(1), first.Revision)
	assert.True(first.NeedsAnswer)
}

func TestConversationGTDMatchingMergeSurvival(t *testing.T) {
	for _, category := range []string{"unmatched", "", "todo", "reference"} {
		t.Run("destination category "+category, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("beeper", "synthetic-merge-history")
			require.NoError(err)
			into, err := st.GetOrCreateSource("beeper", "synthetic-merge-live")
			require.NoError(err)
			incoming, err := st.EnsureConversationWithType(from.ID, "!shared:example.test", "direct_chat", "Before refresh")
			require.NoError(err)
			canonical := incoming
			if category != "unmatched" {
				canonical, err = st.EnsureConversation(into.ID, "!shared:example.test", "Canonical fixture")
				require.NoError(err)
			}
			no := false
			if category != "" && category != "unmatched" {
				_, err = st.SetConversationGTDContext(t.Context(), canonical, store.ConversationGTDChange{Category: category, NeedsAnswer: &no})
				require.NoError(err)
			}
			saved, err := st.SetConversationGTDContext(t.Context(), incoming, store.ConversationGTDChange{Category: "todo", ObservedActivity: "2026-11-01T00:00:00Z", ContextStamp: "prior-source-review"})
			require.NoError(err)
			require.NoError(st.SetConversationGTDHoldContext(t.Context(), incoming, saved.Revision, true))
			refreshed, err := st.EnsureConversationWithType(from.ID, "!shared:example.test", "direct_chat", "After refresh")
			require.NoError(err)
			assert.Equal(incoming, refreshed)
			_, err = st.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: incoming, SourceMessageID: "new-fixture-event", MessageType: "beeper"})
			require.NoError(err)
			beforeSource, err := st.GetConversationGTDContext(t.Context(), incoming)
			require.NoError(err)
			beforeTarget, err := st.GetConversationGTDContext(t.Context(), canonical)
			require.NoError(err)
			_, previewErr := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
			if category == "reference" {
				require.ErrorIs(previewErr, store.ErrSourceMergeInvalid, "preview must reject the same category conflict as apply")
			} else {
				require.NoError(previewErr)
			}
			afterPreviewSource, readErr := st.GetConversationGTDContext(t.Context(), incoming)
			require.NoError(readErr)
			afterPreviewTarget, readErr := st.GetConversationGTDContext(t.Context(), canonical)
			require.NoError(readErr)
			assert.Equal(beforeSource, afterPreviewSource, "preview must not change classification, hold or review evidence")
			assert.Equal(beforeTarget, afterPreviewTarget)
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			if category == "reference" {
				left, readErr := st.GetConversationGTDContext(t.Context(), incoming)
				require.NoError(readErr)
				right, readErr := st.GetConversationGTDContext(t.Context(), canonical)
				require.NoError(readErr)
				assert.Equal(beforeSource, left, "conflict must roll back the source merge")
				assert.Equal(beforeTarget, right)
				unchanged, sourceErr := st.GetSourceByID(from.ID)
				require.NoError(sourceErr)
				assert.Zero(unchanged.MergedIntoSourceID, "conflict must leave the source active")
				require.ErrorIs(err, store.ErrSourceMergeInvalid, "conflicting user categories must be resolved before consolidating their conversations")
				return
			}
			require.NoError(err)
			current, err := st.GetConversationGTDContext(t.Context(), canonical)
			require.NoError(err)
			assert.Equal("todo", current.Category)
			assert.True(current.NeedsAnswer, "the surviving chat must remain in the answer-needed group")
			assert.True(current.ArchiveHold, "source uncertainty cannot be lost during consolidation")
			if category == "unmatched" {
				assert.Equal(saved.Category, current.Category)
				assert.Equal(saved.NeedsAnswer, current.NeedsAnswer)
				assert.Equal(saved.Revision, current.Revision)
			} else {
				assert.Greater(current.Revision, max(beforeSource.Revision, beforeTarget.Revision))
				assert.Empty(current.ContextStamp, "review evidence belongs to its original source identity")
				assert.Empty(current.ObservedActivity)
				assert.Empty(current.ReviewedHead)
			}
			rows, err := st.ListConversationGTDContext(t.Context(), into.ID, 0, 50, true)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(canonical, rows[0].ConversationID)
		})
	}
	t.Run("prior merge and live import share a planned destination", func(t *testing.T) {
		assert := assert.New(t)
		requirements := require.New(t)
		st := testutil.NewTestStore(t)
		prior, err := st.GetOrCreateSource("beeper", "synthetic-plan-history")
		requirements.NoError(err)
		from, err := st.GetOrCreateSource("beeper", "synthetic-plan-source")
		requirements.NoError(err)
		into, err := st.GetOrCreateSource("beeper", "synthetic-plan-destination")
		requirements.NoError(err)
		const chat = "!shared-plan:example.test"
		archived, err := st.EnsureConversationWithType(prior.ID, chat, "direct_chat", "Prior archive")
		requirements.NoError(err)
		_, err = st.SetConversationGTDContext(t.Context(), archived, store.ConversationGTDChange{Category: "todo"})
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: prior.ID, IntoSourceID: from.ID})
		requirements.NoError(err)
		oldRow, err := st.GetConversationGTDContext(t.Context(), archived)
		requirements.NoError(err)
		assert.Equal(from.ID, oldRow.SourceID)
		assert.NotEqual(chat, oldRow.ChatID, "a prior merge keeps the original provider identity in its provenance")
		live, err := st.EnsureConversationWithType(from.ID, chat, "direct_chat", "Fresh live import")
		requirements.NoError(err)
		requirements.NotEqual(archived, live, "the importer can create a live row alongside the archived identity")
		canonical, err := st.EnsureConversationWithType(into.ID, chat, "direct_chat", "Destination")
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
		requirements.NoError(err, "the same complete plan is valid before adding a conflicting classification")
		_, err = st.SetConversationGTDContext(t.Context(), live, store.ConversationGTDChange{Category: "reference"})
		requirements.NoError(err)
		before := map[int64]store.ConversationGTD{}
		for _, id := range []int64{archived, live, canonical} {
			row, readErr := st.GetConversationGTDContext(t.Context(), id)
			requirements.NoError(readErr)
			before[id] = row
		}
		for _, mode := range []struct {
			name string
			dry  bool
		}{{"preview", true}, {"apply", false}} {
			t.Run(mode.name, func(t *testing.T) {
				require := require.New(t)
				_, mergeErr := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: mode.dry})
				require.ErrorIs(mergeErr, store.ErrSourceMergeInvalid, "the complete plan must account for all inputs to the shared destination")
			})
			for id, row := range before {
				current, readErr := st.GetConversationGTDContext(t.Context(), id)
				requirements.NoError(readErr)
				assert.Equal(row, current, "neither preview nor refused apply may alter any classification")
			}
			source, readErr := st.GetSourceByID(from.ID)
			requirements.NoError(readErr)
			assert.Zero(source.MergedIntoSourceID)
		}
		resolved, err := st.SetConversationGTDContext(t.Context(), live, store.ConversationGTDChange{Category: "todo"})
		requirements.NoError(err)
		assert.False(resolved.NeedsAnswer, "recategorization preserves the independent answer flag")
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
		requirements.NoError(err, "equal categories can share a planned destination")
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
		requirements.NoError(err)
		current, err := st.GetConversationGTDContext(t.Context(), canonical)
		requirements.NoError(err)
		assert.Equal("todo", current.Category)
		assert.True(current.NeedsAnswer, "consolidation preserves either input's answer-needed state")
		assert.Greater(current.Revision, max(oldRow.Revision, resolved.Revision))
	})
}
