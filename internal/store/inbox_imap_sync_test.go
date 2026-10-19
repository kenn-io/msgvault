package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newInboxIMAPSyncFixture(t *testing.T) (imapMembershipFixture, inboxcontrol.Target) {
	t.Helper()
	st := testutil.NewTestStore(t)
	config := &imap.Config{Host: "imap.example.test", Username: "reader@example.test", TLS: true}
	source, err := st.GetOrCreateSource("imap", config.Identifier())
	require.NoError(t, err)
	encoded, err := config.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, encoded))
	conversation, err := st.EnsureConversation(source.ID, "sync-observation", "Synthetic sync observation")
	require.NoError(t, err)
	f := imapMembershipFixture{store: st, source: source, convID: conversation}
	id := f.createMessage(t, "synthetic-message", "")
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: config.Username, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "synthetic-message", Mailbox: "INBOX", UIDValidity: 17, UID: 1}
	return f, target
}

func inboxIMAPSyncDelta(flags []string, reset bool) []store.IMAPMailboxDelta {
	return []store.IMAPMailboxDelta{{Mailbox: "INBOX", Reset: reset, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2, HighestModSeq: 900}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: "synthetic-message", Flags: flags}}}}
}

func TestInboxIMAPSyncPublishesCurrentFlagsEvidence(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "delta", true: "reset"}[reset], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, target := newInboxIMAPSyncFixture(t)
			_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE messages SET is_read=? WHERE id=?`), reset, target.ItemID)
			requirements.NoError(err)
			for _, flags := range [][]string{{`\Seen`, "Todo"}, {`\Seen`, "Todo"}, nil, {}, {}, nil, {`\Seen`}} {
				requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, inboxIMAPSyncDelta(flags, reset)))
				state, err := f.store.GetInboxProviderState(t.Context(), target)
				requirements.NoError(err)
				requirements.NotNil(state)
				assertions.Equal(target, state.Target)
				assertions.Equal("17:0", state.Revision, "mailbox HIGHESTMODSEQ is not exact-message CAS evidence")
				assertions.Equal(flags, state.Flags)
				if flags == nil {
					assertions.Nil(state.Read)
					assertions.Nil(state.Tags)
				} else {
					requirements.NotNil(state.Read)
					assertions.Equal(len(flags) > 0, *state.Read)
					assertions.NotNil(state.Tags)
				}
				assertions.Equal(reset, messageIsRead(t, f.store, target.ItemID), "provider read never changes local UI state")
			}
		})
	}
}

func TestInboxIMAPSyncRejectsSupersededGenerationAtomically(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, target := newInboxIMAPSyncFixture(t)
	run, err := f.store.StartSync(f.source.ID, "full")
	requirements.NoError(err)
	requirements.NoError(f.store.CompleteSync(run, "first-generation"))
	requirements.NoError(f.store.ApplyIMAPMailboxDeltasForSyncContext(t.Context(), f.source.ID, run, inboxIMAPSyncDelta([]string{`\Seen`}, false)))
	before, err := f.store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(before)
	next, err := f.store.StartSync(f.source.ID, "full")
	requirements.NoError(err)
	for _, completed := range []bool{false, true} {
		if completed {
			requirements.NoError(f.store.CompleteSync(next, "next-generation"))
		}
		err := f.store.ApplyIMAPMailboxDeltasForSyncContext(t.Context(), f.source.ID, run, inboxIMAPSyncDelta([]string{}, true))
		requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
		got, err := f.store.GetInboxProviderState(t.Context(), target)
		requirements.NoError(err)
		assertions.Equal(before, got)
		_, cached := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
		assertions.JSONEq(`["\\Seen"]`, cached)
	}
}

func TestInboxIMAPSyncInvalidAccountCannotPublishActionableState(t *testing.T) {
	for _, config := range []string{`{}`, `{"username":"other@example.test"}`, `{"username":42}`} {
		t.Run(config, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, target := newInboxIMAPSyncFixture(t)
			requirements.NoError(f.store.UpdateSourceSyncConfig(f.source.ID, config))
			requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, inboxIMAPSyncDelta([]string{`\Seen`}, false)))
			state, err := f.store.GetInboxProviderState(t.Context(), target)
			requirements.NoError(err)
			assertions.Nil(state)
			assertions.Equal(1, membershipCount(t, f.store, f.source.ID), "legacy membership sync remains available")
		})
	}
}

func TestInboxIMAPSyncObservationPublicationIsAtomic(t *testing.T) {
	for _, table := range []string{"inbox_provider_states", "inbox_source_revisions"} {
		t.Run(table, func(t *testing.T) {
			requirements := require.New(t)
			f, _ := newInboxIMAPSyncFixture(t)
			var err error
			if f.store.IsPostgreSQL() {
				_, err = f.store.DB().Exec(`CREATE FUNCTION fail_inbox_sync() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic publication failure'; END; $$ LANGUAGE plpgsql`)
				requirements.NoError(err)
				_, err = f.store.DB().Exec("CREATE TRIGGER fail_inbox_sync BEFORE INSERT ON " + table + " FOR EACH ROW EXECUTE FUNCTION fail_inbox_sync()")
			} else {
				_, err = f.store.DB().Exec("CREATE TRIGGER fail_inbox_sync BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT, 'synthetic publication failure'); END")
			}
			requirements.NoError(err)
			err = f.store.ApplyIMAPMailboxDeltas(f.source.ID, inboxIMAPSyncDelta([]string{}, false))
			requirements.Error(err)
			for _, changed := range []string{"imap_message_memberships", "imap_folder_state", "inbox_provider_states", "inbox_source_revisions"} {
				var count int
				requirements.NoError(f.store.DB().QueryRow(f.store.Rebind("SELECT COUNT(*) FROM "+changed+" WHERE source_id=?"), f.source.ID).Scan(&count))
				assert.Zero(t, count, changed+" must roll back with failed state/revision publication")
			}
		})
	}
}

func TestInboxIMAPSyncRetiresObsoleteObservations(t *testing.T) {
	for _, removal := range []string{"vanished", "epoch", "rekey", "mailbox"} {
		t.Run(removal, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f, original := newInboxIMAPSyncFixture(t)
			requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, inboxIMAPSyncDelta([]string{}, false)))
			state, err := f.store.GetInboxProviderState(t.Context(), original)
			requirements.NoError(err)
			requirements.NotNil(state)
			var beforeRevision int64
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT revision FROM inbox_source_revisions WHERE source_id=?`), f.source.ID).Scan(&beforeRevision))
			deltas := inboxIMAPSyncDelta([]string{}, false)
			switch removal {
			case "vanished":
				deltas[0].Memberships = nil
				deltas[0].VanishedUIDs = []uint32{1}
			case "epoch":
				deltas[0].State.UIDValidity = 18
				deltas[0].Reset = true
			case "rekey":
				_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE messages SET source_message_id='rekeyed-message' WHERE id=?`), original.ItemID)
				requirements.NoError(err)
				deltas[0].Memberships[0].CanonicalSourceMessageID = "rekeyed-message"
			case "mailbox":
				deltas = []store.IMAPMailboxDelta{}
			}
			requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, deltas))
			state, err = f.store.GetInboxProviderState(t.Context(), original)
			requirements.NoError(err)
			assertions.Nil(state)
			var afterRevision int64
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT revision FROM inbox_source_revisions WHERE source_id=?`), f.source.ID).Scan(&afterRevision))
			assertions.Greater(afterRevision, beforeRevision)
		})
	}
}
