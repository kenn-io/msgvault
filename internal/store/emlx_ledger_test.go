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
	r.NoError(scoped.PutEmlxLedgerItemContext(t.Context(), item))
	item.Status = "imported"
	r.NoError(scoped.PutEmlxLedgerItemContext(t.Context(), item))
	r.NoError(f.Store.FailSync(run, "stopped"))
	_ = f.StartSync()
	item.Status = "pending"
	r.ErrorIs(scoped.PutEmlxLedgerItemContext(t.Context(), item), store.ErrSyncRunSuperseded)
	states, err := f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, []string{id})
	r.NoError(err)
	r.NotNil(states[id].Item)
	a.Equal("imported", states[id].Item.Status)
	item.SourceID++
	a.Error(scoped.PutEmlxLedgerItemContext(t.Context(), item))
}

func TestEmlxLedgerAtomicRootInvalidation(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	root := strings.Repeat("a", 64) + "/"
	other := strings.Repeat("b", 64) + "/"
	for _, id := range []string{root + "Inbox/1.emlx", root + "Inbox/2.emlx", other + "Inbox/1.emlx"} {
		r.NoError(f.Store.PutEmlxLedgerItemContext(t.Context(), store.SourceImportItem{SourceID: f.Source.ID, Provider: "emlx-occurrence", ProviderID: id, Status: "imported"}))
	}
	r.NoError(f.Store.InvalidateEmlxRootContext(t.Context(), f.Source.ID, root))
	entries, err := f.Store.ListEmlxOccurrencesContext(t.Context(), f.Source.ID, root, 0, 200)
	r.NoError(err)
	r.Len(entries, 2)
	for _, entry := range entries {
		a.Equal("pending", entry.Status)
	}
	entries, err = f.Store.ListEmlxOccurrencesContext(t.Context(), f.Source.ID, other, 0, 200)
	r.NoError(err)
	r.Len(entries, 1)
	a.Equal("imported", entries[0].Status)
}

func TestEmlxTargetsRawAndDeletionState(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := storetest.New(t)
	id := "emlx-" + strings.Repeat("c", 64)
	mid := f.CreateMessage(id)
	states, err := f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, []string{id})
	r.NoError(err)
	a.Equal(mid, states[id].MessageID)
	a.False(states[id].HasRaw)
	a.False(states[id].Deleted)
	r.NoError(f.Store.UpsertMessageRaw(mid, []byte("Subject: synthetic\r\n\r\nbody")))
	states, err = f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, []string{id})
	r.NoError(err)
	a.True(states[id].HasRaw)
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), mid)
	r.NoError(err)
	states, err = f.Store.EmlxTargetsContext(t.Context(), f.Source.ID, []string{id})
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
	_, err := f.Store.ListEmlxOccurrencesContext(ctx, f.Source.ID, strings.Repeat("a", 64)+"/", 0, 200)
	r.ErrorIs(err, context.Canceled)
	_, err = f.Store.EmlxTargetsContext(ctx, f.Source.ID, []string{"emlx-" + strings.Repeat("a", 64)})
	r.ErrorIs(err, context.Canceled)
	r.Error(f.Store.PutEmlxLedgerItemContext(t.Context(), store.SourceImportItem{SourceID: f.Source.ID, Provider: "drive", ProviderID: "x", Status: "imported"}))
}
