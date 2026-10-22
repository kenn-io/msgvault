package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func prepareEmailFamilyHandlingFixture(t *testing.T, f *attrFixture) {
	t.Helper()
	f.confirm("owner@example.test")
	_, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(t, err)
}

func persistAttributedIncoming(t *testing.T, f *attrFixture, messageID, replyTo string, second int) int64 {
	t.Helper()
	raw := fmt.Sprintf("Message-ID: <%s>\r\n", messageID)
	if replyTo != "" {
		raw += fmt.Sprintf("In-Reply-To: <%s>\r\nReferences: <%s>\r\n", replyTo, replyTo)
	}
	raw += "\r\n"
	id := f.persist(attrMail{raw: raw, from: []string{"client@example.test"}, to: []string{"owner@example.test"}})
	_, err := f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET internal_date=? WHERE id=?`),
		time.Date(2026, 1, 1, 0, 0, second, 0, time.UTC), id)
	require.NoError(t, err)
	return id
}

func TestEmailFamilyScopeExcludesBridgesAndUnknownSeed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	c := newAttrFixtureOn(t, a.st, "imap", "c@example.test")
	id := a.persist(attrMail{raw: "Message-ID: <a@example.test>\r\nIn-Reply-To: <b@example.test>\r\n\r\n"})
	b.persist(attrMail{raw: "Message-ID: <b@example.test>\r\nIn-Reply-To: <c@example.test>\r\n\r\n"})
	c.persist(attrMail{raw: "Message-ID: <c@example.test>\r\n\r\n"})
	got, err := a.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(got.Members, 3)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{a.source.ID, c.source.ID})
	got, err = a.st.EmailFamilyContext(ctx, id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(got.Members, 1)
	_, err = a.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{SourceIDs: []int64{}})
	require.ErrorIs(err, store.ErrNotFound)
	_, err = a.st.EmailFamilyContext(ctx, id, store.EmailFamilyOptions{SourceIDs: []int64{b.source.ID}})
	require.ErrorIs(err, store.ErrNotFound)
}

func TestEmailFamilyGmailThreadStaysWithinItsSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", "thread-owner@example.test")
	other := newAttrFixtureOn(t, f.st, "gmail", "thread-other@example.test")
	const providerThreadID = "shared-provider-thread"
	var err error
	f.conv, err = f.st.EnsureConversation(f.source.ID, providerThreadID, "Shared thread")
	require.NoError(err)
	other.conv, err = f.st.EnsureConversation(other.source.ID, providerThreadID, "Shared thread")
	require.NoError(err)
	first := f.persist(attrMail{raw: "Message-ID: <thread-first@example.test>\r\n\r\n"})
	second := f.persist(attrMail{raw: "Message-ID: <thread-second@example.test>\r\n\r\n"})
	otherSource := other.persist(attrMail{raw: "Message-ID: <other-thread@example.test>\r\n\r\n"})

	got, err := f.st.EmailFamilyContext(t.Context(), first, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal([]int64{first, second}, memberIDs(got.Members))
	assert.NotContains(memberIDs(got.Members), otherSource)
}

func TestEmailFamilyRevisionTracksOnlyRelatedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "revision@example.test")
	id := f.persist(attrMail{raw: "Message-ID: <a@example.test>\r\n\r\n"})
	before, err := f.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	f.persist(attrMail{raw: "Message-ID: <unrelated@example.test>\r\n\r\n"})
	after, err := f.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal(before.Revision, after.Revision)
	require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte("Message-ID: <a@example.test>\r\nReferences: <missing@example.test>\r\n\r\n"), "mime"))
	after, err = f.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.NotEqual(before.Revision, after.Revision)
	assert.Equal("unknown", after.Handling.State)
}

func TestEmailFamilyRevisionUsesAPIInvalidUTF8Replacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "invalid-utf8@example.test")
	if f.st.IsPostgreSQL() {
		t.Skip("PostgreSQL text cannot store invalid UTF-8; this corruption regression is SQLite-specific")
	}
	id := f.persist(attrMail{raw: "Message-ID: <invalid-utf8@example.test>\r\n\r\n"})
	invalidSnippet := "damaged \xff snippet"
	replacementSnippet := "damaged \uFFFD snippet"
	_, err := f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET snippet=? WHERE id=?`), invalidSnippet, id)
	require.NoError(err)

	invalid, err := f.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.NotEmpty(invalid.Revision)

	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET snippet=? WHERE id=?`), replacementSnippet, id)
	require.NoError(err)
	replacement, err := f.st.EmailFamilyContext(t.Context(), id, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal(replacement.Revision, invalid.Revision, "invalid UTF-8 hashes as the API's U+FFFD replacement")
}

func TestEmailFamilyPositiveSentReplyAndImmutableEnvelopes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", "owner@example.test")
	f.confirm("owner@example.test")
	incoming := f.persist(attrMail{raw: "Message-ID: <request@example.test>\r\n\r\n", from: []string{"client@example.test"}, to: []string{"owner@example.test"}})
	reply := f.persist(attrMail{raw: "Message-ID: <reply@example.test>\r\nIn-Reply-To: <request@example.test>\r\n\r\n", from: []string{"owner@example.test"}, to: []string{"client@example.test"}})
	sent, err := f.st.EnsureLabel(f.source.ID, "SENT", "Sent", "system")
	require.NoError(err)
	require.NoError(f.st.AddMessageLabels(reply, []int64{sent}))
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), "2026-01-01 10:00:00", incoming)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), "2026-01-01 11:00:00", reply)
	require.NoError(err)
	got, err := f.st.EmailFamilyContext(t.Context(), incoming, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal("handled", got.Handling.State)
	assert.Equal([]int64{reply}, got.Handling.OutboundEvidenceIDs)
	before := got.Revision
	require.NoError(f.st.MergeParticipants(f.participant("client@example.test"), f.participant("owner@example.test")))
	got, err = f.st.EmailFamilyContext(t.Context(), incoming, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal("handled", got.Handling.State)
	assert.Equal(before, got.Revision)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_recipients SET email_address=NULL WHERE message_id=?`), reply)
	require.NoError(err)
	got, err = f.st.EmailFamilyContext(t.Context(), incoming, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal("unknown", got.Handling.State)
	assert.NotEqual(before, got.Revision)
}

func TestEmailFamilySentReplyIsNotDraftAfterDraftRetirement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	f.confirm("owner@example.test")
	labels, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Sent":   {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"Drafts": {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
	})
	require.NoError(err)
	request := persistAttributedIncoming(t, f, "draft-request@example.test", "", 1)
	reply := f.persist(attrMail{
		raw:    "Message-ID: <draft-reply@example.test>\r\nIn-Reply-To: <draft-request@example.test>\r\nReferences: <draft-request@example.test>\r\n\r\n",
		from:   []string{"owner@example.test"},
		to:     []string{"client@example.test"},
		labels: []int64{labels["Drafts"]},
	})
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET internal_date=? WHERE id=?`),
		time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC), reply)
	require.NoError(err)
	before, err := f.st.EmailFamilyContext(t.Context(), request, store.EmailFamilyOptions{})
	require.NoError(err)
	var draftMember store.EmailFamilyMember
	for _, member := range before.Members {
		if member.MessageID == reply {
			draftMember = member
			break
		}
	}
	require.Equal(reply, draftMember.MessageID)
	assert.True(draftMember.Draft)
	require.NoError(f.st.AddMessageLabels(reply, []int64{labels["Sent"]}))
	require.NoError(f.st.RemoveMessageLabels(reply, []int64{labels["Drafts"]}))
	var authored bool
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT draft_authored FROM messages WHERE id=?`), reply).Scan(&authored))
	assert.True(authored, "draft authorship remains as historical evidence")

	got, err := f.st.EmailFamilyContext(t.Context(), request, store.EmailFamilyOptions{})
	require.NoError(err)
	var replyMember store.EmailFamilyMember
	for _, member := range got.Members {
		if member.MessageID == reply {
			replyMember = member
			break
		}
	}
	require.Equal(reply, replyMember.MessageID)
	assert.False(replyMember.Draft)
	assert.True(replyMember.PositiveSent)
	assert.Equal("handled", got.Handling.State)
	assert.Equal([]int64{reply}, got.Handling.OutboundEvidenceIDs)
}

func TestEmailFamilyBccOnlyIncomingUsesConfirmedAccountWithoutRecipientRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "owner@example.test")
	f.confirm("owner@example.test")
	labels, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"Sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	// The archive has no recipient row because the provider exposed only the
	// undisclosed-recipient placeholder; delivery attribution still confirms the account.
	request := f.persist(attrMail{
		raw:  "Message-ID: <bcc-request@example.test>\r\nFrom: client@example.test\r\nTo: undisclosed-recipients:;\r\nDelivered-To: owner@example.test\r\n\r\n",
		from: []string{"client@example.test"},
	})
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET internal_date=? WHERE id=?`),
		time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), request)
	require.NoError(err)
	reply := f.persist(attrMail{
		raw:    "Message-ID: <bcc-reply@example.test>\r\nIn-Reply-To: <bcc-request@example.test>\r\nReferences: <bcc-request@example.test>\r\n\r\n",
		from:   []string{"owner@example.test"},
		to:     []string{"client@example.test"},
		labels: []int64{labels["Sent"]},
	})
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET internal_date=? WHERE id=?`),
		time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC), reply)
	require.NoError(err)

	got, err := f.st.EmailFamilyContext(t.Context(), request, store.EmailFamilyOptions{})
	require.NoError(err)
	var incoming store.EmailFamilyMember
	for _, member := range got.Members {
		if member.MessageID == request {
			incoming = member
			break
		}
	}
	require.Equal(request, incoming.MessageID)
	assert.Equal("inbound", incoming.AccountPath)
	assert.Equal("owner@example.test", incoming.AccountAddress)
	assert.True(incoming.Incoming)
	assert.True(incoming.AttributionComplete)
	assert.False(incoming.ExternalRecipient)
	assert.Equal([]store.EmailFamilyEnvelope{{Kind: "from", Address: "client@example.test"}}, incoming.Envelopes)
	assert.Equal("handled", got.Handling.State)
	assert.Equal([]int64{reply}, got.Handling.OutboundEvidenceIDs)
}

func TestEmailFamilyBoundsPopulationAndRevisionIsStable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "bounded@example.test")
	prepareEmailFamilyHandlingFixture(t, f)
	var seed int64
	for i := range 500 {
		id := persistAttributedIncoming(t, f, "shared@example.test", "", i+1)
		if i == 0 {
			seed = id
		}
	}
	belowLimit, err := f.st.EmailFamilyContext(t.Context(), seed, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(belowLimit.Members, 500)
	assert.False(belowLimit.Truncated)
	assert.Equal("actionable", belowLimit.Handling.State)
	assert.Len(belowLimit.Handling.OutstandingEventIDs, 1)
	extra := persistAttributedIncoming(t, f, "shared@example.test", "", 501)
	got, err := f.st.EmailFamilyContext(t.Context(), seed, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(got.Members, 500)
	assert.NotContains(memberIDs(got.Members), extra)
	assert.True(got.Truncated)
	assert.Equal("unknown", got.Handling.State)
	assert.Contains(got.Handling.Reasons, "family_truncated")
	assert.NotEqual(belowLimit.Revision, got.Revision)
	again, err := f.st.EmailFamilyContext(t.Context(), seed, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Equal(got.Revision, again.Revision)
}

func TestEmailFamilyLinkageLimitCountsMessagesNotMatchingKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "overlapping-keys@example.test")
	prepareEmailFamilyHandlingFixture(t, f)
	const rootMessageID = "root@example.test"
	root := persistAttributedIncoming(t, f, rootMessageID, "", 1)
	var last int64
	for i := range 251 {
		last = persistAttributedIncoming(t, f, fmt.Sprintf("reply-%d@example.test", i), rootMessageID, i+2)
	}

	got, err := f.st.EmailFamilyContext(t.Context(), root, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(got.Members, 252)
	assert.Equal(last, got.Members[len(got.Members)-1].MessageID)
	assert.False(got.Truncated)
	assert.Equal("actionable", got.Handling.State)
}

func memberIDs(members []store.EmailFamilyMember) []int64 {
	ids := make([]int64, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.MessageID)
	}
	return ids
}
