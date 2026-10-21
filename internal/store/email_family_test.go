package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func familyEvidence(family *store.EmailFamily) map[int64]string {
	evidence := map[int64]string{}
	for _, member := range family.Members {
		evidence[member.MessageID] = member.Evidence
	}
	return evidence
}

// forwardMIME wraps an original email as a message/rfc822 attachment.
func forwardMIME(messageID, originalID string) string {
	original := testemail.NewMessage().Header("Message-ID", "<"+originalID+">").Subject("Original").Bytes()
	return string(testemail.NewMessage().Header("Message-ID", "<"+messageID+">").
		WithAttachment("original.eml", "message/rfc822", original).Bytes())
}

func TestEmailFamilyFindsCrossAccountCopyByMessageID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	id := a.persist(attrMail{raw: "Message-ID: <shared@example.test>\r\nSubject: Quote\r\n\r\nBody", rfc822: "shared@example.test"})
	copyID := b.persist(attrMail{raw: "Message-ID: <shared@example.test>\r\nSubject: Quote\r\n\r\nBody", rfc822: "shared@example.test"})
	b.persist(attrMail{raw: "Message-ID: <unrelated@example.test>\r\n\r\n", rfc822: "unrelated@example.test"})

	// The copies name no other message, so only the Message-ID joins them.
	var keys int
	require.NoError(a.st.DB().QueryRow(`SELECT COUNT(*) FROM email_link_keys`).Scan(&keys))
	require.Zero(keys)

	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(id, family.AnchorMessageID)
	assert.False(family.Truncated)
	assert.Equal(map[int64]string{id: "anchor", copyID: "copy"}, familyEvidence(family))
	for _, member := range family.Members {
		assert.Equal("complete", member.LinkageOutcome)
	}
}

func TestEmailFamilyLabelsReplyForwardAndReferences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	c := newAttrFixtureOn(t, a.st, "imap", "c@example.test")
	anchor := a.persist(attrMail{raw: "Message-ID: <orig@example.test>\r\nIn-Reply-To: <parent@example.test>\r\nReferences: <root@example.test> <parent@example.test>\r\n\r\n", rfc822: "orig@example.test"})
	parent := b.persist(attrMail{raw: "Message-ID: <parent@example.test>\r\n\r\n", rfc822: "parent@example.test"})
	reply := b.persist(attrMail{raw: "Message-ID: <reply@example.test>\r\nIn-Reply-To: <orig@example.test>\r\n\r\n", rfc822: "reply@example.test"})
	forward := c.persist(attrMail{raw: forwardMIME("forward@example.test", "orig@example.test"), rfc822: "forward@example.test"})
	sibling := c.persist(attrMail{raw: "Message-ID: <sibling@example.test>\r\nReferences: <root@example.test>\r\n\r\n", rfc822: "sibling@example.test"})
	reply2 := a.persist(attrMail{raw: "Message-ID: <reply2@example.test>\r\nIn-Reply-To: <reply@example.test>\r\n\r\n", rfc822: "reply2@example.test"})
	c.persist(attrMail{raw: "Message-ID: <elsewhere@example.test>\r\n\r\n", rfc822: "elsewhere@example.test"})
	// A reply that names the original without angle brackets is still a reply.
	bare := b.persist(attrMail{raw: "Message-ID: <bare@example.test>\r\nIn-Reply-To: orig@example.test\r\n\r\n", rfc822: "bare@example.test"})

	family, err := a.st.EmailFamilyContext(t.Context(), anchor)
	require.NoError(err)
	assert.Equal(map[int64]string{
		bare:    "reply",
		anchor:  "anchor",
		parent:  "replied_to",
		reply:   "reply",
		forward: "forward",
		sibling: "references",
		reply2:  "indirect",
	}, familyEvidence(family))

	// From the forward, the original it attaches is the forwarded original.
	family, err = a.st.EmailFamilyContext(t.Context(), forward)
	require.NoError(err)
	assert.Equal("forwarded_original", familyEvidence(family)[anchor])
}

// After an upgrade a reply in another source can still be pending, and the
// lookup cannot see it yet. The family must say so rather than read complete.
func TestEmailFamilyFlagsPendingLinkageInScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	c := newAttrFixtureOn(t, a.st, "imap", "c@example.test")
	id := a.persist(attrMail{raw: "Message-ID: <opened@example.test>\r\n\r\n", rfc822: "opened@example.test"})
	reply := b.persist(attrMail{raw: "Message-ID: <reply@example.test>\r\nIn-Reply-To: <opened@example.test>\r\n\r\n", rfc822: "reply@example.test"})
	other := c.persist(attrMail{raw: "Message-ID: <other@example.test>\r\n\r\n", rfc822: "other@example.test"})
	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.False(family.PendingLinkage)

	for _, pending := range []int64{reply, other} {
		_, err = a.st.DB().Exec(a.st.Rebind(`DELETE FROM email_link_keys WHERE message_id = ?`), pending)
		require.NoError(err)
		_, err = a.st.DB().Exec(a.st.Rebind(`UPDATE messages SET email_linkage_outcome = NULL WHERE id = ?`), pending)
		require.NoError(err)
	}
	family, err = a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(map[int64]string{id: "anchor"}, familyEvidence(family), "the pending reply has no keys to find")
	assert.True(family.PendingLinkage)

	// A source outside the grant is not the caller's to wait for.
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{a.source.ID})
	family, err = a.st.EmailFamilyContext(ctx, id)
	require.NoError(err)
	assert.False(family.PendingLinkage)

	_, err = a.st.RepairEmailLinkageContext(t.Context(), b.source.ID, nil)
	require.NoError(err)
	_, err = a.st.RepairEmailLinkageContext(t.Context(), c.source.ID, nil)
	require.NoError(err)
	family, err = a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(map[int64]string{id: "anchor", reply: "reply"}, familyEvidence(family))
	assert.False(family.PendingLinkage)

	// A deleted email awaiting linkage is not a copy the walk could have missed.
	gone := a.persist(attrMail{raw: "Message-ID: <gone@example.test>\r\n\r\n", rfc822: "gone@example.test"})
	_, err = a.st.DB().Exec(a.st.Rebind(`UPDATE messages SET email_linkage_outcome = NULL, deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), gone)
	require.NoError(err)
	family, err = a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.False(family.PendingLinkage)

	// An email stored without its raw message stays pending, and says so.
	bare := a.persist(attrMail{})
	family, err = a.st.EmailFamilyContext(t.Context(), bare)
	require.NoError(err)
	require.Len(family.Members, 1)
	assert.Equal("pending", family.Members[0].LinkageOutcome)
	assert.True(family.PendingLinkage)
}

func TestEmailFamilyScopeExcludesSourcesAndDoesNotBridgeThem(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	c := newAttrFixtureOn(t, a.st, "imap", "c@example.test")
	// a names b, and b names c; a and c share nothing.
	id := a.persist(attrMail{raw: "Message-ID: <a@example.test>\r\nIn-Reply-To: <b@example.test>\r\n\r\n", rfc822: "a@example.test"})
	b.persist(attrMail{raw: "Message-ID: <b@example.test>\r\nIn-Reply-To: <c@example.test>\r\n\r\n", rfc822: "b@example.test"})
	cID := c.persist(attrMail{raw: "Message-ID: <c@example.test>\r\n\r\n", rfc822: "c@example.test"})
	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Len(family.Members, 3)

	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{a.source.ID, c.source.ID})
	family, err = a.st.EmailFamilyContext(ctx, id)
	require.NoError(err)
	assert.Equal([]int64{id}, []int64{family.Members[0].MessageID}, "c is reachable only through b")
	assert.Len(family.Members, 1)

	_, err = a.st.EmailFamilyContext(ctx, cID+100)
	require.ErrorIs(err, store.ErrMessageNotFound)
	outside := sourcescope.WithSourceIDs(t.Context(), []int64{c.source.ID})
	_, err = a.st.EmailFamilyContext(outside, id)
	require.ErrorIs(err, store.ErrMessageNotFound)
}

func TestEmailFamilyExcludesDeletedAndNonEmail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	id := a.persist(attrMail{raw: "Message-ID: <m@example.test>\r\n\r\n", rfc822: "m@example.test"})
	live := b.persist(attrMail{raw: "Message-ID: <m@example.test>\r\n\r\n", rfc822: "m@example.test"})
	deleted := b.persist(attrMail{raw: "Message-ID: <m@example.test>\r\n\r\n", rfc822: "m@example.test"})
	chat := b.persist(attrMail{raw: "Message-ID: <m@example.test>\r\n\r\n", rfc822: "m@example.test"})
	_, err := a.st.DB().Exec(a.st.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), deleted)
	require.NoError(err)
	_, err = a.st.DB().Exec(a.st.Rebind(`UPDATE messages SET message_type = 'chat' WHERE id = ?`), chat)
	require.NoError(err)

	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(map[int64]string{id: "anchor", live: "copy"}, familyEvidence(family))
	_, err = a.st.EmailFamilyContext(t.Context(), deleted)
	require.ErrorIs(err, store.ErrMessageNotFound)
	_, err = a.st.EmailFamilyContext(t.Context(), chat)
	require.ErrorIs(err, store.ErrMessageNotFound)
}

func TestEmailFamilyDraftReflectsCurrentEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "imaps://a%40example.test@mail.example.test:993")
	b := newAttrFixtureOn(t, a.st, "imap", "imaps://b%40example.test@mail.example.test:993")
	labels, err := b.st.EnsureLabelsBatch(b.source.ID, map[string]store.LabelInfo{
		"Drafts": {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
	})
	require.NoError(err)
	id := a.persist(attrMail{raw: "Message-ID: <orig@example.test>\r\n\r\n", rfc822: "orig@example.test"})
	draft := b.persist(attrMail{raw: "Message-ID: <draft@example.test>\r\nIn-Reply-To: <orig@example.test>\r\n\r\n", rfc822: "draft@example.test", labels: []int64{labels["Drafts"]}})
	sent := b.persist(attrMail{raw: "Message-ID: <sent@example.test>\r\nIn-Reply-To: <orig@example.test>\r\n\r\n", rfc822: "sent@example.test"})
	// A draft that was sent keeps draft_authored but has no draft evidence now.
	_, err = a.st.DB().Exec(a.st.Rebind(`UPDATE messages SET draft_authored = ? WHERE id = ?`), true, sent)
	require.NoError(err)

	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	drafts := map[int64]bool{}
	for _, member := range family.Members {
		drafts[member.MessageID] = member.Draft
	}
	assert.Equal(map[int64]bool{id: false, draft: true, sent: false}, drafts)
}

// The walk keeps 500 emails and says so. What it cuts first is the weakest
// relation: the same Message-ID outranks a reply, a reply outranks a thread
// member, and a long thread holding the opened email still reports the cut.
func TestEmailFamilyCapsAtTheMemberLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (st *store.Store, id int64, kept map[int64]string)
	}{
		{"copies of one Message-ID", func(t *testing.T) (*store.Store, int64, map[int64]string) {
			t.Helper()
			a := newAttrFixture(t, "imap", "a@example.test")
			id := a.persist(attrMail{raw: "Message-ID: <busy@example.test>\r\n\r\n", rfc822: "busy@example.test"})
			for i := range 600 {
				insertFamilyMail(t, a, fmt.Sprintf("copy-%d", i), "busy@example.test", a.conv, nil)
			}
			return a.st, id, map[int64]string{id: "anchor"}
		}},
		{"exact copy among many replies", func(t *testing.T) (*store.Store, int64, map[int64]string) {
			t.Helper()
			a := newAttrFixture(t, "imap", "a@example.test")
			b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
			c := newAttrFixtureOn(t, a.st, "imap", "c@example.test")
			id := a.persist(attrMail{raw: "Message-ID: <popular@example.test>\r\n\r\n", rfc822: "popular@example.test"})
			for i := range 600 {
				insertFamilyMail(t, c, fmt.Sprintf("reply-%d", i), fmt.Sprintf("r%d@example.test", i), c.conv, map[string][]string{"reply": {"popular@example.test"}})
			}
			// The copy has the highest ID, so cutting by ID would drop it first.
			copyID := b.persist(attrMail{raw: "Message-ID: <popular@example.test>\r\n\r\n", rfc822: "popular@example.test"})
			return a.st, id, map[int64]string{copyID: "copy"}
		}},
		{"reply among many thread members", func(t *testing.T) (*store.Store, int64, map[int64]string) {
			t.Helper()
			a := newAttrFixture(t, "gmail", "a@example.test")
			b := newAttrFixtureOn(t, a.st, "gmail", "b@example.test")
			c := newAttrFixtureOn(t, a.st, "gmail", "c@example.test")
			id := a.persist(attrMail{raw: "Message-ID: <opened@example.test>\r\n\r\n", rfc822: "opened@example.test"})
			exact := b.persist(attrMail{raw: "Message-ID: <opened@example.test>\r\n\r\n", rfc822: "opened@example.test"})
			for i := range 600 {
				insertFamilyMail(t, b, fmt.Sprintf("thread-%d", i), fmt.Sprintf("t%d@example.test", i), b.conv, nil)
			}
			reply := c.persist(attrMail{raw: "Message-ID: <reply@example.test>\r\nIn-Reply-To: <opened@example.test>\r\n\r\n", rfc822: "reply@example.test"})
			return a.st, id, map[int64]string{exact: "copy", reply: "reply"}
		}},
		{"long thread holding the opened email", func(t *testing.T) (*store.Store, int64, map[int64]string) {
			t.Helper()
			a := newAttrFixture(t, "gmail", "a@example.test")
			id := a.persist(attrMail{raw: "Message-ID: <opened@example.test>\r\n\r\n", rfc822: "opened@example.test"})
			for i := range 599 {
				insertFamilyMail(t, a, fmt.Sprintf("thread-%d", i), fmt.Sprintf("t%d@example.test", i), a.conv, nil)
			}
			return a.st, id, map[int64]string{id: "anchor"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			st, id, kept := tc.build(t)
			family, err := st.EmailFamilyContext(t.Context(), id)
			require.NoError(t, err)
			assert.Len(family.Members, 500)
			assert.True(family.Truncated)
			assert.Equal(store.EmailFamilyLimitMembers, family.TruncatedReason)
			evidence := familyEvidence(family)
			for member, want := range kept {
				assert.Equal(want, evidence[member])
			}
		})
	}
}

// The lookups must stay index-driven so a walk costs rows touched, not the
// archive's size. This fails on a per-identifier EXISTS probe, which scans
// every email once per identifier.
func TestEmailFamilyLookupPlansUseIndexes(t *testing.T) {
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	if a.st.IsPostgreSQL() {
		t.Skip("EXPLAIN QUERY PLAN is SQLite-specific")
	}
	for i := range 200 {
		a.persist(attrMail{
			raw:    fmt.Sprintf("Message-ID: <m%d@example.test>\r\nIn-Reply-To: <p%d@example.test>\r\n\r\n", i, i),
			rfc822: fmt.Sprintf("m%d@example.test", i),
		})
	}
	_, err := a.st.DB().Exec(`ANALYZE`)
	require.NoError(err)
	byOwnID, byKey := a.st.EmailFamilyLookupQueriesForTest("", "")
	_, byDirectKey := a.st.EmailFamilyLookupQueriesForTest("", store.EmailFamilyDirectKindsForTest)
	for _, tc := range []struct {
		name, query, index string
		args               []any
	}{
		{"own Message-ID", fmt.Sprintf(byOwnID, "?, ?"), "idx_messages_rfc822_message_id_canonical", []any{[]byte("m1@example.test"), []byte("m2@example.test")}},
		{"named identifier", fmt.Sprintf(byKey, "?, ?"), "idx_email_link_keys_value", []any{"p1@example.test", "p2@example.test"}},
		{"named directly", fmt.Sprintf(byDirectKey, "?"), "idx_email_link_keys_value", []any{"p1@example.test"}},
		{"conversation", store.EmailFamilyConversationQueryForTest("", 500), "idx_messages_conversation", []any{a.conv}},
		{"pending probe", store.EmailFamilyPendingQueryForTest(" AND source_id IN (?)"), "idx_messages_email_linkage_pending", []any{a.source.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := a.st.DB().Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
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
			text := strings.Join(plan, "\n")
			assertions := assert.New(t)
			assertions.Contains(text, tc.index)
			assertions.NotContains(text, "SCAN m", "a lookup must not scan the messages table")
			assertions.NotContains(text, "SCAN messages", "a lookup must not scan the messages table")
			assertions.NotContains(text, "SCAN k", "a lookup must not scan every identifier")
		})
	}
}

// insertFamilyMail adds an email without a raw payload, as bulk fixtures need,
// with its own Message-ID and the identifiers it names.
func insertFamilyMail(t *testing.T, f *attrFixture, key, ownID string, conv int64, keys map[string][]string) {
	t.Helper()
	var id int64
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`INSERT INTO messages (source_id, conversation_id, source_message_id, message_type, rfc822_message_id, email_linkage_outcome)
		VALUES (?, ?, ?, 'email', ?, 'complete') RETURNING id`), f.source.ID, conv, key, ownID).Scan(&id))
	for kind, values := range keys {
		for _, value := range values {
			_, err := f.st.DB().Exec(f.st.Rebind(`INSERT INTO email_link_keys (message_id, kind, value) VALUES (?, ?, ?)`), id, kind, value)
			require.NoError(t, err)
		}
	}
}

// The provider threads of the opened email and its exact copies join the
// family even when nothing in their headers links the members, so any copy
// returns the same family. The walk goes no further: the thread of a reply
// found along the way stays out.
func TestEmailFamilyIncludesThreadsOfExactCopies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "gmail", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "gmail", "b@example.test")
	c := newAttrFixtureOn(t, a.st, "gmail", "c@example.test")
	id := a.persist(attrMail{raw: "Message-ID: <opened@example.test>\r\n\r\n", rfc822: "opened@example.test"})
	sibling := a.persist(attrMail{raw: "Message-ID: <sibling@example.test>\r\n\r\n", rfc822: "sibling@example.test"})
	exact := b.persist(attrMail{raw: "Message-ID: <opened@example.test>\r\n\r\n", rfc822: "opened@example.test"})
	copySibling := b.persist(attrMail{raw: "Message-ID: <copy-sibling@example.test>\r\n\r\n", rfc822: "copy-sibling@example.test"})
	// A reply sits in a conversation with an unlinked email.
	reply := c.persist(attrMail{raw: "Message-ID: <reply@example.test>\r\nIn-Reply-To: <opened@example.test>\r\n\r\n", rfc822: "reply@example.test"})
	c.persist(attrMail{raw: "Message-ID: <bystander@example.test>\r\n\r\n", rfc822: "bystander@example.test"})

	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(map[int64]string{id: "anchor", sibling: "thread", exact: "copy", copySibling: "thread", reply: "reply"}, familyEvidence(family))

	family, err = a.st.EmailFamilyContext(t.Context(), exact)
	require.NoError(err)
	assert.Equal(map[int64]string{exact: "anchor", copySibling: "thread", id: "copy", sibling: "thread", reply: "reply"}, familyEvidence(family))
}

// Importers and IMAP derive a conversation from References, so a bad key such
// as "(note)" can join unrelated mail. Only a provider's thread is evidence,
// and the gate reads the source type, so archives that already hold such
// conversations stay safe.
func TestEmailFamilyIgnoresDerivedConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, sourceType := range []string{"imap", "mbox", "eml", "emlx", "pst", "maildir", "msmail"} {
		a := newAttrFixture(t, sourceType, "a@example.test")
		b := newAttrFixtureOn(t, a.st, sourceType, "b@example.test")
		id := a.persist(attrMail{raw: "Message-ID: <root-a@example.test>\r\nReferences: (note) <x@example.test>\r\n\r\n", rfc822: "root-a@example.test"})
		a.persist(attrMail{raw: "Message-ID: <root-b@example.test>\r\nReferences: (note) <y@example.test>\r\n\r\n", rfc822: "root-b@example.test"})
		b.persist(attrMail{raw: "Message-ID: <copy-b@example.test>\r\nIn-Reply-To: <root-b@example.test>\r\n\r\n", rfc822: "copy-b@example.test"})

		family, err := a.st.EmailFamilyContext(t.Context(), id)
		require.NoError(err)
		assert.Equal(map[int64]string{id: "anchor"}, familyEvidence(family), sourceType)
	}
}

// Reaching the identifier limit truncates a family of far fewer than 500
// emails, and says which limit it was.
func TestEmailFamilyReportsTheIdentifierLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a := newAttrFixture(t, "imap", "a@example.test")
	b := newAttrFixtureOn(t, a.st, "imap", "b@example.test")
	id := a.persist(attrMail{raw: "Message-ID: <hub@example.test>\r\n\r\n", rfc822: "hub@example.test"})
	for i := range 60 {
		refs := make([]string, 128)
		for j := range refs {
			refs[j] = fmt.Sprintf("ref-%d-%d@example.test", i, j)
		}
		b.persist(attrMail{
			raw:    fmt.Sprintf("Message-ID: <m%d@example.test>\r\nIn-Reply-To: <hub@example.test>\r\nReferences: <%s>\r\n\r\n", i, strings.Join(refs, "> <")),
			rfc822: fmt.Sprintf("m%d@example.test", i),
		})
	}
	family, err := a.st.EmailFamilyContext(t.Context(), id)
	require.NoError(err)
	assert.Len(family.Members, 61)
	assert.True(family.Truncated)
	assert.Equal(store.EmailFamilyLimitIdentifiers, family.TruncatedReason)
}

// A provider's thread joins the family even when its key is the first
// message's own ID (Gmail) or no header links the members, and the Google
// Groups thread key joins only the messages that carry it, even after a source
// merge has moved the conversation under an archive key. Opening any member
// finds the same siblings.
func TestEmailFamilyJoinsProviderThreads(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, threadKey, rootKey string
		merged                               bool
	}{
		{"gmail thread root", "gmail", "root-1", "root-1", false},
		{"google groups thread", "google-groups", "google-groups:" + strings.Repeat("ab", 32), "root-1", false},
		{"merged google groups thread", "google-groups", "google-groups:" + strings.Repeat("ab", 32), "root-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			a := newAttrFixture(t, tc.sourceType, "a@example.test")
			conv, err := a.st.EnsureConversation(a.source.ID, tc.threadKey, "Thread")
			require.NoError(err)
			a.conv = conv
			root := a.persist(attrMail{raw: "Message-ID: <root@example.test>\r\n\r\n", rfc822: "root@example.test", sourceMsgKey: tc.rootKey})
			sibling := a.persist(attrMail{raw: "Message-ID: <sibling@example.test>\r\n\r\n", rfc822: "sibling@example.test"})
			a.conv, err = a.st.EnsureConversation(a.source.ID, "other-thread", "Other")
			require.NoError(err)
			other := a.persist(attrMail{raw: "Message-ID: <other@example.test>\r\n\r\n", rfc822: "other@example.test"})

			if tc.merged {
				into, err := a.st.GetOrCreateSource(tc.sourceType, "into@example.test")
				require.NoError(err)
				_, err = a.st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: a.source.ID, IntoSourceID: into.ID})
				require.NoError(err)
			}

			family, err := a.st.EmailFamilyContext(t.Context(), root)
			require.NoError(err)
			assert.Equal(map[int64]string{root: "anchor", sibling: "thread"}, familyEvidence(family))
			family, err = a.st.EmailFamilyContext(t.Context(), sibling)
			require.NoError(err)
			assert.Equal(map[int64]string{root: "thread", sibling: "anchor"}, familyEvidence(family))
			assert.NotContains(familyEvidence(family), other)
		})
	}
}
