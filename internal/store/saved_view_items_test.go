package store_test

import (
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
	updated, err := st.ChangeSavedViewItems(ctx, view.ID, view.Revision, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref, ref}, AddedBy: "owner"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	assert.Equal(t, &store.SavedViewItemsApplied{IncludeAdded: 1}, updated.Applied)
	_, err = st.ChangeSavedViewItems(ctx, view.ID, 1, store.SavedViewItemChange{ExcludeAdd: []store.SavedViewItemRef{ref}})
	require.ErrorIs(t, err, store.ErrSavedViewRevisionConflict)
	updated, err = st.ChangeSavedViewItems(ctx, view.ID, updated.Revision, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref}})
	require.NoError(t, err)
	assert.Equal(t, &store.SavedViewItemsApplied{}, updated.Applied)
	var raw string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&raw))
	updated, err = st.UpdateSavedView(ctx, view.ID, updated.Revision, savedViewInput("Renamed"))
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
	assert.Equal(t, &store.SavedViewItemsApplied{IncludeRemoved: 1, ExcludeAdded: 1}, updated.Applied)
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
	id := f.CreateMessage("numeric-fallback")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_message_id='' WHERE id=?`), id)
	require.NoError(t, err)
	providerID := f.CreateMessage("message:one")
	providerKey := fmt.Sprintf("source:%d:message:message:one", f.Source.ID)
	numeric := fmt.Sprintf("source:%d:message:%d", f.Source.ID, id)
	emailConversation := fmt.Sprintf("source:%d:conversation:%d", f.Source.ID, f.ConvID)
	chatSource, err := f.Store.GetOrCreateSource("whatsapp", "synthetic-chat-account")
	require.NoError(t, err)
	chatID, err := f.Store.EnsureConversationWithType(chatSource.ID, "chat", "group_chat", "Synthetic chat")
	require.NoError(t, err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: chatSource.ID, ConversationID: chatID, SourceMessageID: "chat-message", MessageType: "whatsapp"})
	require.NoError(t, err)
	conversation := fmt.Sprintf("source:%d:conversation:%d", chatSource.ID, chatID)
	resolved, rejected, err := f.Store.ResolveEntryKeys(t.Context(), []string{providerKey, numeric, conversation, emailConversation, "source:999999:message:missing", "wrong", fmt.Sprintf("source:%d:message:chat-message", chatSource.ID)})
	require.NoError(t, err)
	assert.Equal(t, []store.EntryKeyRejection{{Key: emailConversation, Reason: "not_found"}, {Key: "source:999999:message:missing", Reason: "not_found"}, {Key: "wrong", Reason: "invalid_entry_key"}, {Key: fmt.Sprintf("source:%d:message:chat-message", chatSource.ID), Reason: "use_conversation_key"}}, rejected)
	assert.Equal(t, providerID, resolved[providerKey].Ref.MessageID)
	assert.Equal(t, store.SavedViewItemRef{MessageID: id, SourceID: f.Source.ID}, resolved[numeric].Ref)
	assert.Equal(t, store.SavedViewItemRef{ConversationID: chatID, SourceID: chatSource.ID}, resolved[conversation].Ref)
}

func TestMergeDuplicatesRemapsSavedViewItems(t *testing.T) {
	t.Run("include", func(t *testing.T) {
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
	})
	t.Run("exclude wins", func(t *testing.T) {
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
	})
}

func TestChangeSavedViewItemsBoundsRollback(t *testing.T) {
	f := storetest.New(t)
	f.CreateMessage("bounded")
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
}

func TestMergeSourcesSavedViewItemsFoldedCollisions(t *testing.T) {
	t.Run("moved", func(t *testing.T) {
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
	})
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
	got, err := f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), got.Revision)
	require.NoError(t, f.Store.RemoveSource(bSource.ID))
	var raw *string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&raw))
	assert.Nil(t, raw)
	got, err = f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(4), got.Revision)
}

func TestSavedViewItemsStoredSizeAndJSONBWhitespace(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("near-bound")
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Size bound"))
	require.NoError(t, err)
	// Leave only a few bytes below the canonical serialized bound. PostgreSQL
	// adds insignificant JSONB output whitespace; it must not shrink this limit.
	raw := fmt.Sprintf(`{"version":1,"include":[{"message_id":%d,"source_id":%d,"added_by":""}]}`, id, f.Source.ID)
	raw = strings.Replace(raw, `"added_by":""`, `"added_by":"`+strings.Repeat("a", (128<<10)-len(raw)-2)+`"`, 1)
	_, err = store.ValidateSavedViewItemsJSON([]byte(raw))
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE saved_views SET items_json=? WHERE id=?`), raw, view.ID)
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	require.Len(t, items.Include, 1)
	assert.Equal(t, id, items.Include[0].MessageID)
	oversized := strings.Replace(raw, `"added_by":"`, `"added_by":"oversized`, 1)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE saved_views SET items_json=? WHERE id=?`), oversized, view.ID)
	require.NoError(t, err)
	_, err = f.Store.GetSavedViewItems(t.Context(), view.ID)
	assert.ErrorIs(t, err, store.ErrSavedViewItemsInvalid)
	assert.NotErrorIs(t, err, store.ErrSavedViewItemsLimit)
	// Cleanup may not abandon a corrupt row and leave dangling membership.
	require.Error(t, f.Store.RemoveSource(f.Source.ID))
	var remaining int
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM sources WHERE id=?`), f.Source.ID).Scan(&remaining))
	assert.Equal(t, 1, remaining)
}
func TestSavedViewItemsIgnoreStaleTargets(t *testing.T) {
	for _, kind := range []string{"missing message", "reused message", "purged conversation"} {
		t.Run(kind, func(t *testing.T) {
			f := storetest.New(t)
			id := f.CreateMessage("stale")
			ref := store.SavedViewItemRef{MessageID: id, SourceID: f.Source.ID}
			var purgeSourceID int64
			if kind == "purged conversation" {
				source, err := f.Store.GetOrCreateSource("discord", "synthetic-guild")
				require.NoError(t, err)
				conv, err := f.Store.EnsureConversationWithType(source.ID, "synthetic-channel", "group_chat", "Synthetic channel")
				require.NoError(t, err)
				_, err = f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "channel-message", MessageType: "discord"})
				require.NoError(t, err)
				ref = store.SavedViewItemRef{ConversationID: conv, SourceID: source.ID}
				purgeSourceID = source.ID
			}
			view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Stale items"))
			require.NoError(t, err)
			_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref}})
			require.NoError(t, err)
			if kind == "purged conversation" {
				require.NoError(t, f.Store.PurgeChannelContext(t.Context(), purgeSourceID, "synthetic-channel"))
			} else {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM messages WHERE id=?`), id)
				require.NoError(t, err)
				if kind == "reused message" {
					other, err := f.Store.GetOrCreateSource("gmail", "replacement@example.test")
					require.NoError(t, err)
					conv, err := f.Store.EnsureConversation(other.ID, "replacement", "Replacement")
					require.NoError(t, err)
					override := ""
					if f.Store.IsPostgreSQL() {
						override = " OVERRIDING SYSTEM VALUE"
					}
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type)`+override+` VALUES (?,?,?,?,?)`), id, other.ID, conv, "replacement", "email")
					require.NoError(t, err)
				}
			}
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
		})
	}
}
