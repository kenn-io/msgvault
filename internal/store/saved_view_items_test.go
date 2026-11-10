package store_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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
		unrelated := f.CreateMessage("unrelated")
		untouched, err = f.Store.ChangeSavedViewItems(t.Context(), untouched.ID, untouched.Revision, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: unrelated, SourceID: f.Source.ID}}})
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
		assert.Equal(t, untouched.Revision, got.Revision)
	})
}

func TestChangeSavedViewItemsBoundsRollback(t *testing.T) {
	f := storetest.New(t)
	refs := make([]store.SavedViewItemRef, 501)
	for i := range refs {
		refs[i] = store.SavedViewItemRef{MessageID: f.CreateMessage(fmt.Sprintf("bounded-%d", i)), SourceID: f.Source.ID}
	}
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Bounded items"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, store.SavedViewItemChange{IncludeAdd: refs[:1], AddedBy: strings.Repeat("x", 128<<10)})
	require.ErrorIs(t, err, store.ErrSavedViewItemsLimit)
	got, err := f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, view.Revision, got.Revision)
	assert.Empty(t, got.ItemsJSON, "byte-limit failure writes nothing")
	view, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, store.SavedViewItemChange{IncludeAdd: refs[:500]})
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, store.SavedViewItemChange{IncludeAdd: refs[500:]})
	require.ErrorIs(t, err, store.ErrSavedViewItemsLimit)
	got, err = f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, view.Revision, got.Revision)
	assert.Equal(t, string(view.ItemsJSON), string(got.ItemsJSON), "count-limit failure preserves the committed 500 refs")
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
				for source, conversation := range map[int64]int64{from.ID: drop, into.ID: keep} {
					_, err = f.Store.UpsertMessage(&store.Message{SourceID: source, ConversationID: conversation, SourceMessageID: "chat-message", MessageType: "beeper"})
					require.NoError(t, err)
				}
				historical = store.SavedViewItemRef{ConversationID: drop, SourceID: from.ID}
				survivor = store.SavedViewItemRef{ConversationID: keep, SourceID: into.ID}
			}
			view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Folded items"))
			require.NoError(t, err)
			_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{historical}})
			require.NoError(t, err)
			_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			require.NoError(t, err)
			items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
			require.NoError(t, err)
			assert.Empty(t, items.Exclude)
			require.Len(t, items.Include, 1)
			assert.Equal(t, survivor, items.Include[0].SavedViewItemRef)
			got, err := f.Store.GetSavedView(t.Context(), view.ID)
			require.NoError(t, err)
			assert.Equal(t, int64(3), got.Revision)
			rendered, stale, err := f.Store.SavedViewItemsForView(t.Context(), got)
			require.NoError(t, err)
			assert.Zero(t, stale)
			require.Len(t, rendered.Include, 1)
			assert.True(t, strings.HasPrefix(rendered.Include[0].EntryKey, fmt.Sprintf("source:%d:", into.ID)))
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
	// A newer format may be present after opening an archive with an older binary.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE saved_views SET items_json=? WHERE id=?`), `{"version":2}`, view.ID)
	require.NoError(t, err)
	// Cleanup may not abandon a corrupt row and leave dangling membership.
	require.Error(t, f.Store.RemoveSource(f.Source.ID))
	var remaining int
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM sources WHERE id=?`), f.Source.ID).Scan(&remaining))
	assert.Equal(t, 1, remaining)
}
func TestSavedViewItemsIgnoreStaleTargets(t *testing.T) {
	for _, kind := range []string{"missing message", "reused message", "purged conversation", "reclassified message", "reclassified conversation"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "reused message" {
				testutil.SkipIfPostgres(t, "automatic row ID reuse is SQLite-only")
			}
			f := storetest.New(t)
			id := f.CreateMessage("stale")
			ref := store.SavedViewItemRef{MessageID: id, SourceID: f.Source.ID}
			message := &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "stale", MessageType: "email"}
			if kind == "reclassified conversation" {
				message.MessageType = "google_chat"
				_, err := f.Store.UpsertMessage(message)
				require.NoError(t, err)
				ref = store.SavedViewItemRef{ConversationID: f.ConvID, SourceID: f.Source.ID}
			}
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
			// Load the membership committed after CreateSavedView.
			view, err = f.Store.GetSavedView(t.Context(), view.ID)
			require.NoError(t, err)
			before, stale, err := f.Store.SavedViewItemsForView(t.Context(), view)
			require.NoError(t, err)
			require.Len(t, before.Include, 1)
			assert.Zero(t, stale)
			if strings.HasPrefix(kind, "reclassified") {
				message.MessageType = "google_chat"
				if kind == "reclassified conversation" {
					message.MessageType = "email"
				}
				_, err = f.Store.UpsertMessage(message)
				require.NoError(t, err)
				resolved, rejected, err := f.Store.ResolveEntryKeys(t.Context(), []string{before.Include[0].EntryKey})
				require.NoError(t, err)
				assert.Empty(t, resolved)
				require.Len(t, rejected, 1)
			} else if kind == "purged conversation" {
				require.NoError(t, f.Store.PurgeChannelContext(t.Context(), purgeSourceID, "synthetic-channel"))
			} else {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM messages WHERE id=?`), id)
				require.NoError(t, err)
				if kind == "reused message" {
					other, err := f.Store.GetOrCreateSource("gmail", "replacement@example.test")
					require.NoError(t, err)
					conv, err := f.Store.EnsureConversation(other.ID, "replacement", "Replacement")
					require.NoError(t, err)
					reused, err := f.Store.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: conv, SourceMessageID: "replacement", MessageType: "email"})
					require.NoError(t, err)
					require.Equal(t, id, reused)
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
			wantStale := 1
			if kind == "purged conversation" {
				wantStale = 0 // Physical purge retired the reference atomically.
			}
			assert.Equal(t, wantStale, stale)
			_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, got.Revision, store.SavedViewItemChange{})
			require.NoError(t, err)
			got, err = f.Store.GetSavedView(t.Context(), view.ID)
			require.NoError(t, err)
			assert.Empty(t, got.ItemsJSON)
		})
	}
}

func TestSavedViewItemsTargetJSONShape(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("shape")
	chat, err := f.Store.EnsureConversationWithType(f.Source.ID, "shape-chat", "chat", "Chat")
	require.NoError(t, err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: chat, SourceMessageID: "shape-chat", MessageType: "google_chat"})
	require.NoError(t, err)
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Target shape"))
	require.NoError(t, err)
	view, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{MessageID: id, SourceID: f.Source.ID}, {ConversationID: chat, SourceID: f.Source.ID}}})
	require.NoError(t, err)
	rendered, _, err := f.Store.SavedViewItemsForView(t.Context(), view)
	require.NoError(t, err)
	wire, err := json.Marshal(rendered)
	require.NoError(t, err)
	for _, raw := range [][]byte{view.ItemsJSON, wire} {
		var document struct {
			Include []map[string]jsontext.Value `json:"include"`
		}
		require.NoError(t, json.Unmarshal(raw, &document))
		require.Len(t, document.Include, 2)
		assert.Contains(t, document.Include[0], "message_id")
		assert.NotContains(t, document.Include[0], "conversation_id")
		assert.Contains(t, document.Include[1], "conversation_id")
		assert.NotContains(t, document.Include[1], "message_id")
	}
}

func TestMergeSourcesSavedViewItemsDoNotResurrectReusedConversation(t *testing.T) {
	testutil.SkipIfPostgres(t, "automatic conversation ID reuse is SQLite-only")
	f := storetest.New(t)
	from, err := f.Store.GetOrCreateSource("discord", "historical-guild")
	require.NoError(t, err)
	old, err := f.Store.EnsureConversationWithType(from.ID, "channel", "group_chat", "Historical")
	require.NoError(t, err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: old, SourceMessageID: "historical", MessageType: "discord"})
	require.NoError(t, err)
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Historical pin"))
	require.NoError(t, err)
	_, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, 1, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{{ConversationID: old, SourceID: from.ID}}})
	require.NoError(t, err)
	require.NoError(t, f.Store.PurgeChannelContext(t.Context(), from.ID, "channel"))
	// Simulate a stale ref persisted by an older version that did not retire
	// purge targets. Remap must still verify provenance independently.
	raw, err := json.Marshal(store.SavedViewItems{Version: 1, Include: []store.SavedViewItemEntry{{SavedViewItemRef: store.SavedViewItemRef{ConversationID: old, SourceID: from.ID}, AddedBy: "owner"}}})
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE saved_views SET items_json=? WHERE id=?`), string(raw), view.ID)
	require.NoError(t, err)
	into, err := f.Store.GetOrCreateSource("discord", "replacement-guild")
	require.NoError(t, err)
	reused, err := f.Store.EnsureConversationWithType(into.ID, "unrelated", "group_chat", "Unrelated")
	require.NoError(t, err)
	require.Equal(t, old, reused)
	// Keep a real source-A map entry so an unsafe blanket source rewrite
	// could resurrect the stale pin while remapping the live conversation.
	_, err = f.Store.EnsureConversationWithType(from.ID, "retained", "group_chat", "Retained")
	require.NoError(t, err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: into.ID, ConversationID: old, SourceMessageID: "unrelated", MessageType: "discord"})
	require.NoError(t, err)
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(t, err)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Include, "a stale source-A pin cannot attach to source-B's reused conversation ID")
}

func TestMigrateSourceMessageIDRemapsSavedViewItems(t *testing.T) {
	f := storetest.New(t)
	legacyID := f.CreateMessage("legacy")
	survivorID := f.CreateMessage("scoped")
	view, err := f.Store.CreateSavedView(t.Context(), savedViewInput("Migration"))
	require.NoError(t, err)
	view, err = f.Store.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, store.SavedViewItemChange{AddedBy: "owner", IncludeAdd: []store.SavedViewItemRef{{MessageID: legacyID, SourceID: f.Source.ID}}})
	require.NoError(t, err)
	require.NoError(t, f.Store.MigrateSourceMessageID(f.Source.ID, f.ConvID, "legacy", "scoped"))
	updated, err := f.Store.GetSavedView(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Equal(t, view.Revision+1, updated.Revision)
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	assert.Empty(t, items.Exclude)
	require.Len(t, items.Include, 1)
	assert.Equal(t, store.SavedViewItemRef{MessageID: survivorID, SourceID: f.Source.ID}, items.Include[0].SavedViewItemRef)
	assert.Equal(t, "owner", items.Include[0].AddedBy)
}

func TestChangeSavedViewItemsDropsTargetsDeletedAfterResolution(t *testing.T) {
	for _, path := range []string{"gc", "purge"} {
		for _, mode := range []string{"include", "exclude"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				if path == "gc" {
					testutil.SkipIfPostgres(t, "archive GC is SQLite-only")
				}
				f := storetest.New(t)
				st := f.Store
				kept := store.SavedViewItemRef{SourceID: f.Source.ID, MessageID: f.CreateMessage("kept")}
				key := fmt.Sprintf("source:%d:message:removed", f.Source.ID)
				if path == "gc" {
					f.CreateMessage("removed")
				} else {
					source, err := st.GetOrCreateSource("discord", "resolution-race-guild")
					require.NoError(t, err)
					conv, err := st.EnsureConversationWithType(source.ID, "removed-channel", "group_chat", "Removed")
					require.NoError(t, err)
					_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "removed", MessageType: "discord"})
					require.NoError(t, err)
					key = fmt.Sprintf("source:%d:conversation:%d", source.ID, conv)
				}
				empty, err := st.CreateSavedView(t.Context(), savedViewInput("Empty during deletion"))
				require.NoError(t, err)
				assert.Empty(t, empty.ItemsJSON)
				populated, err := st.CreateSavedView(t.Context(), savedViewInput("Unrelated membership"))
				require.NoError(t, err)
				populated, err = st.ChangeSavedViewItems(t.Context(), populated.ID, populated.Revision, store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{kept}, AddedBy: "owner"})
				require.NoError(t, err)
				resolved, rejected, err := st.ResolveEntryKeys(t.Context(), []string{key})
				require.NoError(t, err)
				require.Empty(t, rejected)
				require.Contains(t, resolved, key)
				ref := resolved[key].Ref
				// Reproduce the gap between API key resolution and the item transaction.
				if path == "gc" {
					require.NoError(t, st.MarkMessageDeletedBySourceMessageID(f.Source.ID, true, "removed"))
					plan, err := st.PlanGCContext(t.Context())
					require.NoError(t, err)
					require.Equal(t, int64(1), plan.SourceDeleted)
					deleted, err := st.ExecuteGCContext(t.Context(), plan)
					require.NoError(t, err)
					require.Equal(t, int64(1), deleted)
				} else {
					require.NoError(t, st.PurgeChannelContext(t.Context(), ref.SourceID, "removed-channel"))
				}
				change := store.SavedViewItemChange{IncludeAdd: []store.SavedViewItemRef{ref}, AddedBy: "owner"}
				if mode == "exclude" {
					change = store.SavedViewItemChange{ExcludeAdd: []store.SavedViewItemRef{ref}, AddedBy: "owner"}
				}
				for _, view := range []*store.SavedView{empty, populated} {
					updated, err := st.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, change)
					require.NoError(t, err, "deletion does not change the revision of an unaffected view")
					assert.Equal(t, &store.SavedViewItemsApplied{}, updated.Applied)
					got, err := st.GetSavedView(t.Context(), view.ID)
					require.NoError(t, err)
					assert.Equal(t, string(view.ItemsJSON), string(got.ItemsJSON))
					if view.ID == empty.ID {
						var raw *string
						require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT items_json FROM saved_views WHERE id=?`), view.ID).Scan(&raw))
						assert.Nil(t, raw, "a dead addition leaves NULL membership")
					}
				}
			})
		}
	}
}

func TestSavedViewItemsRetireBeforeAutomaticSQLiteIDReuse(t *testing.T) {
	testutil.SkipIfPostgres(t, "automatic INTEGER PRIMARY KEY reuse and archive GC are SQLite behavior")
	for _, path := range []string{"gc", "purge"} {
		for _, mode := range []string{"include", "exclude"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				f := storetest.New(t)
				st := f.Store
				source := f.Source
				if path == "purge" {
					var err error
					source, err = st.GetOrCreateSource("discord", "reuse-guild")
					require.NoError(t, err)
				}
				keptConv, err := st.EnsureConversation(source.ID, "kept", "Kept")
				require.NoError(t, err)
				kept, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: keptConv, SourceMessageID: "kept", MessageType: "email"})
				require.NoError(t, err)
				conv := keptConv
				kind := "email"
				if path == "purge" {
					conv, err = st.EnsureConversationWithType(source.ID, "removed-channel", "group_chat", "Removed")
					require.NoError(t, err)
					kind = "discord"
				}
				target, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "removed", MessageType: kind})
				require.NoError(t, err)
				ref := store.SavedViewItemRef{SourceID: source.ID, MessageID: target}
				if path == "purge" {
					ref = store.SavedViewItemRef{SourceID: source.ID, ConversationID: conv}
				}
				keptRef := store.SavedViewItemRef{SourceID: source.ID, MessageID: kept}
				refs := []store.SavedViewItemRef{ref, keptRef}
				change := store.SavedViewItemChange{IncludeAdd: refs, AddedBy: "owner"}
				if mode == "exclude" {
					change = store.SavedViewItemChange{ExcludeAdd: refs, AddedBy: "owner"}
				}
				view, err := st.CreateSavedView(t.Context(), savedViewInput("Retirement"))
				require.NoError(t, err)
				_, err = st.ChangeSavedViewItems(t.Context(), view.ID, view.Revision, change)
				require.NoError(t, err)
				if path == "gc" {
					require.NoError(t, st.MarkMessageDeletedBySourceMessageID(source.ID, true, "removed"))
					plan, err := st.PlanGCContext(t.Context())
					require.NoError(t, err)
					require.Equal(t, int64(1), plan.SourceDeleted)
					deleted, err := st.ExecuteGCContext(t.Context(), plan)
					require.NoError(t, err)
					assert.Equal(t, int64(1), deleted)
				} else {
					require.NoError(t, st.PurgeChannelContext(t.Context(), source.ID, "removed-channel"))
					replacement, err := st.EnsureConversationWithType(source.ID, "replacement-channel", "group_chat", "Replacement")
					require.NoError(t, err)
					require.Equal(t, conv, replacement, "SQLite automatically reuses the highest deleted conversation ID")
					conv = replacement
				}
				replacement, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "replacement", MessageType: kind})
				require.NoError(t, err)
				require.Equal(t, target, replacement, "SQLite automatically reuses the highest deleted message ID")
				got, err := st.GetSavedView(t.Context(), view.ID)
				require.NoError(t, err)
				assert.Equal(t, int64(3), got.Revision)
				items, stale, err := st.SavedViewItemsForView(t.Context(), got)
				require.NoError(t, err)
				assert.Zero(t, stale)
				entries := items.Include
				if mode == "exclude" {
					entries = items.Exclude
					assert.Empty(t, items.Include)
				} else {
					assert.Empty(t, items.Exclude)
				}
				require.Len(t, entries, 1)
				assert.Equal(t, kept, entries[0].MessageID)
				assert.Equal(t, source.ID, entries[0].SourceID)
			})
		}
	}
}
