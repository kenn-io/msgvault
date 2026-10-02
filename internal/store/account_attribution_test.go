package store_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAccountAttributionTargetedIdentityRecompute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, f.Source.Identifier, "manual"))
	matched := f.CreateMessage("masked")
	unrelated := f.CreateMessage("unrelated")
	require.NoError(st.UpsertMessageRaw(matched, []byte("From: sender@example.test\r\nX-Delivered-To: mask@example.org\r\nDelivered-To: "+f.Source.Identifier+"\r\n\r\nbody")))
	require.NoError(st.UpsertMessageRaw(unrelated, []byte("To: "+f.Source.Identifier+"\r\n\r\nbody")))
	before, err := st.GetAccountAttributionContext(t.Context(), unrelated)
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(f.Source.ID, "mask@example.org", "manual"))
	got, err := st.GetAccountAttributionContext(t.Context(), matched)
	require.NoError(err)
	assert.Equal("mask@example.org", got.Address)
	assert.Equal("original-recipient", got.Basis)
	count, err := st.RecomputeAccountAttributionForIdentitiesContext(t.Context(), f.Source.ID, []string{"MASK@example.org"})
	require.NoError(err)
	assert.Equal(int64(1), count, "only indexed mention matches may be visited")
	after, err := st.GetAccountAttributionContext(t.Context(), unrelated)
	require.NoError(err)
	assert.Equal(before, after)
	_, err = st.RemoveAccountIdentity(f.Source.ID, "mask@example.org")
	require.NoError(err)
	got, err = st.GetAccountAttributionContext(t.Context(), matched)
	require.NoError(err)
	assert.Equal(f.Source.Identifier, got.Address)
}

func TestAccountAttributionAuthorshipFlagIsNotSentEvidence(t *testing.T) {
	for _, kind := range []string{"gmail", "imap", "o365", "msmail", "eml", "mbox", "maildir", "pst", "hey", "apple-mail"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			source, err := f.Store.GetOrCreateSource(kind, "inbox@example.net")
			require.NoError(err)
			for _, address := range []string{"inbox@example.net", "work@example.org"} {
				require.NoError(f.Store.AddAccountIdentity(source.ID, address, "manual"))
			}
			// ImportEmail historically inferred this source flag from From equality.
			// An incoming owned From cannot establish a Sent path on an import.
			id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message: &store.Message{SourceID: source.ID, ConversationID: f.ConvID, SourceMessageID: "owned-from-incoming", MessageType: "email", IsFromMe: true},
				RawMIME: []byte("From: inbox@example.net\r\nX-Original-To: work@example.org\r\n\r\nbody"),
			})
			require.NoError(err)
			got, err := f.Store.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal("inbound", got.Path)
			assert.Equal("work@example.org", got.Address)
		})
	}
}

func TestAccountAttributionBackfillResumeAndReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.com", "manual"))
	var ids []int64
	for _, key := range []string{"one", "two", "three"} {
		id := f.CreateMessage(key)
		ids = append(ids, id)
		require.NoError(st.UpsertMessageRaw(id, []byte("To: work@example.com\r\n\r\nbody")))
		_, err := st.DB().Exec(st.Rebind("DELETE FROM message_account_evidence WHERE message_id = ?"), id)
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind("UPDATE messages SET account_address = NULL, account_path = NULL, account_attribution_basis = 'not-derived' WHERE id = ?"), id)
		require.NoError(err)
	}
	stopped := errors.New("stop after committed page")
	p, err := st.BackfillAccountAttributionContext(t.Context(), f.Source.ID, 1, func(store.AccountAttributionProgress) error { return stopped })
	require.ErrorIs(err, stopped)
	assert.Equal(ids[0], p.LastMessageID)
	p, err = st.BackfillAccountAttributionContext(t.Context(), f.Source.ID, 1, nil)
	require.NoError(err)
	assert.True(p.Completed)
	for _, id := range ids {
		got, e := st.GetAccountAttributionContext(t.Context(), id)
		require.NoError(e)
		assert.Equal("work@example.com", got.Address)
	}
	rev, err := st.DerivedDataRevision()
	require.NoError(err)
	_, err = st.BackfillAccountAttributionContext(t.Context(), f.Source.ID, 1, nil)
	require.NoError(err)
	again, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(rev, again)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.BackfillAccountAttributionContext(ctx, f.Source.ID, 1, nil)
	require.ErrorIs(err, context.Canceled)
}

func TestAccountAttributionBackfillIncludesEntireMessageIDRange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	source, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "work@example.org", "manual"))
	ids := []int64{math.MinInt64, 0, 7}
	for i, id := range ids {
		seedMessageAtID(t, st, i+1, id)
		require.NoError(st.UpsertMessageRaw(id, []byte("To: work@example.org\r\n\r\nbody")))
		_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET account_address=NULL,account_path=NULL,account_attribution_basis='not-derived' WHERE id=?`), id)
		require.NoError(err)
	}
	stop := errors.New("stop after first committed page")
	first, err := st.BackfillAccountAttributionContext(t.Context(), source.ID, 1, func(store.AccountAttributionProgress) error { return stop })
	require.ErrorIs(err, stop)
	assert.Equal(ids[0], first.LastMessageID, "a zero initial cursor must not skip legal nonpositive message IDs")
	finished, err := st.BackfillAccountAttributionContext(t.Context(), source.ID, 1, nil)
	require.NoError(err)
	assert.True(finished.Completed)
	assert.Equal(int64(2), finished.Scanned, "resume excludes the first committed message")
	for _, id := range ids {
		got, err := st.GetAccountAttributionContext(t.Context(), id)
		require.NoError(err)
		assert.Equal("work@example.org", got.Address)
	}
}

func TestCalendarAccountAttributionUsesCalendarIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(source.ID, `{"account_email":"login@example.net","calendar_id":"calendar@example.com"}`))
	require.NoError(st.AddAccountIdentity(source.ID, "calendar@example.com", "manual"))
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: f.ConvID, SourceMessageID: "event", MessageType: "calendar_event"})
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"calendar_id":"calendar@example.com","account_email":"login@example.net","organizer_email":"other@example.org"}`, Valid: true}))
	got, err := st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("calendar@example.com", got.Address)
	assert.Equal("calendar", got.Path)
}

func TestAccountAttributionSentLabelChanges(t *testing.T) {
	for _, tc := range []struct{ name, from string }{
		{"single", "sender@example.org"},
		{"duplicate", "sender@example.org, SENDER@example.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
			require.NoError(st.AddAccountIdentity(f.Source.ID, "sender@example.org", "manual"))
			id := f.CreateMessage("label-change")
			require.NoError(st.UpsertMessageRaw(id, []byte("From: "+tc.from+"\r\nTo: work@example.org\r\n\r\nbody")))
			label, err := st.EnsureLabel(f.Source.ID, "SENT", "Sent", "system")
			require.NoError(err)
			require.NoError(st.LinkMessageLabel(id, label))
			got, err := st.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal("sender@example.org", got.Address)
			assert.Equal("sent", got.Path)
			assert.Equal("sent-from", got.Basis)
			require.NoError(st.RemoveMessageLabels(id, []int64{label}))
			got, err = st.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal("work@example.org", got.Address)
			assert.Equal("inbound", got.Path)
		})
	}
}
func TestCalendarAccountAttributionMappingChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	src, err := st.GetOrCreateSource("gcal", "opaque-calendar")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "first@example.org", "manual"))
	require.NoError(st.AddAccountIdentity(src.ID, "second@example.org", "manual"))
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"opaque-calendar","account_email":"login@example.net","account_address":"first@example.org"}`))
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "mapped-event", MessageType: "calendar_event"})
	require.NoError(err)
	got, err := st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("first@example.org", got.Address)
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"opaque-calendar","account_email":"login@example.net","sync_token":"new-token"}`))
	got, err = st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("first@example.org", got.Address, "sync refresh preserves explicit archive mapping")
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"opaque-calendar","account_address":"second@example.org"}`))
	got, err = st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("second@example.org", got.Address)
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"opaque-calendar","account_address":""}`))
	got, err = st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Empty(got.Address)
	assert.Equal("unmapped-calendar", got.Basis)
}

func TestAccountAttributionPersistsEveryForwardingFixture(t *testing.T) {
	for _, tc := range []struct{ name, address string }{
		{"gmail", "work@example.com"}, {"gmail-visible", "work@example.com"}, {"pop", "work@example.com"}, {"workspace", "work@example.com"}, {"fastmail", "mask@example.org"}, {"generic", "work@example.com"}, {"bcc", "mask@example.org"}, {"list", "mask@example.org"}, {"ambiguous", ""}, {"conflicting", ""}, {"nested", "inbox@example.net"}, {"malformed-mime", "work@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			src, err := st.GetOrCreateSource("gmail", "inbox@example.net")
			require.NoError(err)
			for _, address := range []string{"inbox@example.net", "work@example.com", "mask@example.org", "second@example.com"} {
				require.NoError(st.AddAccountIdentity(src.ID, address, "manual"))
			}
			raw, err := os.ReadFile("../emailattribution/testdata/" + tc.name + ".eml")
			require.NoError(err)
			id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: tc.name, MessageType: "email"}, RawMIME: raw})
			require.NoError(err)
			got, err := st.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(tc.address, got.Address)
			require.NoError(st.RefreshAccountAttributionContext(t.Context(), id))
			again, err := st.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(got, again)
		})
	}
}

func TestAccountAttributionIndexedLookups(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	if st.IsPostgreSQL() {
		t.Skip("SQLite query-plan proof; PostgreSQL runs the functional parity tests")
	}
	for _, stmt := range []string{
		`EXPLAIN QUERY PLAN SELECT id FROM messages WHERE source_id=1 AND account_address='work@example.org'`,
		`EXPLAIN QUERY PLAN SELECT message_id FROM message_account_mentions WHERE source_id=1 AND address_key='work@example.org'`,
	} {
		rows, err := st.DB().Query(stmt)
		require.NoError(err)
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(rows.Scan(&id, &parent, &unused, &detail))
			plan.WriteString(detail)
		}
		require.NoError(rows.Err())
		defer func() { require.NoError(rows.Close()) }()
		assert.Contains(plan.String(), "SEARCH")
		if strings.Contains(stmt, "message_account_mentions") {
			assert.Contains(plan.String(), "idx_account_mentions_address")
		} else {
			assert.Contains(plan.String(), "idx_messages_source_account")
		}
	}
}

func TestAccountAttributionBackfillIncludesLegacyEmptyType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	id := f.CreateMessage("legacy-empty-type")
	require.NoError(st.UpsertMessageRaw(id, []byte("To: work@example.org\r\n\r\nbody")))
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET message_type='',account_address=NULL,account_path=NULL,account_attribution_basis='not-derived' WHERE id=?`), id)
	require.NoError(err)
	p, err := st.BackfillAccountAttributionContext(t.Context(), f.Source.ID, 100, nil)
	require.NoError(err)
	assert.Equal(int64(1), p.Scanned)
	got, err := st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("work@example.org", got.Address)
}

func TestIMAPAccountAttributionUsesMailboxLogin(t *testing.T) {
	for _, tc := range []struct{ name, identifier, config string }{
		{"identifier", "imaps://inbox@example.net@imap.example.net:993", ""},
		{"escaped identifier", "imaps://inbox%40example.net@imap.example.net:993", ""},
		{"configured username", "imap+starttls://login@imap.example.net:143", `{"username":"inbox@example.net"}`},
		{"config wins", "imaps://legacy@example.org@imap.example.net:993", `{"username":"INBOX@example.net"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			src, err := st.GetOrCreateSource("imap", tc.identifier)
			require.NoError(err)
			if tc.config != "" {
				require.NoError(st.UpdateSourceSyncConfig(src.ID, tc.config))
			}
			require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
			persist := func(key, raw string) int64 {
				id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
					Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email"},
					RawMIME: []byte(raw),
				})
				require.NoError(err)
				return id
			}
			forwarded := persist("forwarded", "Delivered-To: inbox@example.net\r\nTo: work@example.org\r\n\r\nbody")
			fallback := persist("fallback", "From: sender@example.org\r\n\r\nbody")
			persist("unrelated", "To: work@example.org\r\n\r\nbody")
			before, err := st.GetAccountAttributionContext(t.Context(), fallback)
			require.NoError(err)
			assert.Empty(before.Address, "a login is not a confirmed archive identity")
			require.NoError(st.AddAccountIdentity(src.ID, "inbox@example.net", "manual"))
			got, err := st.GetAccountAttributionContext(t.Context(), forwarded)
			require.NoError(err)
			assert.Equal("work@example.org", got.Address)
			assert.Equal("recipient-headers", got.Basis, "defer the final IMAP sink behind visible original recipients")
			got, err = st.GetAccountAttributionContext(t.Context(), fallback)
			require.NoError(err)
			assert.Equal("inbox@example.net", got.Address)
			assert.Equal("source-default", got.Basis)
			count, err := st.RecomputeAccountAttributionForIdentitiesContext(t.Context(), src.ID, []string{"inbox@example.net"})
			require.NoError(err)
			assert.Equal(int64(2), count, "fallback dependency mentions use the mailbox, not the connection URL")
			_, err = st.RemoveAccountIdentity(src.ID, "inbox@example.net")
			require.NoError(err)
			got, err = st.GetAccountAttributionContext(t.Context(), fallback)
			require.NoError(err)
			assert.Empty(got.Address, "removing login confirmation clears fallback attribution")
		})
	}
}

func TestIMAPAccountAttributionRejectsNonMailboxLogin(t *testing.T) {
	for _, username := range []string{"login", "Alias <inbox@example.net>"} {
		t.Run(username, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			src, err := f.Store.GetOrCreateSource("imap", "imaps://login@imap.example.net:993")
			require.NoError(err)
			require.NoError(f.Store.UpdateSourceSyncConfig(src.ID, `{"username":"`+username+`"}`))
			require.NoError(f.Store.AddAccountIdentity(src.ID, "inbox@example.net", "manual"))
			id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "invalid-login", MessageType: "email"},
				RawMIME: []byte("From: sender@example.org\r\n\r\nbody"),
			})
			require.NoError(err)
			got, err := f.Store.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Empty(got.Address)
			assert.Equal("missing-evidence", got.Basis)
		})
	}
}

func TestIMAPMailboxReconciliationRefreshesAccountAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	src, err := st.GetOrCreateSource("imap", "imaps://inbox@example.net@imap.example.net:993")
	require.NoError(err)
	for _, address := range []string{"sender@example.org", "work@example.org"} {
		require.NoError(st.AddAccountIdentity(src.ID, address, "manual"))
	}
	labels, err := st.EnsureLabelsBatch(src.ID, map[string]store.LabelInfo{
		"Sent":    {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"Archive": {Name: "Archive", Type: "system"},
	})
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "shared", MessageType: "email"},
		RawMIME: []byte("From: sender@example.org\r\nX-Original-To: work@example.org\r\n\r\nbody"),
	})
	require.NoError(err)
	require.NoError(st.LinkMessageLabel(id, labels["Sent"]))
	initial := []store.IMAPMailboxDelta{
		{Mailbox: "Sent", State: store.IMAPFolderState{Mailbox: "Sent", UIDValidity: 1, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{Mailbox: "Sent", UIDValidity: 1, UID: 1, SourceMessageID: "shared"}}},
		{Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 2, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{Mailbox: "Archive", UIDValidity: 2, UID: 1, SourceMessageID: "shared"}}},
	}
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, initial))
	got, err := st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	require.Equal("sender@example.org", got.Address)
	require.Equal("sent", got.Path)
	changed := []store.IMAPMailboxDelta{
		{Mailbox: "Sent", State: initial[0].State, VanishedUIDs: []uint32{1}},
		{Mailbox: "Archive", State: initial[1].State},
	}
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, changed))
	assert.Equal([]string{"Archive"}, messageLabels(t, st, id))
	got, err = st.GetAccountAttributionContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("work@example.org", got.Address)
	assert.Equal("inbound", got.Path)
	assert.Equal("original-recipient", got.Basis)
	revision, err := st.DerivedDataRevision()
	require.NoError(err)
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, changed))
	replay, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(revision, replay, "unchanged membership replay does not rewrite attribution")
}

func TestScopedSyncLocksIdentityBeforeGenerationFence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL lock-order regression")
	}
	run, err := st.StartSync(f.Source.ID, "full")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
	var revision string
	require.NoError(blocker.QueryRowContext(ctx, "SELECT value FROM archive_metadata WHERE key = $1 FOR UPDATE", "identity_revision").Scan(&revision))
	done := make(chan error, 1)
	scoped := st.ScopedToSync(f.Source.ID, run)
	go func() {
		_, err := scoped.PersistMessageContext(ctx, &store.MessagePersistData{
			Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "scoped-order", MessageType: "email"},
			RawMIME: []byte("To: work@example.org\r\n\r\nbody"),
		})
		done <- err
	}()
	require.Eventually(func() bool {
		var waiting bool
		err := st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%archive_metadata%')`, blockerPID).Scan(&waiting)
		return err == nil && waiting
	}, 20*time.Second, 20*time.Millisecond, "scoped writer must reach the held identity lock")
	// This is the same table lock used by exclusive maintenance/source removal.
	// NOWAIT observes the ordering without manufacturing a real deadlock.
	_, err = blocker.ExecContext(ctx, "LOCK TABLE sync_runs IN EXCLUSIVE MODE NOWAIT")
	require.NoError(err, "a writer waiting for identity must not already hold the generation fence")
	require.NoError(blocker.Commit())
	select {
	case err := <-done:
		require.NoError(err)
	case <-ctx.Done():
		require.FailNow("scoped write did not finish after releasing identity", ctx.Err())
	}
	var count int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?"), f.Source.ID, "scoped-order").Scan(&count))
	assert.Equal(1, count)
}

func TestCanonicalSentRoleChangesRefreshExistingAccountFacts(t *testing.T) {
	for _, writer := range []string{"batch", "repair"} {
		t.Run(writer, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			src, err := st.GetOrCreateSource("imap", "imaps://inbox@example.net@imap.example.net:993")
			require.NoError(err)
			for _, address := range []string{"sender@example.org", "work@example.org"} {
				require.NoError(st.AddAccountIdentity(src.ID, address, "manual"))
			}
			labels, err := st.EnsureLabelsBatch(src.ID, map[string]store.LabelInfo{"folder": {Name: "Folder", Type: "system"}})
			require.NoError(err)
			raw := []byte("From: sender@example.org\r\nX-Original-To: work@example.org\r\n\r\nbody")
			var ids []int64
			for _, key := range []string{"first", "second"} {
				id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
					Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email"}, RawMIME: raw, LabelIDs: []int64{labels["folder"]},
				})
				require.NoError(err)
				ids = append(ids, id)
			}
			progress, err := st.BackfillAccountAttributionContext(t.Context(), src.ID, 100, nil)
			require.NoError(err)
			require.True(progress.Completed)
			setRole := func(role string) {
				info := store.LabelInfo{Name: "Folder", Type: "system", SystemRole: role}
				if writer == "batch" {
					_, err := st.EnsureLabelsBatch(src.ID, map[string]store.LabelInfo{"folder": info})
					require.NoError(err)
				} else {
					_, err := st.PersistRepairMessageWithParticipantsContext(t.Context(), store.MessageIdentityGuard{ID: ids[0], SourceID: src.ID, SourceMessageID: "first"}, []store.ParticipantPersistData{{EmailAddress: "sender@example.org", DisplayName: "Sender", Domain: "example.org"}}, func([]int64) *store.MessagePersistData {
						return &store.MessagePersistData{Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "first", MessageType: "email"}, RawMIME: raw, LabelRefs: []store.MessageLabelRef{{SourceLabelID: "folder", Info: info}}}
					})
					require.NoError(err)
				}
			}
			for _, tc := range []struct{ role, address, path string }{{store.LabelSystemRoleSent, "sender@example.org", "sent"}, {"", "work@example.org", "inbound"}} {
				before, err := st.DerivedDataRevision()
				require.NoError(err)
				setRole(tc.role)
				for _, id := range ids {
					changed, err := st.ReconcileMessageLabels(id, []int64{labels["folder"]}, true)
					require.NoError(err)
					assert.False(changed, "memberships remain unchanged")
					got, err := st.GetAccountAttributionContext(t.Context(), id)
					require.NoError(err)
					assert.Equal(tc.address, got.Address)
					assert.Equal(tc.path, got.Path)
				}
				after, err := st.DerivedDataRevision()
				require.NoError(err)
				assert.Greater(after, before, "changed account facts invalidate cached facts")
				if writer == "batch" {
					setRole(tc.role)
					replay, err := st.DerivedDataRevision()
					require.NoError(err)
					assert.Equal(after, replay, "unchanged roles do not recompute facts")
				}
			}
		})
	}
}

func TestStandaloneLabelChangesRefreshExistingAccountFacts(t *testing.T) {
	for _, sourceType := range []string{"gmail", "imap"} {
		t.Run(sourceType, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			st := f.Store
			src, err := st.GetOrCreateSource(sourceType, "inbox@example.net")
			require.NoError(err)
			for _, address := range []string{"sender@example.org", "work@example.org"} {
				require.NoError(st.AddAccountIdentity(src.ID, address, "manual"))
			}
			key, role := "SENT", ""
			if sourceType == "imap" {
				key, role = "old-folder", store.LabelSystemRoleSent
			}
			labels, err := st.EnsureLabelsBatch(src.ID, map[string]store.LabelInfo{key: {Name: "Old", Type: "system", SystemRole: role}, "replacement": {Name: "New", Type: "user"}})
			require.NoError(err)
			id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message: &store.Message{SourceID: src.ID, ConversationID: f.ConvID, SourceMessageID: "label-transition", MessageType: "email"},
				RawMIME: []byte("From: sender@example.org\r\nX-Original-To: work@example.org\r\n\r\nbody"), LabelIDs: []int64{labels[key]},
			})
			require.NoError(err)
			before, err := st.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal("sender@example.org", before.Address)
			if sourceType == "gmail" {
				// Adopt a new canonical ID while preserving the absent role metadata.
				_, err = st.EnsureLabel(src.ID, "replacement-sent", "Old", "user")
			} else {
				// The surviving non-Sent label absorbs the old Sent memberships.
				_, err = st.EnsureLabel(src.ID, "replacement", "Old", "user")
			}
			require.NoError(err)
			after, err := st.GetAccountAttributionContext(t.Context(), id)
			require.NoError(err)
			assert.Equal("work@example.org", after.Address)
			assert.Equal("inbound", after.Path)
		})
	}
}
