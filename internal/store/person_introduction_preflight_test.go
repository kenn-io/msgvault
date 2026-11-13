package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIntroductionPreflightAmbiguityAndOwner(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "account-a")
	req.NoError(err)
	other, err := f.Store.GetOrCreateSource("beeper", "account-b")
	req.NoError(err)
	pa := f.EnsureParticipant("member-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("member-b@example.com", "B", "example.com")
	pc := f.EnsureParticipant("candidate@example.com", "C", "example.com")
	pd := f.EnsureParticipant("candidate2@example.com", "D", "example.com")
	_, err = f.Store.LinkParticipants(pa, pb)
	req.NoError(err)
	_, err = f.Store.LinkParticipants(pc, pd)
	req.NoError(err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	req.NoError(err)
	// The account address matches only the scoped account-B identifier, not an email.
	req.NoError(f.Store.AddAccountIdentityContext(ctx, other.ID, "self-opaque", "manual"))
	for _, entry := range []struct {
		id            int64
		account, user string
	}{
		{pa, "account-a", "member"}, {pb, "account-b", "self-opaque"},
		{pc, "account-a", "one"}, {pd, "account-a", "two"},
	} {
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), entry.id, beeperidentity.Fallback(entry.account, entry.user))
		req.NoError(err)
	}
	got, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, a.ID, b.ID)
	req.NoError(err)
	check.Contains(got.Blockers, store.IntroductionBlocker{Code: "owner_selected", PersonID: a.ID})
	check.Contains(got.Blockers, store.IntroductionBlocker{Code: "ambiguous_identity", PersonID: b.ID})
	req.Len(got.Members, 1)
	check.Equal(a.ID, got.Members[0].PersonID)
	check.Equal("member", got.Members[0].UserID)
}

func TestIntroductionEmailEnvelopeCandidates(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	pa := f.EnsureParticipant("global-a@example.test", "A", "example.test")
	pb := f.EnsureParticipant("global-b@example.test", "B", "example.test")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE conversations SET conversation_type = 'email_thread' WHERE id = ?`), f.ConvID)
	require.NoError(t, err)
	id := f.CreateMessage("envelope")
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address) VALUES (?, ?, 'from', ' A@Example.Test '), (?, ?, 'to', 'b@example.test')`), id, pa, id, pb)
	require.NoError(t, err)
	preflight := func(want []store.IntroductionBlocker) {
		t.Helper()
		p, err := f.Store.GetPersonIntroductionPreflightContext(ctx, f.Source.ID, a.ID, b.ID)
		require.NoError(t, err)
		assert.Equal(t, want, p.Blockers)
	}
	preflight([]store.IntroductionBlocker{})
	p, err := f.Store.GetPersonIntroductionPreflightContext(ctx, f.Source.ID, a.ID, b.ID)
	require.NoError(t, err)
	require.Len(t, p.Members, 2)
	assert.Equal(t, "a@example.test", p.Members[0].UserID)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address) VALUES (?, ?, 'cc', 'a@example.test')`), id, pa)
	require.NoError(t, err)
	preflight([]store.IntroductionBlocker{}) // repeated normalized tuples are one identity
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent = FALSE WHERE id = ?`), id)
	require.NoError(t, err)
	absent := []store.IntroductionBlocker{{Code: "no_identity_on_account", PersonID: a.ID}, {Code: "no_identity_on_account", PersonID: b.ID}}
	preflight(absent)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent = TRUE, metadata = ? WHERE id = ?`), `{"is_draft":true,"imap_content_origin":"sent"}`, id)
	require.NoError(t, err)
	preflight([]store.IntroductionBlocker{})
	// Retained draft labels do not exclude a source-confirmed sent copy.
	draft, err := f.Store.EnsureLabel(ctx, f.Source.ID, "DRAFT", "DRAFT", "system")
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_labels (message_id,label_id) VALUES (?,?)`), id, draft)
	require.NoError(t, err)
	preflight([]store.IntroductionBlocker{})
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE sources SET source_type='imap' WHERE id=?`), f.Source.ID)
	require.NoError(t, err)
	preflight([]store.IntroductionBlocker{}) // saved IMAP Sent origin
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), id)
	require.NoError(t, err)
	preflight(absent)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent=TRUE WHERE id=?`), id)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'bcc','alias@example.test')`), id, pa)
	require.NoError(t, err)
	preflight([]store.IntroductionBlocker{{Code: "ambiguous_identity", PersonID: a.ID}})
	// Other accounts' evidence and timeless participant mailboxes cannot qualify.
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
	require.NoError(t, err)
	cross, err := f.Store.GetPersonIntroductionPreflightContext(ctx, other.ID, a.ID, b.ID)
	require.NoError(t, err)
	assert.Equal(t, absent, cross.Blockers)

	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE message_recipients SET email_address = NULL WHERE message_id = ?`), id)
	require.NoError(t, err)
	preflight(absent) // no fallback to participants.email_address
}
