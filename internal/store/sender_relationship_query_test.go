package store

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSenderContactRelationshipsQueryUsesNormalizedEnvelopeIndex(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "sender-relationships-query.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())

	query, args := s.senderContactRelationshipsQuery([]string{"one@example.test", "target@example.test"}, nil, false, 0)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(rows.Err())

	plan := strings.Join(details, "\n")
	assertions.Contains(plan, "SEARCH mr USING",
		"contact and archive-history lookup should use an address lookup rather than scan the recipient index:\n%s", plan)
	assertions.Contains(plan, "idx_message_recipients_email_address_normalized",
		"contact and archive-history lookup should probe historical recipient envelopes by normalized address:\n%s", plan)
	assertions.Contains(plan, "idx_msmail_sent_envelope_address",
		"contact and archive-history lookup should probe preserved Sent envelopes by address:\n%s", plan)
	assertions.Equal(3, strings.Count(plan, "idx_participants_email_normalized"),
		"all unscoped pointer-history probes should use the normalized participant email index:\n%s", plan)
}

func TestSenderHistoryMessageIDsQueryUsesNormalizedEnvelopeIndex(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "sender-outbound-query.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())

	query, args := s.senderHistoryMessageIDsQuery("target@example.test", nil, 0, 0, 257)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(rows.Err())

	plan := strings.Join(details, "\n")
	assertions.Contains(plan, "SEARCH mr USING",
		"outbound-history lookup should use an address lookup rather than scan the recipient index:\n%s", plan)
	assertions.Contains(plan, "idx_message_recipients_email_address_normalized",
		"history paging should probe recipient envelopes by normalized address:\n%s", plan)
	assertions.NotContains(plan, "USE TEMP B-TREE FOR ORDER BY",
		"history paging should use the index's message ID order:\n%s", plan)
}

func TestSenderSentEnvelopeHistoryMessageIDsQueryUsesAddressIndex(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "sender-sent-envelope-query.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())

	query, args := s.senderSentEnvelopeHistoryMessageIDsQuery("target@example.test", nil, 0, 0, 257)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(rows.Err())

	plan := strings.Join(details, "\n")
	assertions.Contains(plan, "idx_msmail_sent_envelope_address",
		"Sent envelope history should use the address-first index:\n%s", plan)
	assertions.NotContains(plan, "USE TEMP B-TREE FOR ORDER BY",
		"Sent envelope history paging should preserve message ID order:\n%s", plan)
}

func TestSenderKnownRelationshipKeysRequireArchiveHistory(t *testing.T) {
	assertions := assert.New(t)
	evidence := map[string]SenderRelationshipEvidence{
		"new@example.test":     {Relationship: "none"},
		"seen@example.test":    {Relationship: "none", SeenInArchive: true},
		"contact@example.test": {Relationship: "contact", SeenInArchive: true},
	}

	got := senderKnownRelationshipKeys([]string{"new@example.test", "seen@example.test", "contact@example.test"}, evidence)
	assertions.Equal([]string{"seen@example.test"}, got)
}

func FuzzNativeIMAPMailboxIDsAreNotRawImporterIDs(f *testing.F) {
	for _, mailbox := range []string{
		"sent", "INBOX", "eml-sent",
		"eml-" + strings.Repeat("a", 64),
		"mbox-" + strings.Repeat("a", 64),
		"pst-" + strings.Repeat("a", 12),
	} {
		f.Add(mailbox, uint32(1))
	}

	requirements := require.New(f)
	s, err := OpenForTest(filepath.Join(f.TempDir(), "native-imap-id-fuzz.db"))
	requirements.NoError(err)
	f.Cleanup(func() { requirements.NoError(s.Close()) })
	predicate := "SELECT " + s.senderRawImportedIDSQL("CAST(? AS TEXT)")

	f.Fuzz(func(t *testing.T, mailbox string, uid uint32) {
		if len(mailbox) > 256 {
			t.Skip()
		}
		requirements := require.New(t)
		assertions := assert.New(t)
		sourceMessageID := mailbox + "|" + strconv.FormatUint(uint64(uid), 10)
		args := make([]any, strings.Count(predicate, "?"))
		for i := range args {
			args[i] = sourceMessageID
		}
		var imported bool
		requirements.NoError(s.db.QueryRowContext(t.Context(), predicate, args...).Scan(&imported))
		assertions.False(imported, "native mailbox|UID identifiers are not raw importer IDs")
	})
}
