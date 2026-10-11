package store_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const attrSink = "inbox@example.net"

type attrFixture struct {
	t      *testing.T
	st     *store.Store
	source *store.Source
	conv   int64
	n      int
}

func newAttrFixture(t *testing.T, sourceType, identifier string) *attrFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	return newAttrFixtureOn(t, st, sourceType, identifier)
}

func newAttrFixtureOn(t *testing.T, st *store.Store, sourceType, identifier string) *attrFixture {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceType, identifier)
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "thread-"+identifier, "Thread")
	require.NoError(t, err)
	return &attrFixture{t: t, st: st, source: source, conv: conv}
}

func (f *attrFixture) confirm(addresses ...string) {
	f.t.Helper()
	for _, a := range addresses {
		require.NoError(f.t, f.st.AddAccountIdentity(f.source.ID, a, "manual"))
	}
}

type attrMail struct {
	raw          string
	from         []string
	to, cc       []string
	labels       []int64
	senderID     int64
	noEnvelope   bool
	sourceMsgKey string
	rawFormat    string
}

func (f *attrFixture) participant(address string) int64 {
	f.t.Helper()
	id, err := f.st.EnsureParticipant(address, "", address[strings.LastIndex(address, "@")+1:])
	require.NoError(f.t, err)
	return id
}

func (f *attrFixture) recipientSet(kind string, addresses []string, envelope bool) store.RecipientSet {
	set := store.RecipientSet{Type: kind}
	for _, a := range addresses {
		set.ParticipantIDs = append(set.ParticipantIDs, f.participant(a))
		set.DisplayNames = append(set.DisplayNames, "")
		if envelope {
			set.EmailAddresses = append(set.EmailAddresses, a)
		}
	}
	if !envelope {
		set.EmailAddresses = nil
	}
	return set
}

func (f *attrFixture) persist(m attrMail) int64 {
	f.t.Helper()
	f.n++
	key := m.sourceMsgKey
	if key == "" {
		key = fmt.Sprintf("m-%d", f.n)
	}
	msg := &store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: key,
		MessageType: "email", SizeEstimate: 100,
	}
	if m.senderID != 0 {
		msg.SenderID = sql.NullInt64{Int64: m.senderID, Valid: true}
	}
	data := &store.MessagePersistData{Message: msg, LabelIDs: m.labels}
	if m.raw != "" {
		data.RawMIME = []byte(m.raw)
		data.RawFormat = m.rawFormat
	}
	for _, rs := range []struct {
		kind  string
		addrs []string
	}{{"from", m.from}, {"to", m.to}, {"cc", m.cc}} {
		if len(rs.addrs) > 0 {
			data.Recipients = append(data.Recipients, f.recipientSet(rs.kind, rs.addrs, !m.noEnvelope))
		}
	}
	id, err := f.st.PersistMessageContext(f.t.Context(), data)
	require.NoError(f.t, err)
	return id
}

func searchIDs(t *testing.T, st *store.Store, query string) []int64 {
	t.Helper()
	q := search.Parse(query)
	require.NoError(t, q.Err())
	results, _, err := st.SearchMessagesQuery(q, 0, 1000)
	require.NoError(t, err)
	ids := make([]int64, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	return ids
}

func attribution(t *testing.T, st *store.Store, id int64) (sql.NullString, sql.NullString) {
	t.Helper()
	var address, path sql.NullString
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT account_address, account_path FROM messages WHERE id = ?`), id).Scan(&address, &path))
	return address, path
}

func clearAccountPath(t *testing.T, st *store.Store, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		_, err := st.DB().Exec(st.Rebind(
			`UPDATE messages SET account_address = NULL, account_path = NULL WHERE id = ?`), id)
		require.NoError(t, err)
	}
}

func TestAccountAttributionPersistTiers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	aliases := []string{attrSink, "work@example.com", "mask@example.org", "second@example.com"}
	f.confirm(aliases...)
	want := map[string]string{
		"gmail": "work@example.com", "gmail-visible": "work@example.com", "pop": "work@example.com",
		"workspace": "work@example.com", "fastmail": "mask@example.org", "generic": "work@example.com",
		"bcc": "mask@example.org", "list": "mask@example.org", "ambiguous": "", "conflicting": "",
		"nested": attrSink, "malformed-mime": "work@example.com", "forwarded-inbox": "work@example.com",
	}
	ids := map[string]int64{}
	for name := range want {
		raw, err := os.ReadFile(filepath.Join("..", "emailattribution", "testdata", name+".eml"))
		require.NoError(err)
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		require.NoError(err)
		envelope := func(field string) []string {
			list, err := msg.Header.AddressList(field)
			if err != nil {
				return nil
			}
			var out []string
			for _, a := range list {
				out = append(out, strings.ToLower(a.Address))
			}
			return out
		}
		ids[name] = f.persist(attrMail{raw: string(raw), from: envelope("From"), to: envelope("To"), cc: envelope("Cc")})
	}
	for name, address := range want {
		for _, alias := range aliases {
			for _, op := range []string{"account:", "received:"} {
				found := slices.Contains(searchIDs(t, f.st, op+alias), ids[name])
				assert.Equal(alias == address, found, "%s %s%s", name, op, alias)
			}
		}
	}

	// Attribution never rewrites provenance: recipients, labels and raw bytes.
	id := ids["ambiguous"]
	countRecipients := func() int {
		var n int
		require.NoError(f.st.DB().QueryRow(f.st.Rebind(
			`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), id).Scan(&n))
		return n
	}
	recipientsBefore := countRecipients()
	labelsBefore, err := f.st.MessageLabelIDsContext(t.Context(), id)
	require.NoError(err)
	rawBefore, err := f.st.GetMessageRaw(id)
	require.NoError(err)
	_, err = f.st.RemoveAccountIdentity(f.source.ID, "second@example.com")
	require.NoError(err)
	assert.Equal([]int64{id}, intersect(searchIDs(t, f.st, "received:work@example.com"), []int64{id}),
		"removing one alias resolves the Cc conflict")
	assert.Equal(recipientsBefore, countRecipients())
	labelsAfter, err := f.st.MessageLabelIDsContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(labelsBefore, labelsAfter)
	rawAfter, err := f.st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(rawBefore, rawAfter)
}

func intersect(a, b []int64) []int64 {
	var out []int64
	for _, v := range a {
		if slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

func TestAccountAttributionUpsertMessageAndRawMIME(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	id, err := f.st.UpsertMessage(&store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "upsert-1", MessageType: "email",
	})
	require.NoError(err)
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))
	require.NoError(f.st.UpsertMessageRaw(id, []byte("X-Delivered-To: work@example.org\r\n\r\nbody")))
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))

	other, err := f.st.UpsertMessage(&store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "upsert-2", MessageType: "email",
	})
	require.NoError(err)
	called := false
	restore := f.st.SetAttributionAfterLockHookForTest(func([]int64) { called = true })
	defer restore()
	require.NoError(f.st.UpsertMessageRawWithFormat(other, []byte(`{"x":1}`), "beeper_json"))
	assert.False(called, "a non-MIME raw upsert takes no attribution lock")
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionFollowsRecipientAndRawReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org", "other@example.org")
	id := f.persist(attrMail{raw: "To: list@example.com\r\n\r\nbody", to: []string{"list@example.com"}})
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))

	require.NoError(f.st.ReplaceMessageRecipients(id, "to", []int64{f.participant("work@example.org")}, []string{""}))
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))

	require.NoError(f.st.UpsertMessageRawWithFormat(id,
		[]byte("X-Delivered-To: other@example.org\r\nTo: work@example.org\r\n\r\nbody"), "mime"))
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:other@example.org"))
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionSenderRepairDerivesSentCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	f.confirm(attrSink, "work@example.org")
	sent, err := f.st.EnsureLabel(f.source.ID, "SENT", "SENT", "system")
	require.NoError(err)
	raw := "From: Work <work@example.org>\r\nTo: friend@example.com\r\n\r\nbody"
	id := f.persist(attrMail{raw: raw, to: []string{"friend@example.com"}, labels: []int64{sent}})
	assert.Empty(searchIDs(t, f.st, "account:work@example.org"), "a sent copy without a sender has no account")

	candidates, err := f.st.ListMissingMIMESendersPageContext(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	require.NoError(f.st.ApplySenderRepairContext(t.Context(), id, candidates[0].RawMIMEFingerprint,
		[]mime.Address{{Name: "Work", Email: "work@example.org", Domain: "example.org"}}))
	assert.Equal([]int64{id}, searchIDs(t, f.st, "account:work@example.org"))
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionIdentityAndSinkChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "old@example.net")
	id := f.persist(attrMail{raw: "To: work@example.org\r\n\r\nbody", to: []string{"work@example.org"}})
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))
	f.confirm("work@example.org")
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))
	_, err := f.st.RemoveAccountIdentity(f.source.ID, "work@example.org")
	require.NoError(err)
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))

	conflict := f.persist(attrMail{raw: "To: a@example.org, b@example.org\r\n\r\nbody", to: []string{"a@example.org", "b@example.org"}})
	f.confirm("a@example.org", "b@example.org")
	assert.Empty(searchIDs(t, f.st, "account:a@example.org"))
	_, err = f.st.RemoveAccountIdentity(f.source.ID, "b@example.org")
	require.NoError(err)
	assert.Equal([]int64{conflict}, searchIDs(t, f.st, "account:a@example.org"))

	plain := f.persist(attrMail{raw: "To: nobody@example.com\r\n\r\nbody", to: []string{"nobody@example.com"}})
	f.confirm("old@example.net", "new@example.net")
	assert.Contains(searchIDs(t, f.st, "received:old@example.net"), plain)
	require.NoError(f.st.UpdateSourceIdentifier(f.source.ID, "new@example.net"))
	assert.Contains(searchIDs(t, f.st, "received:new@example.net"), plain)
	assert.NotContains(searchIDs(t, f.st, "received:old@example.net"), plain)
	assert.Contains(searchIDs(t, f.st, "received:new@example.net"), id, "a row without confirmed evidence follows the new mailbox")
}

func TestAccountAttributionRepairResumes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	var ids []int64
	for range 3 {
		ids = append(ids, f.persist(attrMail{raw: "X-Delivered-To: work@example.org\r\n\r\nbody"}))
	}
	clearAccountPath(t, f.st, ids...)
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))
	f.st.SetAccountRepairPageSizeForTest(1)

	ctx, cancel := context.WithCancel(t.Context())
	first, err := f.st.RepairAccountAttributionContext(ctx, f.source.ID,
		func(store.AccountAttributionRepairSummary) { cancel() })
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int64(1), first.Scanned)

	second, err := f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), second.Scanned, "the committed first page is not rescanned")
	assert.Equal(ids, searchIDs(t, f.st, "received:work@example.org"))

	third, err := f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Zero(third.Scanned, "a derived source has nothing pending")
}

func zlibBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, err := w.Write(raw)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func writeRawMIME(t *testing.T, st *store.Store, id int64, data []byte) {
	t.Helper()
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), id)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression) VALUES (?, ?, 'mime', 'zlib')`),
		id, data)
	require.NoError(t, err)
}

func TestAccountAttributionRepairRecordsCorruptRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	var ids []int64
	for range 3 {
		ids = append(ids, f.persist(attrMail{raw: "To: work@example.org\r\n\r\nbody", to: []string{"work@example.org"}}))
	}
	corrupt := zlibBytes(t, []byte("X-Delivered-To: work@example.org\r\n\r\nbody"))
	corrupt[len(corrupt)-1] ^= 0xff // adler32 checksum, not a truncation
	writeRawMIME(t, f.st, ids[1], corrupt)
	clearAccountPath(t, f.st, ids...)

	sum, err := f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.Undecodable)
	assert.Equal(ids, searchIDs(t, f.st, "received:work@example.org"), "the corrupt row still derives from To/Cc")
}

func TestAccountAttributionLargeBodyHeaderOnlyRead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	id := f.persist(attrMail{raw: "Subject: placeholder\r\n\r\nbody"})
	header := "X-Delivered-To: work@example.org\r\nSubject: " + strings.Repeat("h", 900) + "\r\n\r\n"
	// Hash-chained bytes do not compress, so the stored stream stays large.
	body := make([]byte, 0, 4<<20)
	block := sha256.Sum256([]byte("seed"))
	for len(body) < 4<<20 {
		block = sha256.Sum256(block[:])
		body = append(body, block[:]...)
	}
	compressed := zlibBytes(t, append([]byte(header), body...))
	require.Greater(len(compressed), 1<<20)
	writeRawMIME(t, f.st, id, compressed[:1<<20])
	clearAccountPath(t, f.st, id)

	sum, err := f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Undecodable)
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionMalformedValueBesideValidHeader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	id := f.persist(attrMail{raw: "X-Delivered-To: <<bad\r\nX-Original-To: work@example.org\r\n\r\nbody"})
	clearAccountPath(t, f.st, id)
	sum, err := f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.Undecodable)
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionMultiChunkConfirmation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	alias := func(i int) string { return fmt.Sprintf("alias%03d@example.org", i) }
	viaDelivery := f.persist(attrMail{raw: "X-Delivered-To: " + alias(590) + "\r\n\r\nbody"})
	viaEnvelope := f.persist(attrMail{raw: "To: " + alias(595) + "\r\n\r\nbody", to: []string{alias(595)}})
	viaFallback := f.persist(attrMail{to: []string{alias(599)}, noEnvelope: true})
	confirmations := make([]store.IdentityConfirmation, 0, 600)
	for i := range 600 {
		confirmations = append(confirmations, store.IdentityConfirmation{Identifier: alias(i), Signals: []string{"manual"}})
	}
	_, err := f.st.AddAccountIdentitiesBatchContext(t.Context(), f.source.ID, confirmations)
	require.NoError(err)
	assert.Equal([]int64{viaDelivery}, searchIDs(t, f.st, "received:"+alias(590)))
	assert.Equal([]int64{viaEnvelope}, searchIDs(t, f.st, "received:"+alias(595)))
	assert.Equal([]int64{viaFallback}, searchIDs(t, f.st, "received:"+alias(599)))
}

func TestAccountAttributionSentEvidenceDecidesDirection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	f.confirm(attrSink)
	sent, err := f.st.EnsureLabel(f.source.ID, "SENT", "SENT", "system")
	require.NoError(err)
	sentCopy := f.persist(attrMail{
		raw:  "From: stranger@example.com\r\nDelivered-To: " + attrSink + "\r\n\r\nbody",
		from: []string{"stranger@example.com"}, labels: []int64{sent},
	})
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), sentCopy)
	assert.NotContains(searchIDs(t, f.st, "account:"+attrSink), sentCopy, "sent copies never take the source default")

	inbound := f.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody"})
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), inbound)
	require.NoError(f.st.AddMessageLabels(inbound, []int64{sent}))
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), inbound)

	imap := newAttrFixtureOn(t, f.st, "imap", "imaps://"+strings.Replace(attrSink, "@", "%40", 1)+"@mail.example.net:993")
	imap.confirm(attrSink)
	_, err = f.st.EnsureLabelsBatch(imap.source.ID, map[string]store.LabelInfo{
		"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	id := imap.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", sourceMsgKey: "imap-1"})
	state := func(mailbox string, next uint32) store.IMAPFolderState {
		return store.IMAPFolderState{Mailbox: mailbox, UIDValidity: 7, UIDNext: next}
	}
	require.NoError(f.st.ApplyIMAPMailboxDeltas(imap.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "INBOX", State: state("INBOX", 11), Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 7, UID: 10, SourceMessageID: "imap-1"}}},
		{Mailbox: "Sent", State: state("Sent", 11), Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "Sent", UIDValidity: 7, UID: 10, SourceMessageID: "imap-1"}}},
	}))
	_, path := attribution(t, f.st, id)
	assert.Equal("sent", path.String, "joining the Sent mailbox makes it a sent copy")
	require.NoError(f.st.ApplyIMAPMailboxDeltas(imap.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "Sent", State: state("Sent", 12), VanishedUIDs: []uint32{10}},
	}))
	address, path := attribution(t, f.st, id)
	assert.Equal("inbound", path.String, "leaving the Sent mailbox moves it back")
	assert.Equal(attrSink, address.String)
}

func TestAccountAttributionDraftsAreNotReceived(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	f.confirm(attrSink, "alias@example.net")
	draft, err := f.st.EnsureLabel(f.source.ID, "DRAFT", "DRAFT", "system")
	require.NoError(err)

	// A header-free draft to an outside address would otherwise fall back to
	// the source mailbox.
	external := f.persist(attrMail{
		raw:  "From: " + attrSink + "\r\nTo: friend@example.com\r\n\r\nbody",
		from: []string{attrSink}, to: []string{"friend@example.com"}, labels: []int64{draft},
	})
	// A draft to a confirmed alias would otherwise match the visible recipient.
	toAlias := f.persist(attrMail{
		raw:  "From: " + attrSink + "\r\nTo: alias@example.net\r\n\r\nbody",
		from: []string{attrSink}, to: []string{"alias@example.net"}, labels: []int64{draft},
	})
	assert.Empty(searchIDs(t, f.st, "received:"+attrSink))
	assert.Empty(searchIDs(t, f.st, "received:alias@example.net"))
	assert.Equal([]int64{external, toAlias}, searchIDs(t, f.st, "account:"+attrSink), "drafts belong to their author")

	// Sending moves the draft to SENT; it stays out of received:.
	sent, err := f.st.EnsureLabel(f.source.ID, "SENT", "SENT", "system")
	require.NoError(err)
	require.NoError(f.st.AddMessageLabels(external, []int64{sent}))
	require.NoError(f.st.RemoveMessageLabels(external, []int64{draft}))
	address, path := attribution(t, f.st, external)
	assert.Equal(attrSink, address.String)
	assert.Equal("sent", path.String)

	// An IMAP mailbox that sync later learns is \Drafts re-derives its members.
	imap := newAttrFixtureOn(t, f.st, "imap", "imaps://"+strings.Replace(attrSink, "@", "%40", 1)+"@mail.example.net:993")
	imap.confirm(attrSink)
	labels, err := f.st.EnsureLabelsBatch(imap.source.ID, map[string]store.LabelInfo{
		"Drafts": {Name: "Drafts", Type: "system"},
	})
	require.NoError(err)
	id := imap.persist(attrMail{raw: "To: friend@example.com\r\n\r\nbody", to: []string{"friend@example.com"}, labels: []int64{labels["Drafts"]}})
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)
	_, err = f.st.EnsureLabelsBatch(imap.source.ID, map[string]store.LabelInfo{
		"Drafts": {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
	})
	require.NoError(err)
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), id)

	// A server that gives its drafts mailbox no role still flags each draft.
	plain := newAttrFixtureOn(t, f.st, "imap", "imaps://plain%40example.net@mail.example.net:993")
	plain.confirm("plain@example.net")
	draftID := plain.persist(attrMail{raw: "To: friend@example.com\r\n\r\nbody", to: []string{"friend@example.com"}, sourceMsgKey: "plain-1"})
	assert.Contains(searchIDs(t, f.st, "received:plain@example.net"), draftID)
	folder := store.IMAPFolderState{Mailbox: "Brouillons", UIDValidity: 3, UIDNext: 2}
	require.NoError(f.st.ApplyIMAPMailboxDeltas(plain.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "Brouillons", State: folder, Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "Brouillons", UIDValidity: 3, UID: 1, SourceMessageID: "plain-1", Flags: []string{"\\Draft", "\\Seen"}}}},
	}))
	assert.NotContains(searchIDs(t, f.st, "received:plain@example.net"), draftID, "the \\Draft flag marks it written")
	_, path = attribution(t, f.st, draftID)
	assert.Equal("sent", path.String)
}

func TestAccountAttributionLabelDefinitionChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", attrSink)
	f.confirm(attrSink)
	labels, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Outbox": {Name: "Outbox", Type: "system"},
	})
	require.NoError(err)
	id := f.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", labels: []int64{labels["Outbox"]}})
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)
	_, err = f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Outbox": {Name: "Outbox", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), id, "setting the Sent role re-derives members")
	_, err = f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Outbox": {Name: "Outbox", Type: "system"},
	})
	require.NoError(err)
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id, "clearing the Sent role re-derives members")

	// A Gmail name upsert that turns a label into SENT.
	g := newAttrFixtureOn(t, f.st, "gmail", "gmail-owner@example.net")
	g.confirm("gmail-owner@example.net")
	userLabel, err := f.st.EnsureLabel(g.source.ID, "Label_7", "Sent Mail", "user")
	require.NoError(err)
	gid := g.persist(attrMail{raw: "Delivered-To: gmail-owner@example.net\r\n\r\nbody", labels: []int64{userLabel}})
	assert.Contains(searchIDs(t, f.st, "received:gmail-owner@example.net"), gid)
	_, err = f.st.EnsureLabel(g.source.ID, "SENT", "Sent Mail", "system")
	require.NoError(err)
	assert.NotContains(searchIDs(t, f.st, "received:gmail-owner@example.net"), gid)

	// mergeLabelByName folds a Sent label into a non-Sent one.
	m := newAttrFixtureOn(t, f.st, "mbox", "merge@example.net")
	m.confirm("merge@example.net")
	merged, err := f.st.EnsureLabelsBatch(m.source.ID, map[string]store.LabelInfo{
		"sent-folder": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"keep":        {Name: "Keep", Type: "user"},
	})
	require.NoError(err)
	mid := m.persist(attrMail{raw: "Delivered-To: merge@example.net\r\n\r\nbody", labels: []int64{merged["sent-folder"]}})
	assert.NotContains(searchIDs(t, f.st, "received:merge@example.net"), mid)
	_, err = f.st.EnsureLabel(m.source.ID, "keep", "Sent", "user")
	require.NoError(err)
	assert.Contains(searchIDs(t, f.st, "received:merge@example.net"), mid, "the merged-away Sent label no longer marks its members")

	// Removing a message's last Sent label.
	require.NoError(f.st.ReplaceMessageLabels(gid, nil))
	assert.Contains(searchIDs(t, f.st, "received:gmail-owner@example.net"), gid)
}

func TestAccountAttributionLegacySenderFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	sent, err := f.st.EnsureLabel(f.source.ID, "SENT", "SENT", "system")
	require.NoError(err)
	sender := f.participant("legacy@example.org")
	id := f.persist(attrMail{senderID: sender, labels: []int64{sent}})
	assert.Empty(searchIDs(t, f.st, "account:legacy@example.org"))
	f.confirm("legacy@example.org")
	assert.Equal([]int64{id}, searchIDs(t, f.st, "account:legacy@example.org"))

	survivor := f.participant("survivor@example.org")
	require.NoError(f.st.MergeParticipants(sender, survivor))
	assert.Empty(searchIDs(t, f.st, "account:legacy@example.org"), "the fallback follows the merge survivor")
	require.NoError(f.st.RepairParticipantEmailAddresses([]store.ParticipantEmailRepair{
		{ParticipantID: survivor, EmailAddress: "legacy@example.org"},
	}))
	assert.Equal([]int64{id}, searchIDs(t, f.st, "account:legacy@example.org"))
}

func TestAccountAttributionNormalizesMatchedIdentities(t *testing.T) {
	assert := assert.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	f.confirm("Work@Example.org")
	sent, err := f.st.EnsureLabel(f.source.ID, "SENT", "SENT", "system")
	require.NoError(t, err)
	sentCopy := f.persist(attrMail{raw: "From: work@example.org\r\n\r\nbody", from: []string{"work@example.org"}, labels: []int64{sent}})
	inbound := f.persist(attrMail{raw: "To: WORK@example.org\r\n\r\nbody", to: []string{"WORK@example.org"}})
	assert.Equal([]int64{sentCopy, inbound}, searchIDs(t, f.st, "account:work@example.org"))
	assert.Equal([]int64{inbound}, searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionCalendarMapping(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	event := func(cfg, key string, confirm ...string) int64 {
		f := newAttrFixtureOn(t, st, "gcal", key)
		require.NoError(st.UpdateSourceSyncConfig(f.source.ID, cfg))
		f.confirm(confirm...)
		id, err := st.UpsertMessage(&store.Message{
			SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "event-" + key, MessageType: "calendar_event",
		})
		require.NoError(err)
		return id
	}
	primary := event(`{"account_email":"owner@example.org","calendar_id":"owner@example.org","primary":true}`, "cal-primary", "owner@example.org")
	shared := event(`{"account_email":"owner@example.org","calendar_id":"team@example.org"}`, "cal-shared", "owner@example.org")
	group := event(`{"account_email":"owner@example.org","calendar_id":"abc@group.calendar.google.com"}`, "cal-group", "owner@example.org", "abc@group.calendar.google.com")
	assert.Equal([]int64{primary}, searchIDs(t, st, "account:owner@example.org"))
	assert.Empty(searchIDs(t, st, "account:team@example.org"))
	assert.Empty(searchIDs(t, st, "account:abc@group.calendar.google.com"))
	assert.Empty(searchIDs(t, st, "received:owner@example.org"), "calendar events are not received mail")
	for _, id := range []int64{shared, group} {
		_, path := attribution(t, st, id)
		assert.Equal("calendar", path.String)
	}
}

func TestAccountAttributionCalendarConfigChangeRederives(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "gcal", "cal-config")
	require.NoError(f.st.UpdateSourceSyncConfig(f.source.ID, `{"account_email":"owner@example.org","calendar_id":"primary"}`))
	f.confirm("owner@example.org", "team@example.org")
	id, err := f.st.UpsertMessage(&store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "event-config", MessageType: "calendar_event",
	})
	require.NoError(err)
	assert.Equal([]int64{id}, searchIDs(t, f.st, "account:owner@example.org"))

	require.NoError(f.st.UpdateSourceSyncConfig(f.source.ID, `{"account_email":"owner@example.org","calendar_id":"team@example.org"}`))
	assert.Empty(searchIDs(t, f.st, "account:owner@example.org"))
	assert.Equal([]int64{id}, searchIDs(t, f.st, "account:team@example.org"))
}

func TestAccountAttributionRawFormatReplacementClearsDeliveryEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	id := f.persist(attrMail{raw: "X-Delivered-To: work@example.org\r\n\r\nbody"})
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))

	require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte(`{"x":1}`), "beeper_json"))
	assert.Empty(searchIDs(t, f.st, "received:work@example.org"))
	var rows int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(
		`SELECT COUNT(*) FROM message_delivery_addresses WHERE message_id = ?`), id).Scan(&rows))
	assert.Zero(rows)

	persisted := f.persist(attrMail{raw: "X-Delivered-To: work@example.org\r\n\r\nbody", sourceMsgKey: "persist-raw"})
	assert.Contains(searchIDs(t, f.st, "received:work@example.org"), persisted)
	again := f.persist(attrMail{raw: `{"x":1}`, rawFormat: "beeper_json", sourceMsgKey: "persist-raw"})
	require.Equal(persisted, again)
	assert.NotContains(searchIDs(t, f.st, "received:work@example.org"), persisted)
}

func TestAccountAttributionMixedLineEndingsKeepOuterHeaders(t *testing.T) {
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	// LF outer headers; a CRLF blank line only deep inside the body.
	raw := "X-Delivered-To: work@example.org\nSubject: s\n\n" + strings.Repeat("b", 300<<10) + "\r\n\r\ntail"
	id := f.persist(attrMail{raw: raw})
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionMergeDuplicatesBackfillsDelivery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	survivor := f.persist(attrMail{})
	duplicate := f.persist(attrMail{raw: "X-Delivered-To: work@example.org\r\n\r\nbody"})
	assert.NotContains(searchIDs(t, f.st, "received:work@example.org"), survivor)
	_, err := f.st.MergeDuplicates(survivor, []int64{duplicate}, "batch-1")
	require.NoError(err)
	assert.Contains(searchIDs(t, f.st, "received:work@example.org"), survivor)
}

func TestAccountAttributionCopySubsetCarriesEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	src := testutil.NewSQLiteTestStore(t)
	f := newAttrFixtureOn(t, src, "mbox", "archive-1")
	id := f.persist(attrMail{raw: "X-Delivered-To: work@example.org\r\n\r\nbody"})
	dstDir := t.TempDir()
	_, err := store.CopySubset(store.DBPathForTest(src), dstDir, 10, true)
	require.NoError(err)
	dst, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dst.Close() }()
	require.NoError(dst.InitSchema())
	require.NoError(dst.AddAccountIdentity(f.source.ID, "work@example.org", "manual"))
	assert.Equal([]int64{id}, searchIDs(t, dst, "received:work@example.org"))
}

func TestAccountAttributionRefusesUnlockedDerivation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	other := newAttrFixtureOn(t, f.st, "mbox", "archive-2")
	id := f.persist(attrMail{raw: "To: work@example.org\r\n\r\nbody", to: []string{"work@example.org"}})
	clearAccountPath(t, f.st, id)

	err := f.st.RefreshAccountAttributionPlainTxForTest(t.Context(), id)
	require.ErrorIs(err, store.ErrAttributionLockMissingForTest)
	err = f.st.RefreshAccountAttributionLockingSourcesForTest(t.Context(), id, other.source.ID)
	require.ErrorIs(err, store.ErrAttributionLockMissingForTest)
	address, path := attribution(t, f.st, id)
	assert.False(address.Valid)
	assert.False(path.Valid)
}

func TestAccountAttributionRejectsSharedToExclusiveUpgrade(t *testing.T) {
	f := newAttrFixture(t, "mbox", "archive-1")
	err := f.st.LockIdentityInSharedAttributionTxForTest(t.Context(), f.source.ID)
	require.ErrorIs(t, err, store.ErrAttributionLockUpgradeForTest)
}

func TestAccountAttributionNonDerivedWritesKeepWorking(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "archive-1")
	f.confirm("work@example.org")
	meeting, err := f.st.PersistMessageWithParticipantsContext(t.Context(),
		[]store.ParticipantPersistData{{EmailAddress: "host@example.com", Domain: "example.com"}},
		func(ids []int64) *store.MessagePersistData {
			return &store.MessagePersistData{Message: &store.Message{
				SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "meeting-1",
				MessageType: "meeting_transcript", SenderID: sql.NullInt64{Int64: ids[0], Valid: true},
			}}
		})
	require.NoError(err)
	chat, err := f.st.UpsertMessage(&store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "chat-1", MessageType: "chat",
	})
	require.NoError(err)
	for _, id := range []int64{meeting, chat} {
		address, path := attribution(t, f.st, id)
		assert.False(address.Valid)
		assert.False(path.Valid)
	}

	email := f.persist(attrMail{raw: "To: work@example.org\r\n\r\nbody", to: []string{"work@example.org"}, sourceMsgKey: "retype-1"})
	assert.Equal([]int64{email}, searchIDs(t, f.st, "received:work@example.org"))
	_, err = f.st.UpsertMessage(&store.Message{
		SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "retype-1", MessageType: "chat",
	})
	require.NoError(err)
	address, path := attribution(t, f.st, email)
	assert.False(address.Valid, "a row retyped away from email loses its account")
	assert.False(path.Valid)
}

func TestAccountAttributionDraftsAndRelocation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const raw = "From: alice@example.com\r\nX-Delivered-To: work@example.org\r\nTo: user@example.com\r\n\r\nbody"

	st := testutil.NewTestStore(t)
	gsource, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(gsource.ID, "work@example.org", "manual"))
	receipt := store.GmailDraftReceipt{SourceID: gsource.ID, GmailDraftID: "d-1", GmailMessageID: "gm-1", ThreadID: "t-1"}
	people := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}
	gdraft, err := st.PersistGmailDraftContext(t.Context(), receipt, people, gmailTestBuild(gsource.ID, 0, receipt, []byte(raw)))
	require.NoError(err)
	assert.NotContains(searchIDs(t, st, "received:work@example.org"), gdraft.CurrentMessageID, "a Gmail draft was never received")
	_, path := attribution(t, st, gdraft.CurrentMessageID)
	assert.Equal("sent", path.String)

	isource, err := st.GetOrCreateSource("imap", "imap://alice@example.com:143")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(isource.ID, "work@example.org", "manual"))
	iconv, err := st.EnsureConversation(isource.ID, "draft-thread", "Draft")
	require.NoError(err)
	ireceipt := store.IMAPDraftReceipt{SourceID: isource.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 5}
	idraft, err := st.PersistIMAPDraftContext(t.Context(), ireceipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: isource.ID, SourceMessageID: store.IMAPDraftSourceMessageID(ireceipt),
				MessageType: store.MessageTypeEmail, ConversationID: iconv,
			},
			RawMIME: []byte(raw),
		}
	})
	require.NoError(err)
	assert.NotContains(searchIDs(t, st, "received:work@example.org"), idraft.CurrentMessageID, "an IMAP draft was never received")

	fixture := seedIMAPRelocationFixture(t)
	require.NoError(fixture.Store.AddAccountIdentity(fixture.SourceID, "work@example.org", "manual"))
	relocated, err := fixture.Scoped.PersistIMAPRelocationWithParticipantsContext(
		t.Context(), fixture.Guard, imapRelocationParticipants(),
		func(ids []int64) *store.MessagePersistData {
			data := relocatedIMAPMessageData(fixture, ids)
			data.RawMIME = []byte(raw)
			return data
		}, true,
	)
	require.NoError(err)
	assert.Contains(searchIDs(t, fixture.Store, "received:work@example.org"), relocated)
}

func TestAccountAttributionColumnIsIndexed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	for _, name := range []string{"idx_messages_account", "idx_messages_account_pending"} {
		query := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`
		if st.IsPostgreSQL() {
			query = `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?`
		}
		var count int
		require.NoError(st.DB().QueryRow(st.Rebind(query), name).Scan(&count))
		assert.Equal(1, count, name)
	}
	if st.IsPostgreSQL() {
		return
	}
	// Every sync looks for pending rows, so once a source is derived the
	// lookup must not scan it.
	f := newAttrFixtureOn(t, st, "mbox", "archive-1")
	for range 200 {
		f.persist(attrMail{raw: "To: friend@example.com\r\n\r\nbody", to: []string{"friend@example.com"}})
	}
	_, err := st.DB().Exec(`ANALYZE`)
	require.NoError(err)
	rows, err := st.DB().Query(`EXPLAIN QUERY PLAN `+store.AccountPendingQueryForTest, f.source.ID, 0, 500)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(rows.Err())
	assert.Contains(strings.Join(plan, "\n"), "idx_messages_account_pending")
}

func TestAccountAttributionSentCopyWithoutSentFolder(t *testing.T) {
	assert := assert.New(t)
	f := newAttrFixture(t, "mbox", "me@example.net")
	f.confirm("me@example.net")
	sent := f.persist(attrMail{raw: "From: me@example.net\r\nTo: friend@example.com\r\n\r\nbody",
		from: []string{"me@example.net"}, to: []string{"friend@example.com"}})
	received := f.persist(attrMail{raw: "From: friend@example.com\r\nTo: me@example.net\r\n\r\nbody",
		from: []string{"friend@example.com"}, to: []string{"me@example.net"}})
	assert.Equal([]int64{received}, searchIDs(t, f.st, "received:me@example.net"),
		"a sent copy from a file import is not received mail")
	assert.Equal([]int64{sent, received}, searchIDs(t, f.st, "account:me@example.net"))
}

func TestAccountAttributionDraftRecordMarksLegacyDraft(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://alice@example.com:143")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "bob@example.com", "manual"))
	conv, err := st.EnsureConversation(source.ID, "backfill", "Backfill")
	require.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	draft, err := st.PersistIMAPDraftContext(t.Context(), receipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				MessageType: store.MessageTypeEmail, ConversationID: conv,
			},
			RawMIME: []byte("To: bob@example.com\r\n\r\nbody"),
		}
	})
	require.NoError(err)
	// An archive from before account attribution: the draft record survives,
	// its flag and label do not, and the row is pending.
	db := st.DB()
	for _, stmt := range []string{
		`DELETE FROM imap_message_memberships WHERE message_id = ?`,
		`DELETE FROM message_labels WHERE message_id = ?`,
		`UPDATE messages SET draft_authored = FALSE, account_address = NULL, account_path = NULL WHERE id = ?`,
	} {
		_, err = db.Exec(st.Rebind(stmt), draft.CurrentMessageID)
		require.NoError(err)
	}
	_, err = st.RepairAccountAttributionContext(t.Context(), source.ID, nil)
	require.NoError(err)

	var authored bool
	var path sql.NullString
	require.NoError(db.QueryRow(st.Rebind(`SELECT draft_authored, account_path FROM messages WHERE id = ?`),
		draft.CurrentMessageID).Scan(&authored, &path))
	assert.True(authored, "the draft record is draft evidence")
	assert.Equal("sent", path.String)
	assert.NotContains(searchIDs(t, st, "received:bob@example.com"), draft.CurrentMessageID)
}

func TestDraftAuthorshipRecordedWithoutDirectionChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "imap", "imaps://"+strings.Replace(attrSink, "@", "%40", 1)+"@mail.example.net:993")
	f.confirm(attrSink, "friend@example.com")
	_, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	id := f.persist(attrMail{raw: "From: " + attrSink + "\r\nTo: friend@example.com\r\n\r\nbody",
		from: []string{attrSink}, to: []string{"friend@example.com"}, sourceMsgKey: "copy-1"})
	state := func(mailbox string, next uint32) store.IMAPFolderState {
		return store.IMAPFolderState{Mailbox: mailbox, UIDValidity: 7, UIDNext: next}
	}
	// Already written through Sent, the row gains a \Draft copy, so its
	// direction does not change.
	require.NoError(f.st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "Sent", State: state("Sent", 2), Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "Sent", UIDValidity: 7, UID: 1, SourceMessageID: "copy-1"}}},
	}))
	_, path := attribution(t, f.st, id)
	require.Equal("sent", path.String)
	require.NoError(f.st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "Sent", State: state("Sent", 2)},
		{Mailbox: "Scratch", State: state("Scratch", 2), Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "Scratch", UIDValidity: 7, UID: 1, SourceMessageID: "copy-1", Flags: []string{"\\Draft"}}}},
	}))
	require.NoError(f.st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "Sent", State: state("Sent", 3), VanishedUIDs: []uint32{1}},
		{Mailbox: "Scratch", State: state("Scratch", 3), VanishedUIDs: []uint32{1}},
	}))
	var address sql.NullString
	address, path = attribution(t, f.st, id)
	assert.Equal("sent", path.String, "a draft stays written after its evidence is gone")
	assert.Equal(attrSink, address.String)

	// A Sent label that becomes a Drafts label records authorship too.
	m := newAttrFixtureOn(t, f.st, "mbox", "relabel@example.net")
	m.confirm("relabel@example.net", "friend@example.com")
	labels, err := f.st.EnsureLabelsBatch(m.source.ID, map[string]store.LabelInfo{
		"Out": {Name: "Out", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	mid := m.persist(attrMail{raw: "From: relabel@example.net\r\nTo: friend@example.com\r\n\r\nbody",
		from: []string{"relabel@example.net"}, to: []string{"friend@example.com"}, labels: []int64{labels["Out"]}})
	for _, role := range []string{store.LabelSystemRoleDrafts, ""} {
		_, err = f.st.EnsureLabelsBatch(m.source.ID, map[string]store.LabelInfo{
			"Out": {Name: "Out", Type: "system", SystemRole: role},
		})
		require.NoError(err)
	}
	_, path = attribution(t, f.st, mid)
	assert.Equal("sent", path.String)
	assert.NotContains(searchIDs(t, f.st, "received:friend@example.com"), mid)
}

func TestAccountAttributionFollowsLegacyIdentityMigration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// A first file import stores its messages before the legacy [identity]
	// addresses migrate onto the new source.
	f := newAttrFixture(t, "mbox", "archive@example.net")
	id := f.persist(attrMail{raw: "X-Delivered-To: work@example.org\r\nTo: list@example.test\r\n\r\nbody",
		to: []string{"list@example.test"}})
	require.Empty(searchIDs(t, f.st, "received:work@example.org"))

	applied, deferred, _, _, err := f.st.MigrateLegacyIdentityConfig([]string{"work@example.org"})
	require.NoError(err)
	require.True(applied)
	require.False(deferred)
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:work@example.org"))
}

func TestAccountAttributionInboxCopyOfOwnPostWithSentFolder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "imap", "imaps://"+strings.Replace(attrSink, "@", "%40", 1)+"@mail.example.net:993")
	f.confirm(attrSink, "alias@example.org")
	labels, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Sent":  {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"INBOX": {Name: "INBOX", Type: "system"},
	})
	require.NoError(err)
	id := f.persist(attrMail{raw: "From: alias@example.org\r\nTo: list@example.test\r\n\r\nbody",
		from: []string{"alias@example.org"}, to: []string{"list@example.test"}, labels: []int64{labels["INBOX"]}})
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:"+attrSink),
		"a source with a Sent folder keeps its inbox copy of the user's own post received")
}

func TestAccountAttributionFirstSentFolderReturnsSourceToPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttrFixture(t, "imap", "imaps://"+strings.Replace(attrSink, "@", "%40", 1)+"@mail.example.net:993")
	f.confirm(attrSink)
	inbox, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "INBOX", Type: "system"},
	})
	require.NoError(err)
	id := f.persist(attrMail{raw: "From: " + attrSink + "\r\nTo: list@example.test\r\n\r\nbody",
		from: []string{attrSink}, to: []string{"list@example.test"}, labels: []int64{inbox["INBOX"]}})
	_, path := attribution(t, f.st, id)
	require.Equal("sent", path.String, "without a Sent folder, the user's own mail counts as sent")

	_, err = f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	_, path = attribution(t, f.st, id)
	assert.False(path.Valid, "the first Sent folder returns the source's mail to pending")
	_, err = f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Equal([]int64{id}, searchIDs(t, f.st, "received:"+attrSink))
}
