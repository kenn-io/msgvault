package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEmlxLedgerFencedTransitions(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	id := "emlx-" + strings.Repeat("a", 64)
	run := f.StartSync()
	scoped := f.Store.ScopedToSync(f.Source.ID, run)
	item := store.SourceImportItem{SourceID: f.Source.ID, Provider: "emlx-target", ProviderID: id, Status: "pending", Checksum: "dirty"}
	r.NoError(scoped.PutEmlxLedgerItemsContext(t.Context(), item))
	item.Status = "imported"
	r.NoError(scoped.PutEmlxLedgerItemsContext(t.Context(), item))
	r.NoError(f.Store.FailSync(run, "stopped"))
	_ = f.StartSync()
	item.Status = "pending"
	r.ErrorIs(scoped.PutEmlxLedgerItemsContext(t.Context(), item), store.ErrSyncRunSuperseded)
	states, err := f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, 0, []string{id})
	r.NoError(err)
	r.NotNil(states[id].Item)
	a.Equal("imported", states[id].Item.Status)
}

func TestEmlxLedgerRejectsOtherSource(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	other, err := f.Store.GetOrCreateSource("apple-mail", "other@example.test")
	r.NoError(err)
	scoped := f.Store.ScopedToSync(f.Source.ID, f.StartSync())
	id := "emlx-" + strings.Repeat("e", 64)
	item := store.SourceImportItem{SourceID: other.ID, Provider: "emlx-target", ProviderID: id, Status: "imported"}
	r.ErrorContains(scoped.PutEmlxLedgerItemsContext(t.Context(), item), "is scoped to source")
	states, err := f.Store.EmlxTargetsContext(t.Context(), other.ID, 0, []string{id})
	r.NoError(err)
	a.Nil(states[id].Item, "a run cannot publish receipts for another source")
}

func TestEmlxLedgerAtomicRootInvalidation(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	root := strings.Repeat("a", 64) + "/"
	other := strings.Repeat("b", 64) + "/"
	for _, id := range []string{root + "Inbox/1.emlx", root + "Inbox/2.emlx", other + "Inbox/1.emlx"} {
		r.NoError(f.Store.PutEmlxLedgerItemsContext(t.Context(), store.SourceImportItem{SourceID: f.Source.ID, Provider: "emlx-occurrence", ProviderID: id, Status: "imported"}))
	}
	r.NoError(f.Store.InvalidateEmlxRootContext(t.Context(), f.Source.ID, root))
	ids := []string{root + "Inbox/1.emlx", root + "Inbox/2.emlx", other + "Inbox/1.emlx", root + "Inbox/3.emlx"}
	entries, err := f.Store.EmlxOccurrencesContext(t.Context(), f.Source.ID, ids)
	r.NoError(err)
	r.Len(entries, 3, "a missing receipt is a cold occurrence")
	a.Equal("pending", entries[ids[0]].Status)
	a.Equal("pending", entries[ids[1]].Status)
	a.Equal("imported", entries[ids[2]].Status)
}

func TestEmlxTargetsReportLabelMembership(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	id := "emlx-" + strings.Repeat("e", 64)
	mid := f.CreateMessage(id)
	labelID, err := f.Store.EnsureLabel(f.Source.ID, "Inbox", "Inbox", "user")
	r.NoError(err)
	states, err := f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, labelID, []string{id})
	r.NoError(err)
	a.False(states[id].HasLabel)
	r.NoError(f.Store.AddMessageLabels(mid, []int64{labelID}))
	states, err = f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, labelID, []string{id})
	r.NoError(err)
	a.True(states[id].HasLabel)
}

func TestEmlxTargetsRawAndDeletionState(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	id := "emlx-" + strings.Repeat("c", 64)
	mid := f.CreateMessage(id)
	states, err := f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, 0, []string{id})
	r.NoError(err)
	a.Equal(mid, states[id].MessageID)
	a.False(states[id].HasRaw)
	a.False(states[id].Deleted)
	r.NoError(f.Store.UpsertMessageRaw(mid, []byte("Subject: synthetic\r\n\r\nbody")))
	states, err = f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, 0, []string{id})
	r.NoError(err)
	a.True(states[id].HasRaw)
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), mid)
	r.NoError(err)
	states, err = f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, 0, []string{id})
	r.NoError(err)
	a.Equal(mid, states[id].MessageID)
	a.True(states[id].HasRaw)
	a.True(states[id].Deleted)
}

func TestEmlxLedgerBoundedCancellation(t *testing.T) {
	r := require.New(t)
	f := storetest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.Store.EmlxOccurrencesContext(ctx, f.Source.ID, []string{strings.Repeat("a", 64) + "/Inbox/1.emlx"})
	r.ErrorIs(err, context.Canceled)
	_, err = f.Store.EmlxTargetsContext(ctx, f.Source.ID, 0, []string{"emlx-" + strings.Repeat("a", 64)})
	r.ErrorIs(err, context.Canceled)
	r.Error(f.Store.PutEmlxLedgerItemsContext(t.Context(), store.SourceImportItem{SourceID: f.Source.ID, Provider: "drive", ProviderID: "x", Status: "imported"}))
}
