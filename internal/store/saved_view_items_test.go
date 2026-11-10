package store_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSavedViewItemsCASAndCanonicalPreservation(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	ctx := t.Context()
	source := f.Source
	id := f.CreateMessage("item-one")
	view, err := st.CreateSavedView(ctx, savedViewInput("Items"))
	require.NoError(t, err)
	ref := store.SavedViewItemRef{MessageID: id, SourceID: source.ID}
	updated, err := st.ChangeSavedViewItems(ctx, view.ID, view.Revision, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref}, AddedBy: "owner"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	_, err = st.ChangeSavedViewItems(ctx, view.ID, 1, store.SavedViewItemChange{ExcludeAdd: []store.SavedViewItemRef{ref}})
	require.ErrorIs(t, err, store.ErrSavedViewRevisionConflict)
	var raw string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&raw))
	updated, err = st.UpdateSavedView(ctx, view.ID, 2, savedViewInput("Renamed"))
	require.NoError(t, err)
	var after string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&after))
	assert.Equal(t, raw, after)
	updated, err = st.ChangeSavedViewItems(ctx, view.ID, updated.Revision, store.SavedViewItemChange{ExcludeAdd: []store.SavedViewItemRef{ref}, AddedBy: "agent:triage"})
	require.NoError(t, err)
	items, err := st.GetSavedViewItems(ctx, view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Include)
	require.Len(t, items.Exclude, 1)
	assert.Equal(t, "agent:triage", items.Exclude[0].AddedBy)
	_, err = st.ChangeSavedViewItems(ctx, view.ID, updated.Revision, store.SavedViewItemChange{ExcludeRemove: []store.SavedViewItemRef{ref}})
	require.NoError(t, err)
	var empty *string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&empty))
	assert.Nil(t, empty)
}

func TestValidateSavedViewItemsJSONRejects(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"version":2}`, `{"version":1,"other":1}`, `{"version":1,"include":[{"message_id":1,"conversation_id":2,"source_id":1}]}`, `{"version":1,"include":[{"message_id":1,"source_id":1}],"exclude":[{"message_id":1,"source_id":1}]}`} {
		t.Run(raw, func(t *testing.T) { _, err := store.ValidateSavedViewItemsJSON([]byte(raw)); require.Error(t, err) })
	}
	_, err := store.ValidateSavedViewItemsJSON([]byte(`{"version":1,"include":[{"message_id":1,"source_id":1,"added_by":"` + strings.Repeat("a", 128<<10) + `"}]}`))
	assert.ErrorIs(t, err, store.ErrSavedViewItemsLimit)
	entries := make([]string, 501)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"message_id":%d,"source_id":1}`, i+1)
	}
	_, err = store.ValidateSavedViewItemsJSON([]byte(`{"version":1,"include":[` + strings.Join(entries, ",") + `]}`))
	assert.ErrorIs(t, err, store.ErrSavedViewItemsLimit)
}

func TestResolveEntryKeys(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("message:one")
	key := fmt.Sprintf("source:%d:message:message:one", f.Source.ID)
	resolved, rejected, err := f.Store.ResolveEntryKeys(t.Context(), []string{key, "source:1:message:missing", "wrong"})
	require.NoError(t, err)
	assert.Equal(t, id, resolved[key].Ref.MessageID)
	require.Len(t, rejected, 2)
	assert.Equal(t, "not_found", rejected[0].Reason)
	assert.Equal(t, "invalid_entry_key", rejected[1].Reason)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='whatsapp' WHERE id=?`), id)
	require.NoError(t, err)
	_, rejected, err = f.Store.ResolveEntryKeys(t.Context(), []string{key})
	require.NoError(t, err)
	require.Len(t, rejected, 1)
	assert.Equal(t, "use_conversation_key", rejected[0].Reason)
}

func TestMergeDuplicatesRemapsSavedViewItems(t *testing.T) {
	f := storetest.New(t)
	keep := f.CreateMessage("keep")
	drop := f.CreateMessage("drop")
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Dedup items"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: drop, SourceID: f.Source.ID}}, AddedBy: "owner"})
	require.NoError(t, err)
	_, err = f.Store.MergeDuplicates(keep, []int64{drop}, "items-batch")
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	require.Len(t, items.Include, 1)
	assert.Equal(t, keep, items.Include[0].MessageID)
}
func TestSavedViewItemsIgnorePurgedMessageAndPruneSource(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("purged")
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Stale items"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: id, SourceID: f.Source.ID}}, AddedBy: "owner"})
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM messages WHERE id=?`), id)
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Include)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 2, store.SavedViewItemChange{})
	require.NoError(t, err)
	id = f.CreateMessage("remove-source")
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 3, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: id, SourceID: f.Source.ID}}, AddedBy: "owner"})
	require.NoError(t, err)
	require.NoError(t, f.Store.RemoveSource(f.Source.ID))
	var raw *string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&raw))
	assert.Nil(t, raw)
	got, err := f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(5), got.Revision)
}

func TestMergeSourcesRemapsSavedViewItems(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("move")
	into, err := f.Store.GetOrCreateSource("gmail", "destination@example.com")
	require.NoError(t, err)
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Moved items"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: id, SourceID: f.Source.ID}}, AddedBy: "owner"})
	require.NoError(t, err)
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: f.Source.ID, IntoSourceID: into.ID})
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	require.Len(t, items.Include, 1)
	assert.Equal(t, into.ID, items.Include[0].SourceID)
}

func TestChangeSavedViewItemsBoundsRollback(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("bounded")
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Bounded items"))
	require.NoError(t, err)
	refs := make([]store.SavedViewItemRef, 501)
	for i := range refs {
		refs[i] = store.SavedViewItemRef{MessageID: int64(i + 1), SourceID: f.Source.ID}
	}
	for _, change := range []store.SavedViewItemChange{{IncludeAdd: refs}, {IncludeAdd: refs[:1], AddedBy: strings.Repeat("x", 128<<10)}} {
		_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, change)
		require.ErrorIs(t, err, store.ErrSavedViewItemsLimit)
		got, err := f.Store.GetSavedView(t.Context(), view.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), got.Revision)
		assert.Empty(t, got.ItemsJSON)
	}
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: id, SourceID: f.Source.ID}}})
	require.NoError(t, err)
	other, err := f.Store.GetOrCreateSource("gmail", "reused@example.com")
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM messages WHERE id=?`), id)
	require.NoError(t, err)
	otherConv, err := f.Store.EnsureConversation(other.ID, "replacement", "Replacement")
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type) VALUES (?,?,?,?,?)`), id, other.ID, otherConv, "replacement", "email")
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Include)
	got, err := f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	rendered, stale, err := f.Store.SavedViewItemsForView(t.Context(), got)
	require.NoError(t, err)
	assert.Empty(t, rendered.Include)
	assert.Equal(t, 1, stale)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 2, store.SavedViewItemChange{})
	require.NoError(t, err)
	got, err = f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ItemsJSON)
}
func TestMergeDuplicatesSavedViewItemsExcludeWins(t *testing.T) {
	f := storetest.New(t)
	keep := f.CreateMessage("collision-keep")
	drop := f.CreateMessage("collision-drop")
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Collisions"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: drop, SourceID: f.Source.ID}}, ExcludeAdd: []store.SavedViewItemRef{{MessageID: keep, SourceID: f.Source.ID}}})
	require.NoError(t, err)
	untouched, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Untouched"))
	require.NoError(t, err)
	_, err = f.Store.MergeDuplicates(keep, []int64{drop}, "collision-batch")
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Include)
	require.Len(t, items.Exclude, 1)
	assert.Equal(t, keep, items.Exclude[0].MessageID)
	got, err := f.Store.GetSavedView(t.Context(), untouched.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Revision)
}
func FuzzSavedViewItemsCodec(f *testing.F) {
	for _, raw := range []string{`{"version":1}`, `{"version":1,"include":[{"message_id":1,"source_id":2,"added_by":"owner"}]}`, `{}`, `null`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		items, err := store.ValidateSavedViewItemsJSON([]byte(raw))
		if err != nil {
			return
		}
		encoded, err := json.Marshal(items)
		require.NoError(t, err)
		roundTrip, err := store.ValidateSavedViewItemsJSON(encoded)
		require.NoError(t, err)
		assert.Equal(t, items, roundTrip)
	})
}

func TestMergeSourcesSavedViewItemsFoldedCollisions(t *testing.T) {
	for _, kind := range []string{"email", "conversation"} {
		t.Run(kind, func(t *testing.T) {
			f := storetest.New(t)
			from := f.Source
			sourceType := "gmail"
			if kind == "conversation" {
				sourceType = "beeper"
				var err error
				from, err = f.Store.GetOrCreateSource(sourceType, "chat-history@example.com")
				require.NoError(t, err)
			}
			into, err := f.Store.GetOrCreateSource(sourceType, "folded@example.com")
			require.NoError(t, err)
			var historical, survivor store.SavedViewItemRef
			if kind == "email" {
				drop := emailMergeFixtureMessage(t, f.Store, from.ID, "historical", []string{"recipient@example.test"}, true)
				keep := emailMergeFixtureMessage(t, f.Store, into.ID, "survivor", []string{"recipient@example.test"}, true)
				historical = store.SavedViewItemRef{MessageID: drop, SourceID: from.ID}
				survivor = store.SavedViewItemRef{MessageID: keep, SourceID: into.ID}
			} else {
				drop, err := f.Store.EnsureConversationWithType(from.ID, "!shared:example.test", "group_chat", "Chat")
				require.NoError(t, err)
				keep, err := f.Store.EnsureConversationWithType(into.ID, "!shared:example.test", "group_chat", "Chat")
				require.NoError(t, err)
				historical = store.SavedViewItemRef{ConversationID: drop, SourceID: from.ID}
				survivor = store.SavedViewItemRef{ConversationID: keep, SourceID: into.ID}
			}
			view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Folded items"))
			require.NoError(t, err)
			_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{historical}, ExcludeAdd: []store.SavedViewItemRef{survivor}})
			require.NoError(t, err)
			_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			require.NoError(t, err)
			items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
			require.NoError(t, err)
			assert.Empty(t, items.Include)
			require.Len(t, items.Exclude, 1)
			assert.Equal(t, survivor, items.Exclude[0].SavedViewItemRef)
			got, err := f.Store.GetSavedView(t.Context(), view.ID)
			require.NoError(t, err)
			assert.Equal(t, int64(3), got.Revision)
			rendered, stale, err := f.Store.SavedViewItemsForView(t.Context(), got)
			require.NoError(t, err)
			assert.Zero(t, stale)
			require.Len(t, rendered.Exclude, 1)
			assert.True(t, strings.HasPrefix(rendered.Exclude[0].EntryKey, fmt.Sprintf("source:%d:", into.ID)))
		})
	}
}

func TestSavedViewItemsPruneSourcePreservesOtherSource(t *testing.T) {
	f := storetest.New(t)
	a := f.CreateMessage("source-a")
	bSource, err := f.Store.GetOrCreateSource("gmail", "preserved@example.com")
	require.NoError(t, err)
	bConv, err := f.Store.EnsureConversation(bSource.ID, "b", "B")
	require.NoError(t, err)
	b, err := f.Store.UpsertMessage(&store.Message{SourceID: bSource.ID, ConversationID: bConv, SourceMessageID: "source-b", MessageType: "email"})
	require.NoError(t, err)
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Two sources"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: a, SourceID: f.Source.ID}}, ExcludeAdd: []store.SavedViewItemRef{{MessageID: b, SourceID: bSource.ID}}})
	require.NoError(t, err)
	require.NoError(t, f.Store.RemoveSource(f.Source.ID))
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Include)
	require.Len(t, items.Exclude, 1)
	assert.Equal(t, b, items.Exclude[0].MessageID)
	assert.Equal(t, bSource.ID, items.Exclude[0].SourceID)
}

func TestSavedViewItemsAppliedCountsReflectChanges(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("counted")
	ref := store.SavedViewItemRef{MessageID: id, SourceID: f.Source.ID}
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Counts"))
	require.NoError(t, err)
	view, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref, ref}})
	require.NoError(t, err)
	assert.Equal(t, &store.SavedViewItemsApplied{IncludeAdded: 1}, view.Applied)
	view, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref}})
	require.NoError(t, err)
	assert.Equal(t, &store.SavedViewItemsApplied{}, view.Applied)
	view, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, store.SavedViewItemChange{ExcludeAdd: []store.SavedViewItemRef{ref}})
	require.NoError(t, err)
	assert.Equal(t, &store.SavedViewItemsApplied{IncludeRemoved: 1, ExcludeAdded: 1}, view.Applied)
}

func TestResolveEntryKeysConversationAndNumericFallback(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("numeric-fallback")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_message_id='' WHERE id=?`), id)
	require.NoError(t, err)
	numeric := fmt.Sprintf("source:%d:message:%d", f.Source.ID, id)
	conversation := fmt.Sprintf("source:%d:conversation:%d", f.Source.ID, f.ConvID)
	resolved, rejected, err := f.Store.ResolveEntryKeys(t.Context(), []string{numeric, conversation})
	require.NoError(t, err)
	assert.Empty(t, rejected)
	assert.Equal(t, store.SavedViewItemRef{MessageID: id, SourceID: f.Source.ID}, resolved[numeric].Ref)
	assert.Equal(t, store.SavedViewItemRef{ConversationID: f.ConvID, SourceID: f.Source.ID}, resolved[conversation].Ref)
}

func TestSavedViewItemsIgnoreChannelPurgeAndViewOnlyDelete(t *testing.T) {
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("slack", "synthetic-team:synthetic-user")
	require.NoError(t, err)
	conv, err := f.Store.EnsureConversationWithType(source.ID, "synthetic-channel", "group_chat", "Synthetic channel")
	require.NoError(t, err)
	message, err := f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "channel-message", MessageType: "slack"})
	require.NoError(t, err)
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Purge items"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{ConversationID: conv, SourceID: source.ID}}})
	require.NoError(t, err)
	require.NoError(t, f.Store.PurgeChannelContext(t.Context(), source.ID, "synthetic-channel"))
	got, err := f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	items, stale, err := f.Store.SavedViewItemsForView(t.Context(), got)
	require.NoError(t, err)
	assert.Empty(t, items.Include)
	assert.Equal(t, 1, stale)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 2, store.SavedViewItemChange{})
	require.NoError(t, err)
	retained := f.CreateMessage("view-only-delete")
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 3, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: retained, SourceID: f.Source.ID}}})
	require.NoError(t, err)
	require.NoError(t, f.Store.DeleteSavedView(t.Context(), view.ID, 4))
	_, err = f.Store.GetSavedView(t.Context(), view.ID)
	assert.ErrorIs(t, err, store.ErrSavedViewNotFound)
	var remaining int
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE id=?`), retained).Scan(&remaining))
	assert.Equal(t, 1, remaining)
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE id=?`), message).Scan(&remaining))
	assert.Zero(t, remaining)
}
