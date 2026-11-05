package imap

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type storedMembershipLoadCall struct {
	mailbox     string
	uidValidity uint32
	uids        []uint32
}

// archivedMemberships stands in for the store: it answers the loader from the
// observations an earlier listing recorded, which is what a completed sync
// writes to imap_message_memberships.
type archivedMemberships struct {
	rows  map[string]map[uint32]StoredMembership
	calls []storedMembershipLoadCall
	err   error
}

func archiveObservations(observations []MembershipObservation) *archivedMemberships {
	archive := &archivedMemberships{rows: make(map[string]map[uint32]StoredMembership)}
	for _, observation := range observations {
		if archive.rows[observation.Mailbox] == nil {
			archive.rows[observation.Mailbox] = make(map[uint32]StoredMembership)
		}
		flags := append([]string{}, observation.Flags...)
		archive.rows[observation.Mailbox][observation.UID] = StoredMembership{
			SourceMessageID: observation.SourceMessageID,
			Flags:           flags,
		}
	}
	return archive
}

func (a *archivedMemberships) load(
	mailbox string, uidValidity uint32, uids []uint32,
) (map[uint32]StoredMembership, error) {
	a.calls = append(a.calls, storedMembershipLoadCall{
		mailbox: mailbox, uidValidity: uidValidity, uids: append([]uint32(nil), uids...),
	})
	if a.err != nil {
		return nil, a.err
	}
	result := make(map[uint32]StoredMembership, len(uids))
	for _, uid := range uids {
		if row, ok := a.rows[mailbox][uid]; ok {
			result[uid] = row
		}
	}
	return result, nil
}

// syncOnce lists the server with no saved state, as a first sync does, and
// returns the saved folder states and the archive that sync would leave.
func syncOnce(t *testing.T, addr string) (map[string]FolderState, *archivedMemberships) {
	t.Helper()
	first := newTestClient(t, addr)
	listAllMessages(t, first)
	saved := first.ObservedFolderStates()
	archive := archiveObservations(first.ObservedMemberships())
	require.NoError(t, first.Close())
	return saved, archive
}

func TestListMessages_RefreshesFlagChangeWithoutModSeq(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})
	saved, archive := syncOnce(t, addr)
	require.Equal([]string{}, archive.rows["INBOX"][2].Flags)

	// The user replies to an archived message from their mail client. Neither
	// UIDNEXT nor the message count moves, so the mailbox plans as a skip.
	testutil.SetIMAPFlags(t, addr, "INBOX", 2, imapv2.FlagAnswered, imapv2.FlagSeen)

	second := newTestClient(t, addr,
		WithFolderStates(saved), WithStoredMembershipLoader(archive.load))
	assert.Empty(listAllMessages(t, second),
		"a flag change must not re-list or re-download the message")

	observations := second.ObservedMemberships()
	require.Len(observations, 1, "only the UID whose flags changed is published")
	assert.Equal("INBOX", observations[0].Mailbox)
	assert.Equal(uint32(2), observations[0].UID)
	assert.Equal(saved["INBOX"].UIDValidity, observations[0].UIDValidity)
	assert.Equal([]string{`\Answered`, `\Seen`}, observations[0].Flags)
	assert.Equal("INBOX|2", observations[0].CanonicalSourceMessageID,
		"the observation must resolve to the message the stored row points at")
	assert.Empty(observations[0].RFC822MessageID, "no header was fetched")

	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 2)
	for _, delta := range deltas {
		assert.False(delta.Reset, delta.Mailbox)
		assert.True(delta.Incremental, delta.Mailbox)
		assert.Equal(saved[delta.Mailbox].KnownUIDs, delta.State.KnownUIDs, delta.Mailbox)
	}
	assert.True(second.LabelsSnapshotComplete())
}

func TestListMessages_UnchangedFlagsRecordNoMemberships(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})
	testutil.SetIMAPFlags(t, addr, "INBOX", 1, imapv2.FlagSeen, imapv2.FlagFlagged)
	saved, archive := syncOnce(t, addr)
	require.Equal([]string{`\Flagged`, `\Seen`}, archive.rows["INBOX"][1].Flags)

	second := newTestClient(t, addr,
		WithFolderStates(saved), WithStoredMembershipLoader(archive.load))
	assert.Empty(listAllMessages(t, second))
	assert.Empty(second.ObservedMemberships(),
		"flags that match the archive must not produce a membership write")
	assert.Len(second.ObservedMailboxDeltas(), 2)

	// Each mailbox is checked once, over its whole baseline.
	assert.ElementsMatch([]storedMembershipLoadCall{
		{mailbox: "Archive", uidValidity: saved["Archive"].UIDValidity, uids: []uint32{1}},
		{mailbox: "INBOX", uidValidity: saved["INBOX"].UIDValidity, uids: []uint32{1, 2}},
	}, archive.calls)
}

func TestListMessages_RefreshesKnownFlagsBesideNewMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	saved, archive := syncOnce(t, addr)

	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.SetIMAPFlags(t, addr, "INBOX", 1, imapv2.FlagSeen)

	second := newTestClient(t, addr,
		WithFolderStates(saved), WithStoredMembershipLoader(archive.load))
	assert.Equal([]string{"INBOX|3"}, listAllMessages(t, second))

	// The new UID is read in full by the listing, so the refresh leaves it out
	// and asks only about the baseline.
	require.Len(archive.calls, 1)
	assert.Equal([]uint32{1, 2}, archive.calls[0].uids)

	refreshed := make(map[uint32][]string)
	for _, observation := range second.ObservedMemberships() {
		refreshed[observation.UID] = observation.Flags
	}
	assert.Equal([]string{`\Seen`}, refreshed[1])
	assert.NotContains(refreshed, uint32(2), "UID 2 did not change")
	assert.Contains(refreshed, uint32(3), "the label map still records the new UID")

	deltas := second.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.False(deltas[0].Reset)
	assert.Equal([]imapv2.UID{3}, deltas[0].ChangedUIDs)
}

func TestListMessages_FlagRefreshFailureKeepsSnapshotAuthoritative(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	saved, archive := syncOnce(t, addr)
	testutil.SetIMAPFlags(t, addr, "INBOX", 1, imapv2.FlagSeen)
	archive.err = errors.New("archive unavailable")

	var logs bytes.Buffer
	second := newTestClient(t, addr,
		WithFolderStates(saved),
		WithStoredMembershipLoader(archive.load),
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	assert.Empty(listAllMessages(t, second))

	assert.Empty(second.ObservedMemberships(), "without archived flags nothing can be compared")
	require.Len(second.ObservedMailboxDeltas(), 1,
		"a failed refresh must not withhold the proven topology")
	assert.True(second.LabelsSnapshotComplete())
	assert.Contains(logs.String(), "flag refresh skipped")
}

func TestListMessages_NoFlagRefreshWithoutLoader(t *testing.T) {
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	saved, _ := syncOnce(t, addr)
	testutil.SetIMAPFlags(t, addr, "INBOX", 1, imapv2.FlagSeen)

	second := newTestClient(t, addr, WithFolderStates(saved))
	assert.Empty(listAllMessages(t, second))
	assert.Empty(second.ObservedMemberships(),
		"with nothing to compare against, the refresh would rewrite every row")
}

// TestListMessagesModSeqServersSkipFlagRefresh pins that servers reporting
// mod-sequences keep their own paths: an unchanged HIGHESTMODSEQ proves no
// flag moved, and QRESYNC reads flag changes through CHANGEDSINCE.
func TestListMessagesModSeqServersSkipFlagRefresh(t *testing.T) {
	numMessages := uint32(2)
	cases := []struct {
		name         string
		capabilities string
		priorModSeq  uint64
		wantCommand  string
	}{
		{
			name:         "CONDSTORE without QRESYNC, unchanged",
			capabilities: "IMAP4rev1 CONDSTORE",
			priorModSeq:  20,
			wantCommand:  "STATUS INBOX",
		},
		{
			name:         "QRESYNC",
			capabilities: "IMAP4rev1 ENABLE QRESYNC CONDSTORE",
			priorModSeq:  10,
			wantCommand:  "(CHANGEDSINCE 10 VANISHED)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			addr, server := startQresyncTestServer(t, qresyncServerConfig{
				capabilities:  []string{tc.capabilities},
				uidValidity:   1,
				uidNext:       3,
				numMessages:   &numMessages,
				highestModSeq: 20,
				searchUIDs:    []imapv2.UID{1, 2},
			})
			archive := &archivedMemberships{rows: map[string]map[uint32]StoredMembership{
				"INBOX": {
					1: {SourceMessageID: "INBOX|1", Flags: []string{}},
					2: {SourceMessageID: "INBOX|2", Flags: []string{}},
				},
			}}
			client := newQresyncTestClient(t, addr, map[string]FolderState{
				"INBOX": {
					UIDValidity:   1,
					UIDNext:       3,
					HighestModSeq: tc.priorModSeq,
					KnownUIDs:     []uint32{1, 2},
				},
			}, WithStoredMembershipLoader(archive.load))

			listQresyncMessages(t, client)

			assert.Empty(archive.calls, "a mod-sequence server must not refresh flags")
			commands := joinedCommands(server.commandsFor(1))
			assert.Contains(commands, tc.wantCommand)
			assert.NotContains(commands, "(UID FLAGS)")
		})
	}
}

func TestFlagRefreshCandidatesKeepNewestWindow(t *testing.T) {
	known := make([]uint32, 0, flagRefreshWindow+10)
	for uid := uint32(1); uid <= flagRefreshWindow+10; uid++ {
		known = append(known, uid)
	}
	// An unsorted baseline must still give up its oldest UIDs, not its last.
	known[0], known[len(known)-1] = known[len(known)-1], known[0]
	listed := []imapv2.UID{flagRefreshWindow + 10}

	candidates := flagRefreshCandidates(known, listed)

	assert := assert.New(t)
	require.Len(t, candidates, flagRefreshWindow)
	assert.Equal(uint32(10), candidates[0], "the oldest UIDs fall outside the window")
	assert.Equal(uint32(flagRefreshWindow+9), candidates[len(candidates)-1],
		"a UID the listing reads in full is not refreshed")
	assert.Empty(flagRefreshCandidates(nil, nil))
}

func TestSameFlagsIgnoresOrder(t *testing.T) {
	cases := []struct {
		name    string
		saved   []string
		fetched []imapv2.Flag
		want    bool
	}{
		{"reordered", []string{`\Seen`, `\Answered`}, []imapv2.Flag{imapv2.FlagAnswered, imapv2.FlagSeen}, true},
		{"both empty", []string{}, nil, true},
		{"flag added", []string{`\Seen`}, []imapv2.Flag{imapv2.FlagSeen, imapv2.FlagAnswered}, false},
		{"flag cleared", []string{`\Seen`}, nil, false},
		{"keyword added", []string{`\Seen`}, []imapv2.Flag{imapv2.FlagSeen, "$Forwarded"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sameFlags(tc.saved, tc.fetched))
		})
	}
}
