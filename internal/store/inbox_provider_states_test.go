package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxProviderStateKeepsUIRead(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	state := receipt.Before
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET is_read = FALSE WHERE id = ?`), state.Target.ItemID)
	requirements.NoError(err)
	state.Read = new(true)
	changed, err := f.Store.ObserveInboxState(t.Context(), state)
	requirements.NoError(err)
	assertions.True(changed)
	got, err := f.Store.GetInboxProviderState(t.Context(), state.Target)
	requirements.NoError(err)
	requirements.NotNil(got)
	assertions.True(*got.Read)
	var uiRead bool
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT is_read FROM messages WHERE id = ?`), state.Target.ItemID).Scan(&uiRead))
	assertions.False(uiRead)

	older := state
	older.Read = new(false)
	older.ObservedAt = older.ObservedAt.Add(-time.Second)
	changed, err = f.Store.ObserveInboxState(t.Context(), older)
	requirements.NoError(err)
	assertions.False(changed)
	got, err = f.Store.GetInboxProviderState(t.Context(), state.Target)
	requirements.NoError(err)
	assertions.Equal(state, *got)
	state.Read = nil
	state.ObservedAt = state.ObservedAt.Add(time.Second)
	changed, err = f.Store.ObserveInboxState(t.Context(), state)
	requirements.NoError(err)
	assertions.True(changed)
	got, err = f.Store.GetInboxProviderState(t.Context(), state.Target)
	requirements.NoError(err)
	assertions.Nil(got.Read, "unknown state must remain distinct from false")
}

func TestInboxProviderStateRequiresExactMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	state := receipt.Before
	state.Target.SourceType = "imap"
	state.Target.Mailbox = "INBOX"
	state.Target.UIDValidity = 10
	state.Target.UID = 20
	_, err := f.Store.ObserveInboxState(t.Context(), state)
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid, "a provider type cannot override the archived source")
	state = receipt.Before
	state.Target.ItemID += 10000
	_, err = f.Store.ObserveInboxState(t.Context(), state)
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	state = receipt.Before
	state.ObservedAt = time.Time{}
	_, err = f.Store.ObserveInboxState(t.Context(), state)
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	other := receipt.Before.Target
	other.SourceIdentifier = "other@example.com"
	got, err := f.Store.GetInboxProviderState(t.Context(), other)
	requirements.NoError(err)
	assertions.Nil(got)
}

func TestInboxProviderStateSupportsLegacyGmailSourceType(t *testing.T) {
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET source_type = '' WHERE id = ?`), f.Source.ID)
	requirements.NoError(err)
	_, err = f.Store.ObserveInboxState(t.Context(), receipt.Before)
	requirements.NoError(err)
	got, err := f.Store.GetInboxProviderState(t.Context(), receipt.Before.Target)
	requirements.NoError(err)
	requirements.NotNil(got)
	assert.Equal(t, receipt.Before, *got)
}
