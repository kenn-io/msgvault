package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type relocationFixture struct {
	st          *store.Store
	source      *store.Source
	addr        string
	server      *scriptedRFC7162Server
	id          int64
	draft, sent scriptedRFC7162Message
	final       scriptedRFC7162Snapshot
}

func newRelocationFixture(t *testing.T) relocationFixture {
	t.Helper()
	draft := newScriptedRFC7162Message(1, "overlap-relocation@example.test", imapapi.FlagDraft)
	draft.Body = "draftobsoleteword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://overlap-relocation@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(t, first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(t, err)
	require.Positive(t, id)
	// Legacy persisted-state fixture: this suite exercises forced
	// relocation of a lost Drafts canonical, which now coexists with
	// Sent-over-Drafts overlap refresh. Seed the pre-precedence archive
	// state — stale Drafts canonical and snapshot plus a saved Sent
	// membership and both cursors — through the real topology helper, so
	// every recovery case below still starts from a lost canonical holding
	// the old draft body. The overlap behavior itself is covered separately
	// by TestIMAPRelocationSentOutranksStaleDraftsCanonical.
	overlap := baseline.clone()
	overlap.Mailboxes[1].UIDNext = 2
	overlap.Mailboxes[1].HighestModSeq = 2
	overlap.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
	overlap.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	seedLegacyDraftsCanonicalWithSentMembership(
		t, st, source, []byte(scriptedRFC7162RawMessage(sent)), "<"+draft.MessageID+">")
	body, err := st.GetMessageBodyText(id)
	require.NoError(t, err)
	require.Contains(t, body, draft.Body,
		"the legacy fixture keeps the stale draft snapshot under the Drafts canonical")
	require.Len(t, queryScriptedRFC7162Memberships(t, st, source.ID), 2)
	final := overlap.clone()
	final.Mailboxes[0].Messages = nil
	final.Mailboxes[0].HighestModSeq = 3
	final.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
	final.Mailboxes[1].ChangedUIDs = nil
	return relocationFixture{st: st, source: source, addr: addr, server: server, id: id, draft: draft, sent: sent, final: final}
}

func (f relocationFixture) sync(t *testing.T, extra ...imap.Option) (*imap.Client, error) {
	t.Helper()
	opts := append(imapFolderStateOptions(f.st, f.source, false), extra...)
	client := newScriptedRFC7162Client(t, f.addr, opts...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(client, f.st, options).WithLogger(slog.New(slog.DiscardHandler)).FullWithFinalizer(
		t.Context(), f.source, func(summary *gmail.SyncSummary) error {
			return saveIMAPFolderStates(t.Context(), f.st, f.source, client, summary, options.Limit)
		})
	if err == nil && summary.Errors != 0 {
		err = fmt.Errorf("relocation sync completed with %d errors", summary.Errors)
	}
	return client, err
}

func (f relocationFixture) assertSent(t *testing.T, otherMemberships ...string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	body, err := f.st.GetMessageBodyText(f.id)
	require.NoError(err)
	assert.Contains(body, f.sent.Body)
	assert.NotContains(body, f.draft.Body)
	raw, err := f.st.GetMessageRaw(f.id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(f.sent), string(raw))
	message, err := f.st.GetMessage(f.id)
	require.NoError(err)
	assert.Equal("Sent|1", message.SourceMessageID)
	assert.ElementsMatch([]string{"Sent"}, message.Labels)
	assert.ElementsMatch(append([]string{"Sent|1|[\"\\\\Seen\"]"}, otherMemberships...), queryScriptedRFC7162Memberships(t, f.st, f.source.ID))
	results, total, err := f.st.SearchMessagesQuery(&search.Query{TextTerms: []string{f.sent.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(f.id, results[0].ID)
	_, total, err = f.st.SearchMessagesQuery(&search.Query{TextTerms: []string{f.draft.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total)
}

func TestIMAPRelocationAfterOverlapVANISHED(t *testing.T) {
	for _, mode := range []string{"VANISHED", "disappeared mailbox", "new epoch", "fallback", "changed alias"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRelocationFixture(t)
			switch mode {
			case "disappeared mailbox":
				f.final.Mailboxes = f.final.Mailboxes[1:]
			case "new epoch":
				f.final.Mailboxes[0].UIDValidity = 99
			case "fallback":
				f.final.Capabilities = "IMAP4rev1 SPECIAL-USE"
			case "changed alias":
				f.final.Mailboxes[1].HighestModSeq = 3
				f.final.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
			}
			f.server.setSnapshot(f.final)
			third, err := f.sync(t)
			require.NoError(err)
			require.NoError(third.Close())
			f.assertSent(t)
			if mode == "VANISHED" {
				assert.NotRegexp(`UID SEARCH UID \d+:\*`, f.server.commandsFor(2))
				assert.Contains(f.server.commandsFor(2), "BODY.PEEK",
					"the forced loader fetches the unchanged Sent survivor")
			}
			states, err := f.st.GetIMAPFolderStates(f.source.ID)
			require.NoError(err)
			require.Len(states, len(f.final.Mailboxes))
			for i, state := range states {
				assert.Equal(f.final.Mailboxes[i].HighestModSeq, state.HighestModSeq)
				assert.Equal(f.final.Mailboxes[i].UIDValidity, state.UIDValidity)
			}
			if mode == "VANISHED" {
				f.final.Mailboxes[0].VanishedUIDs = nil
				f.server.setSnapshot(f.final)
				fourth, err := f.sync(t, imap.WithRelocationCandidateLoader(func(context.Context, []string) ([]imap.RelocationCandidate, error) {
					return nil, errors.New("unchanged sync must not load candidates")
				}))
				require.NoError(err)
				require.NoError(fourth.Close())
				assert.NotContains(f.server.commandsFor(3), "BODY.PEEK[]")
			}
		})
	}
}

func TestIMAPRelocationFailureKeepsSnapshotAndCursorsRetryable(t *testing.T) {
	for _, failure := range []string{"candidate query", "raw identity", "raw body", "source collision", "target guard"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRelocationFixture(t)
			beforeStates, err := f.st.GetIMAPFolderStates(f.source.ID)
			require.NoError(err)
			beforeMemberships := queryScriptedRFC7162Memberships(t, f.st, f.source.ID)
			beforeRaw, err := f.st.GetMessageRaw(f.id)
			require.NoError(err)
			failed := f.final.clone()
			var extra []imap.Option
			var collisionID int64
			switch failure {
			case "candidate query":
				extra = append(extra, imap.WithRelocationCandidateLoader(func(context.Context, []string) ([]imap.RelocationCandidate, error) {
					return nil, errors.New("candidate query failure")
				}))
			case "raw identity":
				failed.Mailboxes[1].Messages[0].MessageID = "different@example.test"
			case "raw body":
				failed.Mailboxes[1].Messages[0].MissingRaw = true
			case "source collision":
				conv, err := f.st.EnsureConversation(f.source.ID, "collision-thread", "Collision")
				require.NoError(err)
				collisionID, err = f.st.UpsertMessage(&store.Message{SourceID: f.source.ID, ConversationID: conv, SourceMessageID: "Sent|1", MessageType: "email"})
				require.NoError(err)
			case "target guard":
				extra = append(extra, imap.WithRelocationCandidateLoader(func(ctx context.Context, lost []string) ([]imap.RelocationCandidate, error) {
					candidates, err := f.st.GetIMAPRelocationCandidatesContext(ctx, f.source.ID, lost)
					if err != nil {
						return nil, err
					}
					var result []imap.RelocationCandidate
					for _, c := range candidates {
						result = append(result, imap.RelocationCandidate{Target: gmail.MessageRelocationTarget{
							InternalID: c.ID + 1000, SourceID: c.SourceID, SourceMessageID: c.SourceMessageID, RFC822MessageID: c.RFC822MessageID,
						}, Mailbox: c.Mailbox, UIDValidity: c.UIDValidity, UID: c.UID})
					}
					return result, nil
				}))
			}
			f.server.setSnapshot(failed)
			client, err := f.sync(t, extra...)
			require.Error(err)
			require.NoError(client.Close())
			afterStates, err := f.st.GetIMAPFolderStates(f.source.ID)
			require.NoError(err)
			assert.Equal(beforeStates, afterStates)
			assert.Equal(beforeMemberships, queryScriptedRFC7162Memberships(t, f.st, f.source.ID))
			raw, err := f.st.GetMessageRaw(f.id)
			require.NoError(err)
			assert.Equal(beforeRaw, raw)
			message, err := f.st.GetMessage(f.id)
			require.NoError(err)
			assert.Equal("Drafts|1", message.SourceMessageID)
			assert.Contains(message.Labels, "Drafts")
			if collisionID != 0 {
				_, err := f.st.DB().Exec(f.st.Rebind("DELETE FROM messages WHERE id = ?"), collisionID)
				require.NoError(err)
			}
			f.server.setSnapshot(f.final)
			retry, err := f.sync(t)
			require.NoError(err)
			require.NoError(retry.Close())
			f.assertSent(t)
		})
	}
}

// A reset can reuse the lost canonical composite ID in the same listing as
// its forced replacement. Ordinary routing must not invalidate the retained
// target before relocation, including when the relocation needs another run.
func TestIMAPRelocationBeforeReusedUIDRouting(t *testing.T) {
	for _, failure := range []string{"none", "raw body", "raw identity", "source collision"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newRelocationFixture(t)
			unrelated := newScriptedRFC7162Message(1, "unrelated-new-epoch@example.test", imapapi.FlagDraft)
			unrelated.Body = "unrelatedreplacementword"
			f.final.Mailboxes[0].UIDValidity = 99
			f.final.Mailboxes[0].HighestModSeq = 1
			f.final.Mailboxes[0].VanishedUIDs = nil
			f.final.Mailboxes[0].Messages = []scriptedRFC7162Message{unrelated}
			beforeStates, err := f.st.GetIMAPFolderStates(f.source.ID)
			require.NoError(err)
			beforeMemberships := queryScriptedRFC7162Memberships(t, f.st, f.source.ID)
			before, err := f.st.GetMessage(f.id)
			require.NoError(err)
			beforeRaw, err := f.st.GetMessageRaw(f.id)
			require.NoError(err)
			failed := f.final.clone()
			var collisionID int64
			switch failure {
			case "raw body":
				failed.Mailboxes[1].Messages[0].MissingRaw = true
			case "raw identity":
				failed.Mailboxes[1].Messages[0].MessageID = "different-survivor@example.test"
			case "source collision":
				conv, err := f.st.EnsureConversation(f.source.ID, "collision-thread", "Collision")
				require.NoError(err)
				collisionID, err = f.st.UpsertMessage(&store.Message{SourceID: f.source.ID, ConversationID: conv, SourceMessageID: "Sent|1", MessageType: "email"})
				require.NoError(err)
			}
			f.server.setSnapshot(failed)
			client, syncErr := f.sync(t)
			require.NoError(client.Close())
			if failure == "none" {
				require.NoError(syncErr)
			} else {
				require.Error(syncErr)
				afterStates, err := f.st.GetIMAPFolderStates(f.source.ID)
				require.NoError(err)
				assert.Equal(beforeStates, afterStates)
				assert.Equal(beforeMemberships, queryScriptedRFC7162Memberships(t, f.st, f.source.ID))
				after, err := f.st.GetMessage(f.id)
				require.NoError(err)
				assert.Equal(before.SourceMessageID, after.SourceMessageID, "failed relocation must retain the key used to discover its retry")
				assert.Equal(before.BodyText, after.BodyText)
				assert.Equal(before.Labels, after.Labels)
				raw, err := f.st.GetMessageRaw(f.id)
				require.NoError(err)
				assert.Equal(beforeRaw, raw)
				results, total, err := f.st.SearchMessagesQuery(&search.Query{TextTerms: []string{f.draft.Body}}, 0, 10)
				require.NoError(err)
				assert.Equal(int64(1), total)
				require.Len(results, 1)
				assert.Equal(f.id, results[0].ID)
				unrelatedID, err := f.st.GetMessageIDByRFC822ID(f.source.ID, "<"+unrelated.MessageID+">")
				require.NoError(err)
				assert.Zero(unrelatedID, "ordinary routing waits for forced relocation to succeed")
				if collisionID != 0 {
					_, err := f.st.DB().Exec(f.st.Rebind("DELETE FROM messages WHERE id = ?"), collisionID)
					require.NoError(err)
				}
				f.server.setSnapshot(f.final)
				retry, err := f.sync(t)
				require.NoError(err)
				require.NoError(retry.Close())
			}
			f.assertSent(t, "Drafts|1|[\"\\\\Draft\"]")
			unrelatedID, err := f.st.GetMessageIDByRFC822ID(f.source.ID, "<"+unrelated.MessageID+">")
			require.NoError(err)
			require.Positive(unrelatedID)
			assert.NotEqual(f.id, unrelatedID)
			message, err := f.st.GetMessage(unrelatedID)
			require.NoError(err)
			assert.Equal("Drafts|1", message.SourceMessageID)
			assert.Contains(message.BodyText, unrelated.Body)
			assert.ElementsMatch([]string{"Drafts"}, message.Labels)
			raw, err := f.st.GetMessageRaw(unrelatedID)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(unrelated), string(raw))
			results, total, err := f.st.SearchMessagesQuery(&search.Query{TextTerms: []string{unrelated.Body}}, 0, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			require.Len(results, 1)
			assert.Equal(unrelatedID, results[0].ID)
			var count int
			require.NoError(f.st.DB().QueryRow(f.st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), f.source.ID).Scan(&count))
			assert.Equal(2, count, "no invalidated stale archive row is stranded")
			states, err := f.st.GetIMAPFolderStates(f.source.ID)
			require.NoError(err)
			require.Len(states, 2)
			for i, state := range states {
				assert.Equal(f.final.Mailboxes[i].UIDValidity, state.UIDValidity)
				assert.Equal(f.final.Mailboxes[i].HighestModSeq, state.HighestModSeq)
			}
			known, err := f.st.GetIMAPKnownUIDs(f.source.ID)
			require.NoError(err)
			assert.Equal(map[string][]uint32{"Drafts": {1}, "Sent": {1}}, known)
			noop, err := f.sync(t, imap.WithRelocationCandidateLoader(func(context.Context, []string) ([]imap.RelocationCandidate, error) {
				return nil, errors.New("unchanged sync must not load candidates")
			}))
			require.NoError(err)
			require.NoError(noop.Close())
			connection := 4
			if failure != "none" {
				connection++
			}
			assert.NotContains(f.server.commandsFor(connection), "BODY.PEEK[]")
		})
	}
}

// A UIDVALIDITY change that reuses the same UID keeps the composite source ID.
// The surviving copy at that unchanged key must refresh the archived snapshot
// through identity routing without a same-key relocation, and a reused UID
// holding a different message must never replace the archived original.
func TestIMAPRelocationUIDValidityChangeSameUIDSurvivor(t *testing.T) {
	for _, mode := range []string{"edited survivor", "reused by different message"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			draft := newScriptedRFC7162Message(1, "uidvalidity-survivor@example.test", imapapi.FlagDraft)
			draft.Body = "draftoriginalword"
			survivor := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			survivor.Body = "editedfinalword"
			unrelated := newScriptedRFC7162Message(1, "uidvalidity-unrelated@example.test", imapapi.FlagSeen)
			unrelated.Body = "unrelatedreplacementword"
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://uidvalidity-survivor@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)

			reset := baseline.clone()
			reset.Mailboxes[0].UIDValidity = 99
			reset.Mailboxes[0].HighestModSeq = 2
			reset.Mailboxes[0].ChangedUIDs = []imapapi.UID{1}
			if mode == "edited survivor" {
				reset.Mailboxes[0].Messages = []scriptedRFC7162Message{survivor}
			} else {
				reset.Mailboxes[0].Messages = []scriptedRFC7162Message{unrelated}
			}
			server.setSnapshot(reset)
			second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err, "unchanged composite key after an epoch change must not strand the sync")
			require.NoError(second.Close())

			states, err := st.GetIMAPFolderStates(source.ID)
			require.NoError(err)
			require.Len(states, 1)
			assert.Equal(uint32(99), states[0].UIDValidity)
			known, err := st.GetIMAPKnownUIDs(source.ID)
			require.NoError(err)
			assert.Equal(map[string][]uint32{"Drafts": {1}}, known)

			if mode == "edited survivor" {
				message, err := st.GetMessage(id)
				require.NoError(err)
				assert.Equal("Drafts|1", message.SourceMessageID)
				assert.Contains(message.BodyText, survivor.Body)
				assert.NotContains(message.BodyText, draft.Body)
				assert.ElementsMatch([]string{"Drafts"}, message.Labels)
				assert.ElementsMatch([]string{"Drafts|1|[\"\\\\Seen\"]"}, queryScriptedRFC7162Memberships(t, st, source.ID))
				var count int
				require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
				assert.Equal(1, count)
			} else {
				message, err := st.GetMessage(id)
				require.NoError(err)
				assert.Contains(message.BodyText, draft.Body, "a reused UID holding a different message must not replace the archived original")
				raw, err := st.GetMessageRaw(id)
				require.NoError(err)
				assert.Equal(scriptedRFC7162RawMessage(draft), string(raw))
				var deleted int
				require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ? AND deleted_from_source_at IS NOT NULL"), source.ID).Scan(&deleted))
				assert.Equal(1, deleted, "the superseded original is retired, not rewritten")
				unrelatedID, err := st.GetMessageIDByRFC822ID(source.ID, "<"+unrelated.MessageID+">")
				require.NoError(err)
				require.Positive(unrelatedID)
				unrelatedMessage, err := st.GetMessage(unrelatedID)
				require.NoError(err)
				assert.Equal("Drafts|1", unrelatedMessage.SourceMessageID)
				assert.Contains(unrelatedMessage.BodyText, unrelated.Body)
				assert.ElementsMatch([]string{"Drafts"}, unrelatedMessage.Labels)
			}

			server.setSnapshot(reset)
			third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(third.Close())
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
			if mode == "edited survivor" {
				assert.Equal(1, count)
				message, err := st.GetMessage(id)
				require.NoError(err)
				assert.Contains(message.BodyText, survivor.Body)
			} else {
				assert.Equal(2, count)
			}
		})
	}
}

// A saved membership that already carries the current epoch while the saved
// folder state is older makes the relocation listing offer the unchanged
// composite key itself. The listing must skip that candidate: persistence
// requires the key to change, and ordinary identity routing already refreshes
// the snapshot, so forcing it would fail every retry without progress.
func TestIMAPRelocationSkipsSameCompositeForcedCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "same-composite-candidate@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	survivor := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	survivor.Body = "editedfinalword"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://same-composite-candidate@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO imap_message_memberships (source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
		SELECT source_id, mailbox, 99, uid, message_id, flags, updated_at
		FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = 'Drafts' AND uidvalidity = 77 AND uid = 1
	`), source.ID)
	require.NoError(err)

	reset := baseline.clone()
	reset.Mailboxes[0].UIDValidity = 99
	reset.Mailboxes[0].HighestModSeq = 2
	reset.Mailboxes[0].ChangedUIDs = []imapapi.UID{1}
	reset.Mailboxes[0].Messages = []scriptedRFC7162Message{survivor}
	server.setSnapshot(reset)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err, "a same-composite forced candidate must fall back to identity routing")
	require.NoError(second.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Drafts|1", message.SourceMessageID)
	assert.Contains(message.BodyText, survivor.Body)
	assert.NotContains(message.BodyText, draft.Body)
	assert.ElementsMatch([]string{"Drafts"}, message.Labels)
	assert.ElementsMatch([]string{"Drafts|1|[\"\\\\Seen\"]"}, queryScriptedRFC7162Memberships(t, st, source.ID))
	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	require.Len(states, 1)
	assert.Equal(uint32(99), states[0].UIDValidity)
	known, err := st.GetIMAPKnownUIDs(source.ID)
	require.NoError(err)
	assert.Equal(map[string][]uint32{"Drafts": {1}}, known)

	third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(third.Close())
	assert.NotContains(server.commandsFor(3), "BODY.PEEK[]")
}

// A forged RFC822 Message-ID on an incoming message must not replace an
// archived snapshot. Content refresh is authorized only by provider-controlled
// placement evidence (server-advertised \Sent or \Drafts special use), which
// external senders cannot produce; every other adoption keeps the canonical
// snapshot and only rekeys the location, matching pre-relocation behavior.
func TestIMAPRelocationForgedSurvivorPreservesSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.Sync.ArchiveRemoteImages = true
	victim := newScriptedRFC7162Message(1, "forged-survivor@example.test", imapapi.FlagSeen)
	victim.Body = "victimoriginalword"
	victim.Raw = scriptedOutgoingRaw(
		victim.MessageID, "Original subject",
		"victim-to@example.test", "victim-cc@example.test",
		"victimoriginalword", "<p>victimoriginalword</p>",
		"victim.bin", "victim attachment bytes")
	forger := newScriptedRFC7162Message(2, victim.MessageID, imapapi.FlagSeen)
	forger.Body = "forgedreplacementword"
	forger.Raw = scriptedOutgoingRaw(
		victim.MessageID, "Forged replacement",
		"forger-to@example.test", "forger-cc@example.test",
		"forgedreplacementword",
		"<p>forgedreplacementword <img src=\"http://images.example/forged-chart\"></p>",
		"forger.bin", "forged attachment bytes")
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(55, 2, 1, victim),
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://forged-survivor@example.test"
	attachDir := t.TempDir()
	run := func(snapshot scriptedRFC7162Snapshot) error {
		server.setSnapshot(snapshot)
		client, _, err := runScriptedRelocationSync(t, st, identifier, addr, func(o *msgsync.Options) {
			o.AttachmentsDir = attachDir
		})
		if closeErr := client.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	first, source, err := runScriptedRelocationSync(t, st, identifier, addr, func(o *msgsync.Options) {
		o.AttachmentsDir = attachDir
	})
	require.NoError(err)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+victim.MessageID+">")
	require.NoError(err)
	require.Positive(id)
	before, err := st.GetMessage(id)
	require.NoError(err)
	require.Len(before.Attachments, 1)
	assert.Equal("victim.bin", before.Attachments[0].Filename)

	replaced := baseline.clone()
	replaced.Mailboxes[0].HighestModSeq = 2
	replaced.Mailboxes[0].UIDNext = 3
	replaced.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
	replaced.Mailboxes[0].ChangedUIDs = []imapapi.UID{2}
	replaced.Mailboxes[0].Messages = []scriptedRFC7162Message{forger}
	require.NoError(run(replaced))

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("INBOX|2", message.SourceMessageID, "the forged survivor still rekeys the location")
	assert.Contains(message.BodyText, victim.Body, "the archived body must survive a forged Message-ID")
	assert.NotContains(message.BodyText, forger.Body)
	assert.NotEqual("Forged replacement", message.Subject)
	assert.Equal([]string{"victim-to@example.test"}, message.To,
		"forged recipients must not overwrite the archived recipient")
	assert.Equal([]string{"victim-cc@example.test"}, message.Cc)
	assert.Equal(before.FromEmail, message.FromEmail)
	require.Len(message.Attachments, 1)
	assert.Equal("victim.bin", message.Attachments[0].Filename,
		"the forged attachment must not replace or join the archived one")
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(victim.Raw, string(raw))
	assert.ElementsMatch([]string{"INBOX"}, message.Labels)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count)
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{victim.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{forger.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total, "forged content must not reach the search index")
	refs, err := st.MessageRemoteImages(id)
	require.NoError(err)
	assert.Empty(refs, "rejected forged bytes must not trigger remote-image processing")

	require.NoError(run(replaced))
	message, err = st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, victim.Body)
	assert.Equal("INBOX|2", message.SourceMessageID)
	refs, err = st.MessageRemoteImages(id)
	require.NoError(err)
	assert.Empty(refs)
}

// A forged copy mirrored into the server's \All mailbox must not replace the
// archived snapshot even though the original still exists and the copy is at
// the preferred canonical location: \All placement holds received mail too,
// so it is not provider-authored evidence.
func TestIMAPPreferredAllMailForgedCopyPreservesSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	victim := newScriptedRFC7162Message(1, "forged-all-mail@example.test", imapapi.FlagSeen)
	victim.Body = "victimoriginalword"
	forger := newScriptedRFC7162Message(1, victim.MessageID, imapapi.FlagSeen)
	forger.Body = "forgedreplacementword"
	forger.Subject = "Forged replacement"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(66, 2, 1, victim),
		{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 77, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://forged-all-mail@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+victim.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	mirrored := baseline.clone()
	mirrored.Mailboxes[0].HighestModSeq = 2
	mirrored.Mailboxes[0].UIDNext = 3
	mirrored.Mailboxes[0].ChangedUIDs = []imapapi.UID{2}
	mirrored.Mailboxes[0].Messages = []scriptedRFC7162Message{victim, forgerWithUID(forger, 2)}
	mirrored.Mailboxes[1].HighestModSeq = 2
	mirrored.Mailboxes[1].UIDNext = 2
	mirrored.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	mirrored.Mailboxes[1].Messages = []scriptedRFC7162Message{forger}
	server.setSnapshot(mirrored)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("All Mail|1", message.SourceMessageID, "preferred adoption still rekeys the canonical location")
	assert.Contains(message.BodyText, victim.Body, "the archived body must survive a forged preferred copy")
	assert.NotContains(message.BodyText, forger.Body)
	assert.NotEqual(forger.Subject, message.Subject)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(victim), string(raw))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count)
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{victim.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{forger.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total, "forged content must not reach the search index")

	third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(third.Close())
	message, err = st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, victim.Body)
	assert.Equal("All Mail|1", message.SourceMessageID)
}

// A membership previously bound to the archived row through a forged
// Message-ID must not let forced recovery replace the snapshot when the
// original location vanishes: the surviving location holds received mail, not
// provider-authored placement.
func TestIMAPRelocationForgedMembershipPreservesSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	victim := newScriptedRFC7162Message(1, "forged-membership@example.test", imapapi.FlagSeen)
	victim.Body = "victimoriginalword"
	forger := newScriptedRFC7162Message(2, victim.MessageID, imapapi.FlagSeen)
	forger.Body = "forgedreplacementword"
	forger.Subject = "Forged replacement"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(55, 2, 1, victim),
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://forged-membership@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+victim.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	overlap := baseline.clone()
	overlap.Mailboxes[0].HighestModSeq = 2
	overlap.Mailboxes[0].UIDNext = 3
	overlap.Mailboxes[0].ChangedUIDs = []imapapi.UID{2}
	overlap.Mailboxes[0].Messages = []scriptedRFC7162Message{victim, forger}
	server.setSnapshot(overlap)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err, "the overlapping forged copy must not disturb the archive")
	require.NoError(second.Close())
	memberships := queryScriptedRFC7162Memberships(t, st, source.ID)
	assert.ElementsMatch([]string{"INBOX|1|[\"\\\\Seen\"]", "INBOX|2|[\"\\\\Seen\"]"}, memberships,
		"the overlap sync binds the forged copy's membership to the archived row")

	vanished := overlap.clone()
	vanished.Mailboxes[0].HighestModSeq = 3
	vanished.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
	vanished.Mailboxes[0].ChangedUIDs = nil
	vanished.Mailboxes[0].Messages = []scriptedRFC7162Message{forger}
	server.setSnapshot(vanished)
	third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(third.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("INBOX|2", message.SourceMessageID, "forced recovery still rekeys to the surviving location")
	assert.Contains(message.BodyText, victim.Body, "forced recovery must not adopt forged content")
	assert.NotContains(message.BodyText, forger.Body)
	assert.NotEqual(forger.Subject, message.Subject)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(victim), string(raw))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count)
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{victim.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{forger.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total, "forged content must not reach the search index")
}

// Without advertised \Sent or \Drafts special use there is no defensible
// provider evidence for an edited survivor, so even a genuine draft-to-sent
// edit keeps the archived snapshot and only rekeys the location. Mailbox
// names alone are never trusted.
func TestIMAPRelocationWithoutSpecialUsePreservesEditedSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "no-special-use@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "editedfinalword"
	sent.Subject = "Edited final"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "Drafts", UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", UIDValidity: 88, UIDNext: 2, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://no-special-use@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	edited := baseline.clone()
	edited.Mailboxes[0].HighestModSeq = 2
	edited.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
	edited.Mailboxes[0].Messages = nil
	edited.Mailboxes[1].HighestModSeq = 2
	edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(edited)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Sent|1", message.SourceMessageID, "the surviving location is still adopted")
	assert.Contains(message.BodyText, draft.Body, "unadvertised placement keeps the archived snapshot")
	assert.NotContains(message.BodyText, sent.Body)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(draft), string(raw))
	assert.ElementsMatch([]string{"Sent"}, message.Labels)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count)
}

func forgerWithUID(message scriptedRFC7162Message, uid imapapi.UID) scriptedRFC7162Message {
	message.UID = uid
	return message
}

// runScriptedRelocationSync mirrors runScriptedRFC7162Sync with an options
// modifier, so relocation tests can exercise production wiring with an
// attachments directory or remote-image fetcher configured.
func runScriptedRelocationSync(
	t *testing.T, st *store.Store, identifier, addr string,
	mod func(*msgsync.Options),
) (*imap.Client, *store.Source, error) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(t, err)
	client := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	if mod != nil {
		mod(options)
	}
	summary, err := newMessageSyncer(client, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	if err != nil {
		return client, source, err
	}
	if summary.Errors != 0 {
		return client, source, fmt.Errorf(
			"scripted IMAP sync completed with %d errors", summary.Errors)
	}
	return client, source, saveIMAPFolderStates(
		context.Background(), st, source, client, summary, options.Limit)
}

func scriptedOutgoingRaw(
	messageID, subject, to, cc, textBody, htmlBody, attachmentName, attachmentBytes string,
) string {
	headers := fmt.Sprintf(
		"From: account-owner@example.test\r\nTo: %s\r\nCc: %s\r\nSubject: %s\r\n"+
			"Date: Mon, 1 Jan 2024 00:00:00 +0000\r\nMessage-ID: <%s>\r\n"+
			"MIME-Version: 1.0\r\n", to, cc, subject, messageID)
	alternative := fmt.Sprintf(
		"--outmix\r\nContent-Type: multipart/alternative; boundary=outalt\r\n\r\n"+
			"--outalt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n"+
			"--outalt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n"+
			"--outalt--\r\n", textBody, htmlBody)
	if attachmentName == "" {
		return headers + "Content-Type: multipart/alternative; boundary=outalt\r\n\r\n" +
			fmt.Sprintf("--outalt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n"+
				"--outalt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n--outalt--\r\n",
				textBody, htmlBody)
	}
	return headers + "Content-Type: multipart/mixed; boundary=outmix\r\n\r\n" +
		alternative +
		fmt.Sprintf("--outmix\r\nContent-Type: application/octet-stream; name=%q\r\n"+
			"Content-Disposition: attachment; filename=%q\r\n\r\n%s\r\n--outmix--\r\n",
			attachmentName, attachmentName, attachmentBytes)
}

// The reported topology: advertised \Drafts, \Sent, and \All all exist. An
// untrusted \All mirror must not strand the genuine edited Sent copy — the
// trusted outgoing placement refreshes the snapshot even after the mirror took
// the canonical key, both when the draft disappears directly and when an
// overlap sync adopts the mirror first.
func TestIMAPRelocationDraftsSentAllMailTopology(t *testing.T) {
	for _, mode := range []string{"direct disappearance", "overlap then disappearance"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			draft := newScriptedRFC7162Message(1, "all-mail-topology@example.test", imapapi.FlagDraft)
			draft.Body = "draftoriginalword"
			sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			sent.Body = "sentfinalword"
			sent.Subject = "Sent final"
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				// All Mail listed first so its mirror is ingested before Sent.
				{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 1, HighestModSeq: 1},
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
				{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://all-mail-topology@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)

			edited := baseline.clone()
			edited.Mailboxes[0].HighestModSeq = 2
			edited.Mailboxes[0].UIDNext = 2
			edited.Mailboxes[0].ChangedUIDs = []imapapi.UID{1}
			edited.Mailboxes[0].Messages = []scriptedRFC7162Message{sent}
			edited.Mailboxes[1].HighestModSeq = 2
			edited.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
			edited.Mailboxes[1].Messages = nil
			edited.Mailboxes[2].HighestModSeq = 2
			edited.Mailboxes[2].UIDNext = 2
			edited.Mailboxes[2].ChangedUIDs = []imapapi.UID{1}
			edited.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
			if mode == "overlap then disappearance" {
				edited.Mailboxes[1].VanishedUIDs = nil
				edited.Mailboxes[1].Messages = []scriptedRFC7162Message{draft}
			}
			server.setSnapshot(edited)
			second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(second.Close())

			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Contains(message.BodyText, sent.Body,
				"the trusted Sent copy must refresh the snapshot despite the All Mail mirror")
			assert.NotContains(message.BodyText, draft.Body)
			assert.Equal("Sent final", message.Subject)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
			assert.Equal(1, count)
			results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{sent.Body}}, 0, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			require.Len(results, 1)
			assert.Equal(id, results[0].ID)
			_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{draft.Body}}, 0, 10)
			require.NoError(err)
			assert.Zero(total)

			final := edited.clone()
			if mode == "overlap then disappearance" {
				final.Mailboxes[1].HighestModSeq = 3
				final.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
				final.Mailboxes[1].Messages = nil
			}
			server.setSnapshot(final)
			third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(third.Close())
			message, err = st.GetMessage(id)
			require.NoError(err)
			assert.Contains(message.BodyText, sent.Body)
			assert.NotContains(message.BodyText, draft.Body)
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
			assert.Equal(1, count)
			assert.ElementsMatch([]string{"Sent", "All Mail"}, message.Labels)

			server.setSnapshot(final)
			forth, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(forth.Close())
			assert.NotContains(server.commandsFor(4), "BODY.PEEK[]")
		})
	}
}

// Servers without RFC 6154 role discovery — the reported provider uses a
// localized Sent folder — stay deny-by-default, but the explicit
// trusted_imap_sent_mailboxes configuration restores the edited-copy
// refresh through the same production wiring.
func TestIMAPRelocationConfiguredTrustedOutgoingMailbox(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	const identifier = "imap://configured-trust@example.test"
	cfg = config.NewDefaultConfig()
	cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
		identifier: {"Gesendete Elemente"},
	}

	draft := newScriptedRFC7162Message(1, "configured-trust@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	sent.Subject = "Sent final"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "Entw&APw-rfe", UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Gesendete Elemente", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	edited := baseline.clone()
	edited.Mailboxes[0].HighestModSeq = 2
	edited.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
	edited.Mailboxes[0].Messages = nil
	edited.Mailboxes[1].HighestModSeq = 2
	edited.Mailboxes[1].UIDNext = 2
	edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(edited)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())
	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Gesendete Elemente|1", message.SourceMessageID)
	assert.Contains(message.BodyText, sent.Body, "the configured trust restores the edited-copy refresh")
	assert.NotContains(message.BodyText, draft.Body)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
	assert.ElementsMatch([]string{"Gesendete Elemente"}, message.Labels)
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{sent.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
}

// A mailbox whose advertised role is ambiguous — Sent combined with All, Junk,
// Trash, or INBOX — carries no outgoing-placement assumption, so an edited
// survivor there keeps the archived snapshot.
func TestIMAPRelocationAmbiguousRoleDeniesRefresh(t *testing.T) {
	for _, extra := range []string{"All", "Junk", "Trash", "InboxName"} {
		t.Run(extra, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			draft := newScriptedRFC7162Message(1, "ambiguous-role@example.test", imapapi.FlagDraft)
			draft.Body = "draftoriginalword"
			sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			sent.Body = "sentfinalword"
			survivorAttrs := []imapapi.MailboxAttr{imapapi.MailboxAttrSent, imapapi.MailboxAttrAll}
			survivorName := "Sent"
			switch extra {
			case "Junk":
				survivorAttrs = []imapapi.MailboxAttr{imapapi.MailboxAttrSent, imapapi.MailboxAttrJunk}
			case "Trash":
				survivorAttrs = []imapapi.MailboxAttr{imapapi.MailboxAttrSent, imapapi.MailboxAttrTrash}
			case "InboxName":
				survivorName = "INBOX"
			}
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
				{Name: survivorName, Attrs: survivorAttrs, UIDValidity: 88, UIDNext: 2, HighestModSeq: 1},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://ambiguous-role@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)

			edited := baseline.clone()
			edited.Mailboxes[0].HighestModSeq = 2
			edited.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
			edited.Mailboxes[0].Messages = nil
			edited.Mailboxes[1].HighestModSeq = 2
			edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
			edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
			server.setSnapshot(edited)
			second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(second.Close())

			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Equal(survivorName+"|1", message.SourceMessageID, "the location is still adopted")
			assert.Contains(message.BodyText, draft.Body, "an ambiguous advertised role denies the refresh")
			assert.NotContains(message.BodyText, sent.Body)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(draft), string(raw))
		})
	}
}

// runScriptedRelocationResync mirrors runScriptedRelocationSync with a forced
// full rescan, matching the CLI's --noresume production wiring.
func runScriptedRelocationResync(
	t *testing.T, st *store.Store, identifier, addr string,
) (*imap.Client, error) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(t, err)
	client := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source, true)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(client, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	if err != nil {
		return client, err
	}
	if summary.Errors != 0 {
		return client, fmt.Errorf(
			"scripted IMAP resync completed with %d errors", summary.Errors)
	}
	return client, saveIMAPFolderStates(
		context.Background(), st, source, client, summary, options.Limit)
}

// Force-full rescans and realistic mailbox rebuilds — including an All Mail
// epoch rebuild, a stale-Draft resurrection, a flag change, and repeated
// rescans — must keep the archived edited-Sent snapshot: alias and dedup
// suppression route unchanged copies through label refresh, and no adoption
// path may overwrite the sent body, raw MIME, or search content with the
// stale draft. Canonical placement follows the normal adoption rules and is
// not pinned here. Every stage runs through completed production syncs.
func TestIMAPRelocationRescanOrderingKeepsEditedSent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "rescan-ordering@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	sent.Subject = "Sent final"
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 1, HighestModSeq: 1},
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://rescan-ordering@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	requireEditedSent := func(stage string) {
		message, err := st.GetMessage(id)
		require.NoError(err)
		assert.Contains(message.BodyText, sent.Body, stage)
		assert.NotContains(message.BodyText, draft.Body, stage)
	}

	// Overlap: the edited Sent copy and its All mirror appear while the
	// draft remains. The production sync establishes the edited-Sent
	// snapshot (mirror preferred adoption first, then the trusted Sent
	// refresh) that every rescan stage below must keep.
	overlap := baseline.clone()
	overlap.Mailboxes[0].HighestModSeq = 2
	overlap.Mailboxes[0].UIDNext = 2
	overlap.Mailboxes[0].ChangedUIDs = []imapapi.UID{1}
	overlap.Mailboxes[0].Messages = []scriptedRFC7162Message{sent}
	overlap.Mailboxes[2].HighestModSeq = 2
	overlap.Mailboxes[2].UIDNext = 2
	overlap.Mailboxes[2].ChangedUIDs = []imapapi.UID{1}
	overlap.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(overlap)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())
	requireEditedSent("overlap establishes the edited Sent snapshot")

	// Force-full rescan with every copy present: durable memberships route
	// each copy through alias label refresh, so nothing is re-ingested.
	rescan, err := runScriptedRelocationResync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(rescan.Close())
	assert.NotContains(server.commandsFor(3), "BODY.PEEK[]",
		"a force-full rescan with saved memberships must not refetch copies")

	// The draft vanishes while the sent copy and its mirror remain: the
	// edited snapshot must survive whatever location recovery adopts.
	vanished := overlap.clone()
	vanished.Mailboxes[1].HighestModSeq = 2
	vanished.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
	vanished.Mailboxes[1].Messages = nil
	server.setSnapshot(vanished)
	forced, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(forced.Close())
	requireEditedSent("forced recovery after the draft vanished")

	// Realistic All Mail rebuild (new UIDVALIDITY epoch) while Sent is
	// unchanged: the edited snapshot must survive the location change.
	allMailRebuilt := vanished.clone()
	allMailRebuilt.Mailboxes[0].UIDValidity = 100
	allMailRebuilt.Mailboxes[0].HighestModSeq = 1
	allMailRebuilt.Mailboxes[0].ChangedUIDs = nil
	server.setSnapshot(allMailRebuilt)
	mirror, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(mirror.Close())
	requireEditedSent("All Mail epoch rebuild")

	// Realistic Drafts rebuild that resurrects the stale draft: it must not
	// downgrade the snapshot while the trusted Sent copy remains valid.
	draftsRebuilt := allMailRebuilt.clone()
	draftsRebuilt.Mailboxes[1].UIDValidity = 78
	draftsRebuilt.Mailboxes[1].HighestModSeq = 1
	draftsRebuilt.Mailboxes[1].VanishedUIDs = nil
	draftsRebuilt.Mailboxes[1].Messages = []scriptedRFC7162Message{draft}
	server.setSnapshot(draftsRebuilt)
	stale, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(stale.Close())
	requireEditedSent("stale Draft resurrection")

	// Incremental flag change on the resurrected draft is an ordinary label
	// refresh and must not touch the snapshot.
	flagged := draftsRebuilt.clone()
	flaggedDraft := draft
	flaggedDraft.Flags = []imapapi.Flag{imapapi.FlagSeen}
	flagged.Mailboxes[1].HighestModSeq = 2
	flagged.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	flagged.Mailboxes[1].Messages = []scriptedRFC7162Message{flaggedDraft}
	server.setSnapshot(flagged)
	flagSync, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(flagSync.Close())
	requireEditedSent("draft flag change")

	// Final force-full rescan with every copy present, then a no-op sync.
	finalRescan, err := runScriptedRelocationResync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(finalRescan.Close())
	requireEditedSent("final force-full rescan")
	server.setSnapshot(flagged)
	noop, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(noop.Close())
	requireEditedSent("final no-op sync")

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Sent final", message.Subject)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count)
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{sent.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{draft.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total)
}

// Explicit trusted_imap_sent_mailboxes configuration must not override a
// conflicting advertised role or the INBOX name: those placements are denied
// exactly as when they are advertised without configuration.
func TestIMAPRelocationConfiguredConflictDenied(t *testing.T) {
	for _, mode := range []string{"sent-plus-all", "all-only", "inbox"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			savedCfg := cfg
			t.Cleanup(func() { cfg = savedCfg })
			cfg = config.NewDefaultConfig()

			draft := newScriptedRFC7162Message(1, "configured-conflict@example.test", imapapi.FlagDraft)
			draft.Body = "draftoriginalword"
			sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			sent.Body = "sentfinalword"
			survivorName := "Sent"
			survivorAttrs := []imapapi.MailboxAttr{imapapi.MailboxAttrSent, imapapi.MailboxAttrAll}
			switch mode {
			case "all-only":
				survivorName = "Mirror"
				survivorAttrs = []imapapi.MailboxAttr{imapapi.MailboxAttrAll}
			case "inbox":
				survivorName = "INBOX"
				survivorAttrs = nil
			}
			const identifier = "imap://configured-conflict@example.test"
			cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
				identifier: {survivorName},
			}
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
				{Name: survivorName, Attrs: survivorAttrs, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)

			edited := baseline.clone()
			edited.Mailboxes[0].HighestModSeq = 2
			edited.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
			edited.Mailboxes[0].Messages = nil
			edited.Mailboxes[1].HighestModSeq = 2
			edited.Mailboxes[1].UIDNext = 2
			edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
			edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
			server.setSnapshot(edited)
			second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(second.Close())

			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Equal(survivorName+"|1", message.SourceMessageID, "the location is still adopted")
			assert.Contains(message.BodyText, draft.Body,
				"a configured name must not override a conflicting advertised role or INBOX")
			assert.NotContains(message.BodyText, sent.Body)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(draft), string(raw))
		})
	}
}

// runScriptedRelocationSyncSummary mirrors runScriptedRelocationSync but
// returns the raw Full result, so tests can distinguish a completed run that
// reports per-item errors from an aborted one.
func runScriptedRelocationSyncSummary(
	t *testing.T, st *store.Store, identifier, addr string,
) (*gmail.SyncSummary, error) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(t, err)
	client := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	summary, err := newMessageSyncer(client, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	if err != nil {
		return summary, err
	}
	return summary, saveIMAPFolderStates(
		context.Background(), st, source, client, summary, options.Limit)
}

// A persistently failing relocation target must not starve unrelated mail:
// the failure stays a retryable per-item error, the guarded row keeps its
// snapshot and old composite key, reused keys and later-page duplicates are
// deferred rather than merged, the run reports errors so topology is not
// published, and a later successful retry completes both the relocation and
// the held work.
func TestIMAPRelocationPersistentFailureKeepsUnrelatedMailFlowing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "liveness-target@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	sent.Subject = "Sent final"
	unrelated := func(uid imapapi.UID, n int) scriptedRFC7162Message {
		message := newScriptedRFC7162Message(uid, fmt.Sprintf("unrelated-%d@example.test", n), imapapi.FlagSeen)
		message.Body = fmt.Sprintf("unrelatedbody%d", n)
		return message
	}
	reusedKey := newScriptedRFC7162Message(1, "reused-key-holder@example.test", imapapi.FlagSeen)
	reusedKey.Body = "reusedkeyholderword"
	duplicate := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	duplicate.Body = "duplicatecopyword"

	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(55, 1, 1),
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://liveness-target@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	// Overlap: the sent copy appears while the draft remains, and ten
	// unrelated INBOX messages arrive.
	// Legacy persisted-state precondition: the row keeps its stale Drafts
	// canonical and snapshot while a saved Sent membership exists, seeded
	// through the real topology helper (see the shared relocation fixture),
	// so the failing phase exercises the forced relocation loader. The
	// unrelated INBOX mail has been present since the first sync.
	overlap := baseline.clone()
	overlap.Mailboxes[2].HighestModSeq = 2
	overlap.Mailboxes[2].UIDNext = 2
	overlap.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(overlap)
	seedLegacyDraftsCanonicalWithSentMembership(
		t, st, source, []byte(scriptedRFC7162RawMessage(sent)), "<"+draft.MessageID+">")
	before, err := st.GetMessage(id)
	require.NoError(err)
	beforeStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	beforeMemberships := queryScriptedRFC7162Memberships(t, st, source.ID)
	beforeRaw, err := st.GetMessageRaw(id)
	require.NoError(err)

	// Persistent failure: the draft's mailbox moves to a new epoch whose
	// UID 1 holds an unrelated message (the old composite is reused), the
	// survivor cannot be fetched, two new unrelated messages and a
	// later-page duplicate of the guarded identity arrive.
	failing := overlap.clone()
	failing.Mailboxes[0].UIDNext = 13
	failing.Mailboxes[0].HighestModSeq = 3
	failing.Mailboxes[0].ChangedUIDs = []imapapi.UID{11, 12}
	failing.Mailboxes[0].Messages = append([]scriptedRFC7162Message(nil), overlap.Mailboxes[0].Messages...)
	failing.Mailboxes[0].Messages = append(failing.Mailboxes[0].Messages, unrelated(11, 11), unrelated(12, 12))
	failing.Mailboxes[1].UIDValidity = 99
	failing.Mailboxes[1].HighestModSeq = 1
	failing.Mailboxes[1].VanishedUIDs = nil
	failing.Mailboxes[1].Messages = []scriptedRFC7162Message{reusedKey}
	unfetchable := sent
	unfetchable.MissingRaw = true
	failing.Mailboxes[2].Messages = []scriptedRFC7162Message{unfetchable}
	failing.Mailboxes = append(failing.Mailboxes, scriptedRFC7162Mailbox{
		Name: "ZFolder", UIDValidity: 22, UIDNext: 2, HighestModSeq: 2,
		ChangedUIDs: []imapapi.UID{1}, Messages: []scriptedRFC7162Message{duplicate},
	})
	server.setSnapshot(failing)

	runFailing := func() *gmail.SyncSummary {
		summary, fullErr := runScriptedRelocationSyncSummary(t, st, identifier, addr)
		require.NoError(fullErr, "a per-item relocation failure must complete the run, not abort it")
		require.Positive(summary.Errors)
		return summary
	}
	summary := runFailing()
	assert.Equal(int64(2), summary.MessagesAdded,
		"unrelated new mail must ingest while the relocation target fails")

	after, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal(before.SourceMessageID, after.SourceMessageID, "the guarded row keeps its old composite key")
	assert.Equal(before.BodyText, after.BodyText)
	assert.Equal(before.Labels, after.Labels)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(beforeRaw, raw)
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{draft.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{sent.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total, "the unfetchable survivor's content must not reach the index")
	for _, held := range []string{"reused-key-holder@example.test", "liveness-target@example.test"} {
		heldID, err := st.GetMessageIDByRFC822ID(source.ID, "<"+held+">")
		require.NoError(err)
		if held == "reused-key-holder@example.test" {
			assert.Zero(heldID, "the reused composite key stays deferred while its target fails")
		} else {
			assert.Equal(id, heldID)
		}
	}
	_, duplicateHits, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{"duplicatecopyword"}}, 0, 10)
	require.NoError(err)
	assert.Zero(duplicateHits, "a later-page duplicate must not mutate or join the guarded row")
	afterStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.Equal(beforeStates, afterStates, "a run reporting errors must not publish folder state")
	assert.Equal(beforeMemberships, queryScriptedRFC7162Memberships(t, st, source.ID))

	// A repeated completed attempt behaves identically.
	runFailing()
	after, err = st.GetMessage(id)
	require.NoError(err)
	assert.Equal(before.SourceMessageID, after.SourceMessageID)
	assert.Equal(before.BodyText, after.BodyText)

	// Recovery: the survivor becomes fetchable; relocation, the held reused
	// key, and folder publication all complete.
	recovered := failing.clone()
	recovered.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
	recovered.Mailboxes[2].HighestModSeq = 3
	server.setSnapshot(recovered)
	fixed, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(fixed.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Sent|1", message.SourceMessageID)
	assert.Contains(message.BodyText, sent.Body)
	assert.NotContains(message.BodyText, draft.Body)
	raw, err = st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
	reusedID, err := st.GetMessageIDByRFC822ID(source.ID, "<reused-key-holder@example.test>")
	require.NoError(err)
	require.Positive(reusedID, "the held reused-key message archives after recovery")
	reusedMessage, err := st.GetMessage(reusedID)
	require.NoError(err)
	assert.Equal("Drafts|1", reusedMessage.SourceMessageID)
	assert.Contains(reusedMessage.BodyText, reusedKey.Body)
	assert.NotEqual(id, reusedID, "the held message archives as its own row")
	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	require.Len(states, 4)
	byMailbox := map[string]uint32{}
	for _, state := range states {
		byMailbox[state.Mailbox] = state.UIDValidity
	}
	assert.Equal(uint32(99), byMailbox["Drafts"], "folder state advances after the successful retry")
	known, err := st.GetIMAPKnownUIDs(source.ID)
	require.NoError(err)
	assert.Equal([]uint32{1}, known["Sent"])
}

// cancelOnSecondListIMAP surfaces a context cancellation on the second
// ListMessages call, after the first page's checkpoint has been saved.
type cancelOnSecondListIMAP struct {
	*imap.Client

	calls int
}

func (c *cancelOnSecondListIMAP) ListMessages(
	ctx context.Context, query, pageToken string,
) (*gmail.MessageListResponse, error) {
	c.calls++
	if c.calls == 2 {
		return nil, fmt.Errorf("list messages: %w", context.Canceled)
	}
	return c.Client.ListMessages(ctx, query, pageToken)
}

// An interrupted run that deferred a relocation target must not resume past
// it: the saved cursor stays empty, the next attempt is a fresh replan whose
// forced target runs before ordinary routing, and the reused old composite
// on that first page is deferred rather than consumed.
func TestIMAPRelocationDeferredTargetInterruptedRunRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "interrupted-target@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	reusedKey := newScriptedRFC7162Message(1, "interrupted-reused@example.test", imapapi.FlagSeen)
	reusedKey.Body = "reusedkeyholderword"

	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(55, 1, 1),
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://interrupted-target@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	// Legacy persisted-state precondition: stale Drafts canonical and
	// snapshot with a saved Sent membership, seeded through the real
	// topology helper so the interrupted run exercises forced relocation.
	overlap := baseline.clone()
	overlap.Mailboxes[2].HighestModSeq = 2
	overlap.Mailboxes[2].UIDNext = 2
	overlap.Mailboxes[2].ChangedUIDs = []imapapi.UID{1}
	overlap.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(overlap)
	seedLegacyDraftsCanonicalWithSentMembership(
		t, st, source, []byte(scriptedRFC7162RawMessage(sent)), "<"+draft.MessageID+">")
	before, err := st.GetMessage(id)
	require.NoError(err)
	beforeStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)

	// Failing snapshot: reused old composite, unfetchable survivor, and
	// enough INBOX mail to force a second page.
	failing := overlap.clone()
	// The IMAP client lists in pages of 100, so the reused composite lands
	// on the second page and only a fresh full replan reaches it.
	inbox := []scriptedRFC7162Message{}
	for n := 1; n <= 105; n++ {
		message := newScriptedRFC7162Message(imapapi.UID(n),
			fmt.Sprintf("interrupted-fill-%d@example.test", n), imapapi.FlagSeen)
		message.Body = fmt.Sprintf("fillerbody%d", n)
		inbox = append(inbox, message)
	}
	failing.Mailboxes[0].UIDNext = 106
	failing.Mailboxes[0].HighestModSeq = 2
	failing.Mailboxes[0].ChangedUIDs = nil
	for uid := 1; uid <= 105; uid++ {
		failing.Mailboxes[0].ChangedUIDs = append(failing.Mailboxes[0].ChangedUIDs, imapapi.UID(uid))
	}
	failing.Mailboxes[0].Messages = inbox
	failing.Mailboxes[1].UIDValidity = 99
	failing.Mailboxes[1].HighestModSeq = 1
	failing.Mailboxes[1].VanishedUIDs = nil
	failing.Mailboxes[1].Messages = []scriptedRFC7162Message{reusedKey}
	unfetchable := sent
	unfetchable.MissingRaw = true
	failing.Mailboxes[2].Messages = []scriptedRFC7162Message{unfetchable}
	server.setSnapshot(failing)

	// Interrupt after the first page: the forced target defers, page one
	// completes, then the listing is canceled mid-run.
	source2, err := st.GetSourceByIdentifier(identifier)
	require.NoError(err)
	interrupted := &cancelOnSecondListIMAP{Client: newScriptedRFC7162Client(
		t, addr, imapFolderStateOptions(st, source2, false)...)}
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	_, err = newMessageSyncer(interrupted, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	require.ErrorIs(err, context.Canceled)
	require.NoError(interrupted.Close())

	run, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.False(run.CursorBefore.Valid && run.CursorBefore.String != "",
		"the saved cursor must stay empty so the deferred target cannot be resumed past")

	// The next attempt must be a fresh replan even with resume enabled: the
	// forced target re-runs ahead of ordinary routing, the reused composite
	// is deferred, and the guarded row is untouched.
	resumableOptions := msgsync.DefaultOptions()
	resumableOptions.SourceType = sourceTypeIMAP
	resumable := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source2, false)...)
	summary, err := newMessageSyncer(resumable, st, resumableOptions).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	require.NoError(err, "the retry attempt completes with per-item errors")
	require.NoError(resumable.Close())
	assert.False(summary.WasResumed, "an empty saved cursor must force a fresh replan, not a resume")
	assert.Positive(summary.Errors)
	after, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal(before.SourceMessageID, after.SourceMessageID,
		"protection is rebuilt before ordinary routing on the fresh replan")
	assert.Equal(before.BodyText, after.BodyText)
	reusedID, err := st.GetMessageIDByRFC822ID(source.ID, "<interrupted-reused@example.test>")
	require.NoError(err)
	assert.Zero(reusedID, "the reused old composite stays deferred across the restart")
	afterStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.Equal(beforeStates, afterStates)

	// Recovery completes the relocation and publishes topology on the fresh
	// successful retry.
	recovered := failing.clone()
	recovered.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
	recovered.Mailboxes[2].HighestModSeq = 3
	server.setSnapshot(recovered)
	fixed, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(fixed.Close())
	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("Sent|1", message.SourceMessageID)
	assert.Contains(message.BodyText, sent.Body)
	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	require.Len(states, 3)
	byMailbox := map[string]uint32{}
	for _, state := range states {
		byMailbox[state.Mailbox] = state.UIDValidity
	}
	assert.Equal(uint32(99), byMailbox["Drafts"], "the fresh successful retry publishes folder state")
}

// A saved All Mail alias whose preferred label-validation/rekey route runs
// while the selected All Mail candidate persistently fails must not consume
// the failed target's old archive key: that key is what rediscovers the
// candidate on the next attempt. The client's own listing picks the canonical
// All Mail representative and the relocation selector picks the target; the
// flag-changed alias appears on a later page through ordinary routing.
func TestIMAPRelocationFailedTargetAliasDoesNotConsumeOldKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "alias-target@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	mirror := func(uid imapapi.UID) scriptedRFC7162Message {
		message := newScriptedRFC7162Message(uid, draft.MessageID, imapapi.FlagSeen)
		message.Body = "draftmirrorword"
		return message
	}
	filler := func(n int) scriptedRFC7162Message {
		message := newScriptedRFC7162Message(imapapi.UID(n),
			fmt.Sprintf("alias-fill-%d@example.test", n), imapapi.FlagSeen)
		message.Body = fmt.Sprintf("fillerbody%d", n)
		return message
	}

	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(55, 1, 1),
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 44, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://alias-target@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	// Binding sync: three All Mail mirrors and filler INBOX mail appear.
	// The client's listing selects one mirror as the canonical All Mail
	// representative; the others are stubbed and saved as memberships.
	binding := baseline.clone()
	binding.Mailboxes[0].UIDNext = 104
	binding.Mailboxes[0].HighestModSeq = 2
	binding.Mailboxes[0].ChangedUIDs = nil
	for uid := 1; uid <= 103; uid++ {
		binding.Mailboxes[0].ChangedUIDs = append(binding.Mailboxes[0].ChangedUIDs, imapapi.UID(uid))
		binding.Mailboxes[0].Messages = append(binding.Mailboxes[0].Messages, filler(uid))
	}
	binding.Mailboxes[2].UIDNext = 4
	binding.Mailboxes[2].HighestModSeq = 2
	binding.Mailboxes[2].ChangedUIDs = []imapapi.UID{1, 2, 3}
	binding.Mailboxes[2].Messages = []scriptedRFC7162Message{mirror(1), mirror(2), mirror(3)}
	server.setSnapshot(binding)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())
	bound, err := st.GetMessage(id)
	require.NoError(err)
	canonicalKey := bound.SourceMessageID
	require.Regexp("^All Mail\\|\\d$", canonicalKey,
		"preferred adoption takes an All Mail canonical key")
	canonicalUID := int(canonicalKey[len("All Mail|")] - '0')
	var others []int
	for uid := 1; uid <= 3; uid++ {
		if uid != canonicalUID {
			others = append(others, uid)
		}
	}
	require.Len(others, 2)
	beforeStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)

	// Failing sync: the canonical mirror vanishes, the selector's candidate
	// cannot be fetched, the remaining alias only changes flags, and two new
	// unrelated messages arrive. Enough filler keeps the flag-changed alias
	// on the second page.
	failing := binding.clone()
	failing.Mailboxes[0].UIDNext = 106
	failing.Mailboxes[0].HighestModSeq = 3
	failing.Mailboxes[0].ChangedUIDs = []imapapi.UID{104, 105}
	failing.Mailboxes[0].Messages = append([]scriptedRFC7162Message(nil), binding.Mailboxes[0].Messages...)
	for _, n := range []int{104, 105} {
		message := newScriptedRFC7162Message(imapapi.UID(n),
			fmt.Sprintf("alias-new-%d@example.test", n), imapapi.FlagSeen)
		message.Body = fmt.Sprintf("newbody%d", n)
		failing.Mailboxes[0].Messages = append(failing.Mailboxes[0].Messages, message)
	}
	unreadable := mirror(imapapi.UID(others[0]))
	unreadable.MissingRaw = true
	flagged := mirror(imapapi.UID(others[1]))
	flagged.Flags = []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged}
	failing.Mailboxes[2].HighestModSeq = 3
	failing.Mailboxes[2].VanishedUIDs = []imapapi.UID{imapapi.UID(canonicalUID)}
	failing.Mailboxes[2].ChangedUIDs = []imapapi.UID{imapapi.UID(others[1])}
	failing.Mailboxes[2].Messages = []scriptedRFC7162Message{unreadable, flagged}
	server.setSnapshot(failing)

	summary, fullErr := runScriptedRelocationSyncSummary(t, st, identifier, addr)
	require.NoError(fullErr, "the failing candidate defers instead of aborting the run")
	require.Positive(summary.Errors)
	assert.Equal(int64(2), summary.MessagesAdded, "unrelated new mail still ingests")

	after, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal(canonicalKey, after.SourceMessageID,
		"the flag-changed alias's preferred rekey must not consume the failed target's old key")
	assert.Contains(after.BodyText, draft.Body)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(draft), string(raw))
	afterStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.Equal(beforeStates, afterStates, "a run reporting errors publishes no folder state")

	// A repeated failing attempt stays stable.
	summary, fullErr = runScriptedRelocationSyncSummary(t, st, identifier, addr)
	require.NoError(fullErr)
	require.Positive(summary.Errors)
	after, err = st.GetMessage(id)
	require.NoError(err)
	assert.Equal(canonicalKey, after.SourceMessageID)

	// Recovery: the candidate becomes fetchable; relocation and publication
	// complete on the fresh successful attempt.
	recovered := failing.clone()
	recovered.Mailboxes[2].Messages = []scriptedRFC7162Message{mirror(imapapi.UID(others[0])), flagged}
	recovered.Mailboxes[2].HighestModSeq = 4
	server.setSnapshot(recovered)
	fixed, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(fixed.Close())
	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, draft.Body,
		"the untrusted All Mail relocation rekeys without replacing content")
	_, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{draft.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	require.Len(states, 3)
}

// immediateLabelScriptedIMAP runs the production client with authoritative
// label reconciliation enabled, so the untrusted forced adoption executes
// its label step in the same call as the rekey.
type immediateLabelScriptedIMAP struct {
	*imap.Client
}

func (*immediateLabelScriptedIMAP) DefersAuthoritativeLabelReconciliation() bool {
	return false
}

// An untrusted forced adoption whose label reconciliation fails late must
// keep the old composite key and the guarded row untouched: rekey and labels
// commit in one guarded transaction, leaving the failed target retryable.
func TestIMAPRelocationUntrustedAdoptionKeepsOldKeyOnLabelFailure(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail the label write")
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "atomic-forced@example.test", imapapi.FlagSeen)
	draft.Body = "survivorbodyword"
	survivor := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	survivor.Body = "survivorbodyword"

	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "OldBox", UIDValidity: 66, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "NewBox", UIDValidity: 77, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://atomic-forced@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	// Overlap binds the untrusted survivor's membership.
	overlap := baseline.clone()
	overlap.Mailboxes[1].HighestModSeq = 2
	overlap.Mailboxes[1].UIDNext = 2
	overlap.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	overlap.Mailboxes[1].Messages = []scriptedRFC7162Message{survivor}
	server.setSnapshot(overlap)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())
	before, err := st.GetMessage(id)
	require.NoError(err)
	require.Equal("OldBox|1", before.SourceMessageID)
	beforeStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)

	// The canonical mailbox rebuilds (new epoch), making the untrusted
	// survivor the forced target. Label writes for the guarded row fail.
	failing := overlap.clone()
	failing.Mailboxes[0].UIDValidity = 67
	failing.Mailboxes[0].HighestModSeq = 1
	failing.Mailboxes[0].Messages = nil
	server.setSnapshot(failing)
	_, triggerErr := st.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_forced_labels_insert
		BEFORE INSERT ON message_labels
		WHEN NEW.message_id = %d
		BEGIN
			SELECT RAISE(ABORT, 'forced label write unavailable');
		END`, id))
	require.NoError(triggerErr)
	_, triggerErr = st.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_forced_labels_delete
		BEFORE DELETE ON message_labels
		WHEN OLD.message_id = %d
		BEGIN
			SELECT RAISE(ABORT, 'forced label write unavailable');
		END`, id))
	require.NoError(triggerErr)
	t.Cleanup(func() {
		_, _ = st.DB().Exec(`DROP TRIGGER IF EXISTS fail_forced_labels_insert`)
		_, _ = st.DB().Exec(`DROP TRIGGER IF EXISTS fail_forced_labels_delete`)
	})

	runImmediate := func() (*gmail.SyncSummary, error) {
		src, srcErr := st.GetSourceByIdentifier(identifier)
		require.NoError(srcErr)
		client := &immediateLabelScriptedIMAP{Client: newScriptedRFC7162Client(
			t, addr, imapFolderStateOptions(st, src, false)...)}
		options := msgsync.DefaultOptions()
		options.SourceType = sourceTypeIMAP
		options.NoResume = true
		summary, err := newMessageSyncer(client, st, options).
			WithLogger(slog.New(slog.DiscardHandler)).
			Full(t.Context(), identifier)
		if closeErr := client.Close(); err == nil {
			err = closeErr
		}
		return summary, err
	}
	summary, fullErr := runImmediate()
	require.NoError(fullErr, "the failed adoption defers instead of aborting the run")
	require.Positive(summary.Errors)

	after, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("OldBox|1", after.SourceMessageID,
		"a failed untrusted adoption must not consume the old composite key")
	assert.Equal(before.Labels, after.Labels)
	assert.Contains(after.BodyText, draft.Body)
	afterStates, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	assert.Equal(beforeStates, afterStates, "a run reporting errors publishes no folder state")

	// Removing the fault lets the retry adopt the untrusted location with
	// reconciled labels and publish topology.
	_, err = st.DB().Exec(`DROP TRIGGER fail_forced_labels_insert`)
	require.NoError(err)
	_, err = st.DB().Exec(`DROP TRIGGER fail_forced_labels_delete`)
	require.NoError(err)
	fixed, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(fixed.Close())
	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal("NewBox|1", message.SourceMessageID)
	assert.Contains(message.BodyText, draft.Body)
	states, err := st.GetIMAPFolderStates(source.ID)
	require.NoError(err)
	require.Len(states, 2)
}

// A full-enumeration run (initial archive, rescan, or QRESYNC-ineligible
// fallback) fetches the untrusted All mirror before the trusted Sent copy.
// The cross-copy RFC822 dedup must not turn that trusted copy into a
// raw-less stub: the stale archived draft snapshot then never refreshes even
// though the run held the only trusted copy it needs.
func TestIMAPRelocationFullEnumerationRefreshesEditedSent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "full-enum-refresh@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	sent.Subject = "Sent final"

	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 1, HighestModSeq: 1},
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://full-enum-refresh@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)
	stale, err := st.GetMessage(id)
	require.NoError(err)
	require.Contains(stale.BodyText, draft.Body, "the archived snapshot starts as the draft")

	// The draft vanishes while the edited Sent copy and its All mirror
	// appear. QRESYNC is unavailable, so the run enumerates fully and the
	// All mirror is fetched ahead of the trusted Sent copy.
	edited := baseline.clone()
	edited.Capabilities = "IMAP4rev1 SPECIAL-USE"
	edited.Mailboxes[0].HighestModSeq = 2
	edited.Mailboxes[0].UIDNext = 2
	edited.Mailboxes[0].Messages = []scriptedRFC7162Message{sent}
	edited.Mailboxes[1].HighestModSeq = 2
	edited.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
	edited.Mailboxes[1].Messages = nil
	edited.Mailboxes[2].HighestModSeq = 2
	edited.Mailboxes[2].UIDNext = 2
	edited.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(edited)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, sent.Body,
		"the trusted Sent copy must refresh the stale draft snapshot in a full-enumeration run")
	assert.NotContains(message.BodyText, draft.Body)
	assert.Equal("Sent final", message.Subject)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count, "the duplicate copies collapse into one row")
	results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{sent.Body}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID, "the internal ID is retained")
	_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{draft.Body}}, 0, 10)
	require.NoError(err)
	assert.Zero(total, "the stale draft body leaves the search index")

	server.setSnapshot(edited)
	third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(third.Close())
	message, err = st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, sent.Body)
	assert.NotContains(message.BodyText, draft.Body)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
	assert.Equal(1, count)
}

// runScriptedSourceSync runs a full production sync for one IMAP source
// against its own scripted server under the current global configuration.
func runScriptedSourceSync(
	t *testing.T, st *store.Store, identifier, addr string,
	mod func(*msgsync.Options),
) (*imap.Client, *store.Source, error) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(t, err)
	client := newScriptedRFC7162Client(t, addr, imapFolderStateOptions(st, source, false)...)
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	options.NoResume = true
	if mod != nil {
		mod(options)
	}
	summary, err := newMessageSyncer(client, st, options).
		WithLogger(slog.New(slog.DiscardHandler)).
		Full(t.Context(), identifier)
	if err != nil {
		return client, source, err
	}
	if summary.Errors != 0 {
		return client, source, fmt.Errorf(
			"scripted IMAP sync completed with %d errors", summary.Errors)
	}
	return client, source, saveIMAPFolderStates(
		context.Background(), st, source, client, summary, options.Limit)
}

// Sent-folder trust is scoped to the exact IMAP source identifier: two
// accounts with the same localized folder name, only one of them configured,
// must behave differently. The configured account refreshes an edited
// outgoing copy even in a full-enumeration run with a mirror fetched first;
// the unconfigured account's same-named folder cannot replace an archived
// snapshot, its raw MIME, participants, attachments, or search content, and
// rejected bytes never reach remote-image processing.
func TestIMAPRelocationSentTrustIsSourceScoped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.Sync.ArchiveRemoteImages = true
	const identifierA = "imap://scoped-a@example.test"
	const identifierB = "imap://scoped-b@example.test"
	cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
		identifierA: {"Gesendete Elemente"},
	}

	// Source A: configured localized Sent folder, no advertised roles. The
	// run enumerates fully (no QRESYNC), so the untrusted mirror is fetched
	// before the configured Sent copy.
	draftA := newScriptedRFC7162Message(1, "scoped-a@example.test", imapapi.FlagDraft)
	draftA.Body = "draftaword"
	sentA := newScriptedRFC7162Message(1, draftA.MessageID, imapapi.FlagSeen)
	sentA.Body = "sentafinalword"
	baselineA := scriptedRFC7162Snapshot{Capabilities: "IMAP4rev1 SPECIAL-USE", Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "Alles", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 1, HighestModSeq: 1},
		{Name: "Entw&APw-rfe", UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draftA}},
		{Name: "Gesendete Elemente", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addrA, serverA := startScriptedRFC7162Server(t, baselineA)
	stA := testutil.NewTestStore(t)
	withAttachments := func(o *msgsync.Options) { o.AttachmentsDir = t.TempDir() }
	firstA, sourceA, err := runScriptedSourceSync(t, stA, identifierA, addrA, withAttachments)
	require.NoError(err)
	require.NoError(firstA.Close())
	idA, err := stA.GetMessageIDByRFC822ID(sourceA.ID, "<"+draftA.MessageID+">")
	require.NoError(err)
	require.Positive(idA)

	editedA := baselineA.clone()
	editedA.Mailboxes[0].HighestModSeq = 2
	editedA.Mailboxes[0].UIDNext = 2
	editedA.Mailboxes[0].Messages = []scriptedRFC7162Message{sentA}
	editedA.Mailboxes[1].HighestModSeq = 2
	editedA.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
	editedA.Mailboxes[1].Messages = nil
	editedA.Mailboxes[2].HighestModSeq = 2
	editedA.Mailboxes[2].UIDNext = 2
	editedA.Mailboxes[2].Messages = []scriptedRFC7162Message{sentA}
	serverA.setSnapshot(editedA)
	secondA, _, err := runScriptedSourceSync(t, stA, identifierA, addrA, withAttachments)
	require.NoError(err)
	require.NoError(secondA.Close())
	messageA, err := stA.GetMessage(idA)
	require.NoError(err)
	assert.Contains(messageA.BodyText, sentA.Body,
		"the configured source refreshes the edited copy despite the mirror-first full enumeration")
	assert.NotContains(messageA.BodyText, draftA.Body)

	// Source B: same folder name, no configuration. A forged Message-ID
	// survivor there must not replace the archived snapshot.
	victimB := newScriptedRFC7162Message(1, "scoped-b@example.test", imapapi.FlagSeen)
	victimB.Body = "victimbword"
	victimB.Raw = scriptedOutgoingRaw(
		victimB.MessageID, "Original B",
		"victim-to@example.test", "victim-cc@example.test",
		"victimbword", "<p>victimbword</p>",
		"victimb.bin", "victim b attachment bytes")
	forgerB := newScriptedRFC7162Message(2, victimB.MessageID, imapapi.FlagSeen)
	forgerB.Raw = scriptedOutgoingRaw(
		victimB.MessageID, "Forged B",
		"forger-to@example.test", "forger-cc@example.test",
		"forgerbword",
		"<p>forgerbword <img src=\"http://images.example/forger-b\"></p>",
		"forgerb.bin", "forger b attachment bytes")
	baselineB := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		scriptedRFC7162Inbox(55, 2, 1, victimB),
		{Name: "Gesendete Elemente", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addrB, serverB := startScriptedRFC7162Server(t, baselineB)
	stB := testutil.NewTestStore(t)
	firstB, sourceB, err := runScriptedSourceSync(t, stB, identifierB, addrB, withAttachments)
	require.NoError(err)
	require.NoError(firstB.Close())
	idB, err := stB.GetMessageIDByRFC822ID(sourceB.ID, "<"+victimB.MessageID+">")
	require.NoError(err)
	require.Positive(idB)
	beforeB, err := stB.GetMessage(idB)
	require.NoError(err)
	require.Len(beforeB.Attachments, 1)

	replacedB := baselineB.clone()
	replacedB.Mailboxes[0].HighestModSeq = 2
	replacedB.Mailboxes[0].UIDNext = 3
	replacedB.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
	replacedB.Mailboxes[0].Messages = nil
	replacedB.Mailboxes[1].HighestModSeq = 2
	replacedB.Mailboxes[1].UIDNext = 3
	replacedB.Mailboxes[1].ChangedUIDs = []imapapi.UID{2}
	replacedB.Mailboxes[1].Messages = []scriptedRFC7162Message{forgerB}
	serverB.setSnapshot(replacedB)
	secondB, _, err := runScriptedSourceSync(t, stB, identifierB, addrB, withAttachments)
	require.NoError(err)
	require.NoError(secondB.Close())

	messageB, err := stB.GetMessage(idB)
	require.NoError(err)
	assert.Equal("Gesendete Elemente|2", messageB.SourceMessageID,
		"the surviving location in the same-named folder is still adopted")
	assert.Contains(messageB.BodyText, victimB.Body,
		"the unconfigured source's same-named folder cannot replace the snapshot")
	assert.NotContains(messageB.BodyText, "forgerbword")
	assert.Equal([]string{"victim-to@example.test"}, messageB.To)
	assert.Equal([]string{"victim-cc@example.test"}, messageB.Cc)
	require.Len(messageB.Attachments, 1)
	assert.Equal("victimb.bin", messageB.Attachments[0].Filename)
	rawB, err := stB.GetMessageRaw(idB)
	require.NoError(err)
	assert.Equal(victimB.Raw, string(rawB))
	_, hitsB, err := stB.SearchMessagesQuery(&search.Query{TextTerms: []string{"forgerbword"}}, 0, 10)
	require.NoError(err)
	assert.Zero(hitsB, "forged content must not reach the search index")
	refsB, err := stB.MessageRemoteImages(idB)
	require.NoError(err)
	assert.Empty(refsB,
		"rejected bytes must not create remote-image references; the no-HTTP-request guarantee is covered by the sync-level counter test")
}

// A configured name the server itself advertises as \Drafts keeps its Drafts
// meaning: explicit configuration cannot turn a Drafts folder into the
// account's Sent folder and grant its duplicate copies the trusted dedup
// bypass that would let a stale draft downgrade a fresh snapshot.
func TestIMAPRelocationConfiguredDraftsRoleNotSent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	const identifier = "imap://configured-drafts@example.test"
	cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
		identifier: {"My Drafts"},
	}

	sent := newScriptedRFC7162Message(1, "configured-drafts@example.test", imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	staleDraft := newScriptedRFC7162Message(1, sent.MessageID, imapapi.FlagDraft)
	staleDraft.Body = "draftoriginalword"

	baseline := scriptedRFC7162Snapshot{Capabilities: "IMAP4rev1 SPECIAL-USE", Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 1, HighestModSeq: 1},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{sent}},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+sent.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	// The All mirror is fetched first in the full enumeration; the stale
	// draft copy in the configured-but-advertised-Drafts mailbox arrives
	// second and must stay a dedup stub.
	failing := baseline.clone()
	failing.Mailboxes[0].HighestModSeq = 2
	failing.Mailboxes[0].UIDNext = 2
	failing.Mailboxes[0].Messages = []scriptedRFC7162Message{sent}
	failing.Mailboxes = append(failing.Mailboxes, scriptedRFC7162Mailbox{
		Name: "My Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts},
		UIDValidity: 66, UIDNext: 2, HighestModSeq: 2, ChangedUIDs: []imapapi.UID{1},
		Messages: []scriptedRFC7162Message{staleDraft},
	})
	server.setSnapshot(failing)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, sent.Body,
		"a configured Drafts-role name must not gain the Sent dedup bypass")
	assert.NotContains(message.BodyText, staleDraft.Body)
}

// An edited Sent copy must refresh a stale archived Drafts snapshot even
// while the old Drafts copy still exists and remains canonical: Sent
// placement outranks Drafts placement for content precedence. Received
// placements never gain that precedence, and a later stale Drafts copy must
// not downgrade the refreshed snapshot.
func TestIMAPRelocationSentOutranksStaleDraftsCanonical(t *testing.T) {
	for _, mode := range []string{"qresync", "full enumeration", "configured localized sent", "removed sent mailbox", "removed sent mailbox full enumeration"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			savedCfg := cfg
			t.Cleanup(func() { cfg = savedCfg })
			cfg = config.NewDefaultConfig()
			draft := newScriptedRFC7162Message(1, "sent-precedence@example.test", imapapi.FlagDraft)
			draft.Body = "draftoriginalword"
			sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			sent.Body = "sentfinalword"
			sent.Subject = "Sent final"
			sentMailbox := scriptedRFC7162Mailbox{
				Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent},
				UIDValidity: 88, UIDNext: 1, HighestModSeq: 1,
			}
			if mode == "configured localized sent" {
				sentMailbox = scriptedRFC7162Mailbox{
					Name: "Gesendete Elemente", UIDValidity: 88, UIDNext: 1, HighestModSeq: 1,
				}
				const identifier = "imap://sent-precedence@example.test"
				cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
					identifier: {"Gesendete Elemente"},
				}
			}
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
				sentMailbox,
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://sent-precedence@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)
			stale, err := st.GetMessage(id)
			require.NoError(err)
			require.Contains(stale.BodyText, draft.Body, "the archived snapshot starts as the draft")

			edited := baseline.clone()
			if mode == "full enumeration" || mode == "removed sent mailbox full enumeration" {
				edited.Capabilities = "IMAP4rev1 SPECIAL-USE"
			}
			edited.Mailboxes[1].HighestModSeq = 2
			edited.Mailboxes[1].UIDNext = 2
			edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
			edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
			server.setSnapshot(edited)
			second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(second.Close())

			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Contains(message.BodyText, sent.Body,
				"the edited Sent copy must refresh the stale draft while Drafts remains")
			assert.NotContains(message.BodyText, draft.Body)
			assert.Equal("Sent final", message.Subject)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
			assert.Equal(1, count)
			results, total, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{sent.Body}}, 0, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
			require.Len(results, 1)
			assert.Equal(id, results[0].ID)
			_, total, err = st.SearchMessagesQuery(&search.Query{TextTerms: []string{draft.Body}}, 0, 10)
			require.NoError(err)
			assert.Zero(total, "the stale draft body leaves the search index")

			// A repeated run must not let the surviving stale Drafts copy
			// downgrade the refreshed snapshot.
			server.setSnapshot(edited)
			third, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(third.Close())
			message, err = st.GetMessage(id)
			require.NoError(err)
			assert.Contains(message.BodyText, sent.Body)
			assert.NotContains(message.BodyText, draft.Body)
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&count))
			assert.Equal(1, count)

			// Losing the Sent copy must only relocate the archive to Drafts,
			// not replace its final content with the surviving stale draft.
			require.Equal(sentMailbox.Name+"|1", message.SourceMessageID)
			require.Len(queryScriptedRFC7162Memberships(t, st, source.ID), 2)
			edited.Mailboxes[1].Messages = nil
			edited.Mailboxes[1].HighestModSeq = 3
			edited.Mailboxes[1].ChangedUIDs = nil
			edited.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
			removeMailbox := mode == "removed sent mailbox" || mode == "removed sent mailbox full enumeration"
			if removeMailbox {
				edited.Mailboxes = edited.Mailboxes[:1]
			}
			for range 2 {
				server.setSnapshot(edited)
				client, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
				require.NoError(err)
				require.NoError(client.Close())
				message, err = st.GetMessage(id)
				require.NoError(err)
				assert.Equal("Drafts|1", message.SourceMessageID)
				assert.Contains(message.BodyText, sent.Body)
				assert.NotContains(message.BodyText, draft.Body)
				raw, err = st.GetMessageRaw(id)
				require.NoError(err)
				assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
				if !removeMailbox {
					edited.Mailboxes[1].VanishedUIDs = nil
				}
			}
		})
	}
}

// An advertised \Drafts role outranks explicit Sent configuration: a
// mistakenly configured Drafts canonical is still recognized as Drafts and
// yields to a genuine Sent copy, while never itself gaining the Sent
// placement's dedup bypass.
func TestIMAPRelocationConfiguredDraftsCanonicalStillYieldsToSent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	const identifier = "imap://configured-drafts-canonical@example.test"
	cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
		identifier: {"My Drafts"},
	}

	draft := newScriptedRFC7162Message(1, "configured-drafts-canonical@example.test", imapapi.FlagDraft)
	draft.Body = "draftoriginalword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	sent.Subject = "Sent final"

	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "My Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 66, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
	require.NoError(err)
	require.Positive(id)

	edited := baseline.clone()
	edited.Mailboxes[1].HighestModSeq = 2
	edited.Mailboxes[1].UIDNext = 2
	edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
	edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
	server.setSnapshot(edited)
	second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(err)
	require.NoError(second.Close())

	message, err := st.GetMessage(id)
	require.NoError(err)
	assert.Contains(message.BodyText, sent.Body,
		"an advertised \\Drafts canonical yields to a genuine Sent copy despite the mistaken configuration")
	assert.NotContains(message.BodyText, draft.Body)
}

// seedLegacyDraftsCanonicalWithSentMembership persists the archive state that
// predates Sent-over-Drafts overlap refresh through the real production
// topology helper: the row keeps its stale Drafts canonical and snapshot while
// a saved Sent membership and both mailbox cursors exist, so a later sync
// rediscovers the Sent survivor through the forced relocation loader. Real
// membership and folder-state persistence keep the recovery path identical to
// a legacy archive's.
func seedLegacyDraftsCanonicalWithSentMembership(
	t *testing.T,
	st *store.Store,
	source *store.Source,
	sentRaw []byte,
	rfc822 string,
) {
	t.Helper()
	require.NoError(t, st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "Drafts",
			State: store.IMAPFolderState{
				Mailbox: "Drafts", UIDValidity: 77, UIDNext: 2, HighestModSeq: 1,
			},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Drafts", UIDValidity: 77, UID: 1,
				SourceMessageID: "Drafts|1", RFC822MessageID: rfc822,
				Flags: []string{"\\Draft"},
			}},
		},
		{
			Mailbox: "Sent",
			State: store.IMAPFolderState{
				Mailbox: "Sent", UIDValidity: 88, UIDNext: 2, HighestModSeq: 2,
			},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Sent", UIDValidity: 88, UID: 1,
				SourceMessageID: "Sent|1", RFC822MessageID: rfc822,
				RawSHA256: sha256.Sum256(sentRaw),
				RawSize:   int64(len(sentRaw)),
				Flags:     []string{"\\Seen"},
			}},
		},
	}))
}

// A mailbox the server advertises with BOTH \Sent and \Drafts is ambiguous:
// it must neither authorize snapshot replacement nor gain the Sent placement
// precedence or dedup bypass, including when the account lists it explicitly.
func TestIMAPRelocationDualSentDraftsRoleDenied(t *testing.T) {
	for _, mode := range []string{"advertised", "advertised and configured"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			savedCfg := cfg
			t.Cleanup(func() { cfg = savedCfg })
			cfg = config.NewDefaultConfig()
			draft := newScriptedRFC7162Message(1, "dual-role@example.test", imapapi.FlagDraft)
			draft.Body = "draftoriginalword"
			sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			sent.Body = "sentfinalword"
			const identifier = "imap://dual-role@example.test"
			if mode == "advertised and configured" {
				cfg.Sync.TrustedIMAPSentMailboxes = map[string][]string{
					identifier: {"Outgoing"},
				}
			}
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
				{Name: "Outgoing", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent, imapapi.MailboxAttrDrafts}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)

			edited := baseline.clone()
			edited.Mailboxes[0].HighestModSeq = 2
			edited.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
			edited.Mailboxes[0].Messages = nil
			edited.Mailboxes[1].HighestModSeq = 2
			edited.Mailboxes[1].UIDNext = 2
			edited.Mailboxes[1].ChangedUIDs = []imapapi.UID{1}
			edited.Mailboxes[1].Messages = []scriptedRFC7162Message{sent}
			server.setSnapshot(edited)
			second, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(second.Close())

			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Equal("Outgoing|1", message.SourceMessageID,
				"the surviving location in the ambiguous mailbox is still adopted")
			assert.Contains(message.BodyText, draft.Body,
				"an ambiguous \\Sent+\\Drafts advertisement must not authorize snapshot replacement")
			assert.NotContains(message.BodyText, sent.Body)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(draft), string(raw))
		})
	}
}

func TestIMAPRelocationSentContentSurvivesAllAdoption(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	draft := newScriptedRFC7162Message(1, "survivors@example.test", imapapi.FlagDraft)
	draft.Body = "draftobsoleteword"
	sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
	sent.Body = "sentfinalword"
	// The mirror starts with the draft bytes so Sent supplies an edited
	// snapshot and becomes canonical before its later disappearance.
	baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
		{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
		{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{sent}},
	}}
	addr, server := startScriptedRFC7162Server(t, baseline)
	st := testutil.NewTestStore(t)
	const identifier = "imap://survivors@example.test"
	first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
	require.NoError(first.Close())
	id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+sent.MessageID+">")
	require.NoError(err)
	require.Positive(id)
	before, err := st.GetMessage(id)
	require.NoError(err)
	require.Equal("Sent|1", before.SourceMessageID)
	require.Contains(before.BodyText, sent.Body)
	require.Len(queryScriptedRFC7162Memberships(t, st, source.ID), 3)
	final := baseline.clone()
	final.Mailboxes[2].Messages = nil
	final.Mailboxes[2].HighestModSeq = 3
	final.Mailboxes[2].VanishedUIDs = []imapapi.UID{1}
	for range 2 {
		server.setSnapshot(final)
		client, err := runScriptedRelocationResync(t, st, identifier, addr)
		require.NoError(err)
		require.NoError(client.Close())
		after, err := st.GetMessage(id)
		require.NoError(err)
		assert.Contains(after.BodyText, sent.Body)
		assert.NotContains(after.BodyText, draft.Body)
		assert.Equal("All Mail|1", after.SourceMessageID)
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		assert.Equal(scriptedRFC7162RawMessage(sent), string(raw))
		final.Mailboxes[2].VanishedUIDs = nil
	}
}

// Keep the existing placement policy for archives without origin metadata,
// and continue refreshing a Sent survivor when the original Sent copy is gone.
func TestIMAPRelocationRetainsOutgoingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		survivorRole    imapapi.MailboxAttr
		missingMetadata bool
		removeCanonical bool
		wantRefresh     bool
	}{
		{name: "sent survivor refreshes", survivorRole: imapapi.MailboxAttrSent, removeCanonical: true, wantRefresh: true},
		{name: "existing sent without metadata", survivorRole: imapapi.MailboxAttrDrafts, missingMetadata: true},
		{name: "lost sent without metadata", survivorRole: imapapi.MailboxAttrDrafts, missingMetadata: true, removeCanonical: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			original := newScriptedRFC7162Message(1, "outgoing-precedence@example.test", imapapi.FlagSeen)
			original.Body = "originalsentword"
			survivor := original
			survivor.Body = "survivingcopyword"
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{original}},
				{Name: "Survivor", Attrs: []imapapi.MailboxAttr{tc.survivorRole}, UIDValidity: 77, UIDNext: 1, HighestModSeq: 1},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://outgoing-precedence@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+original.MessageID+">")
			require.NoError(err)
			require.Positive(id)
			if tc.missingMetadata {
				_, err := st.DB().Exec(st.Rebind("UPDATE messages SET metadata = NULL WHERE id = ?"), id)
				require.NoError(err)
			}
			next := baseline.clone()
			next.Mailboxes[1].Messages = []scriptedRFC7162Message{survivor}
			next.Mailboxes[1].UIDNext = 2
			next.Mailboxes[1].HighestModSeq = 2
			wantSource := "Sent|1"
			if tc.removeCanonical {
				next.Mailboxes[0].Messages = nil
				next.Mailboxes[0].HighestModSeq = 2
				next.Mailboxes[0].VanishedUIDs = []imapapi.UID{1}
				wantSource = "Survivor|1"
			}
			server.setSnapshot(next)
			second, err := runScriptedRelocationResync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(second.Close())
			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Equal(wantSource, message.SourceMessageID)
			want := original
			if tc.wantRefresh {
				want = survivor
			}
			assert.Contains(message.BodyText, want.Body)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(want), string(raw))
		})
	}
}

// An unchanged Sent membership must still supply the final content when a
// lost Drafts canonical also has an All Mail survivor in the changed set.
func TestIMAPRelocationUnchangedSentOutranksChangedAll(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newRelocationFixture(t)
	rawSent := []byte(scriptedRFC7162RawMessage(f.sent))
	rfc822 := "<" + f.sent.MessageID + ">"
	// Keep the legacy draft snapshot and all three prior memberships. The
	// next incremental sync loses Drafts and sees only an All Mail flag change.
	require.NoError(f.st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "Drafts",
			State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 77, UIDNext: 2, HighestModSeq: 1},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Drafts", UIDValidity: 77, UID: 1, SourceMessageID: "Drafts|1",
				RFC822MessageID: rfc822, Flags: []string{"\\Draft"},
			}},
		},
		{
			Mailbox: "Sent",
			State:   store.IMAPFolderState{Mailbox: "Sent", UIDValidity: 88, UIDNext: 2, HighestModSeq: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Sent", UIDValidity: 88, UID: 1, SourceMessageID: "Sent|1",
				RFC822MessageID: rfc822, RawSHA256: sha256.Sum256(rawSent), RawSize: int64(len(rawSent)), Flags: []string{"\\Seen"},
			}},
		},
		{
			Mailbox: "All Mail",
			State:   store.IMAPFolderState{Mailbox: "All Mail", UIDValidity: 99, UIDNext: 2, HighestModSeq: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "All Mail", UIDValidity: 99, UID: 1, SourceMessageID: "All Mail|1",
				RFC822MessageID: rfc822, RawSHA256: sha256.Sum256(rawSent), RawSize: int64(len(rawSent)), Flags: []string{"\\Seen"},
			}},
		},
	}))
	require.Len(queryScriptedRFC7162Memberships(t, f.st, f.source.ID), 3)
	before, err := f.st.GetMessage(f.id)
	require.NoError(err)
	require.Equal("Drafts|1", before.SourceMessageID)
	require.Contains(before.BodyText, f.draft.Body)
	changed := f.sent
	changed.Flags = []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged}
	f.final.Mailboxes = append(f.final.Mailboxes, scriptedRFC7162Mailbox{
		Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll},
		UIDValidity: 99, UIDNext: 2, HighestModSeq: 3,
		ChangedUIDs: []imapapi.UID{1}, Messages: []scriptedRFC7162Message{changed},
	})
	for range 2 {
		f.server.setSnapshot(f.final)
		client, err := f.sync(t)
		require.NoError(err)
		require.NoError(client.Close())
		after, err := f.st.GetMessage(f.id)
		require.NoError(err)
		assert.Contains(after.BodyText, f.sent.Body)
		assert.NotContains(after.BodyText, f.draft.Body)
		raw, err := f.st.GetMessageRaw(f.id)
		require.NoError(err)
		assert.Equal(rawSent, raw)
		_, total, err := f.st.SearchMessagesQuery(&search.Query{TextTerms: []string{f.draft.Body}}, 0, 10)
		require.NoError(err)
		assert.Zero(total, "the obsolete draft must leave the search index")
		f.final.Mailboxes[0].VanishedUIDs = nil
		f.final.Mailboxes[2].ChangedUIDs = nil
	}
}

func TestIMAPRelocationMalformedSentAdvancesFolderStates(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("persisted-membership=%t", legacy), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			draft := newScriptedRFC7162Message(1, "malformed-relocation@example.test", imapapi.FlagDraft)
			draft.Body = "validdraftword"
			sent := newScriptedRFC7162Message(1, draft.MessageID, imapapi.FlagSeen)
			sent.Raw = "From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: Invalid final\r\nMessage-ID: <" + draft.MessageID + ">\r\nMIME-Version: 1.0\r\nContent-Type: multipart mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: text/plain\r\n\r\nfinalword\r\n--outer--\r\n"
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				scriptedRFC7162Inbox(55, 1, 1),
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
				{Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 1, HighestModSeq: 1},
			}}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://malformed-relocation@example.test"
			first, source := requireScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(first.Close())
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+draft.MessageID+">")
			require.NoError(err)
			require.Positive(id)
			if legacy {
				seedLegacyDraftsCanonicalWithSentMembership(t, st, source, []byte(sent.Raw), "<"+draft.MessageID+">")
			}
			edited := baseline.clone()
			edited.Mailboxes[1].Messages = nil
			edited.Mailboxes[1].HighestModSeq = 3
			edited.Mailboxes[1].VanishedUIDs = []imapapi.UID{1}
			edited.Mailboxes[2].Messages = []scriptedRFC7162Message{sent}
			edited.Mailboxes[2].UIDNext = 2
			edited.Mailboxes[2].HighestModSeq = 2
			if !legacy {
				edited.Mailboxes[2].ChangedUIDs = []imapapi.UID{1}
			}
			for n := 1; n <= 3; n++ {
				inbox := newScriptedRFC7162Message(imapapi.UID(n), fmt.Sprintf("new-inbox-%d@example.test", n), imapapi.FlagSeen)
				inbox.Body = fmt.Sprintf("inboxword%d", n)
				edited.Mailboxes[0].Messages = append(edited.Mailboxes[0].Messages, inbox)
				edited.Mailboxes[0].UIDNext = uint32(n + 1)
				edited.Mailboxes[0].HighestModSeq = uint64(n + 1)
				edited.Mailboxes[0].ChangedUIDs = append(edited.Mailboxes[0].ChangedUIDs, imapapi.UID(n))
				server.setSnapshot(edited)
				summary, err := runScriptedRelocationSyncSummary(t, st, identifier, addr)
				require.NoError(err)
				require.NotNil(summary)
				assert.Zero(summary.Errors)
				assert.Equal(int64(1), summary.MessagesAdded)
				inboxID, err := st.GetMessageIDByRFC822ID(source.ID, "<"+inbox.MessageID+">")
				require.NoError(err)
				assert.Positive(inboxID)
				target, err := st.GetMessage(id)
				require.NoError(err)
				assert.Equal("Sent|1", target.SourceMessageID)
				assert.Contains(target.BodyText, "MIME parsing failed")
				assert.NotContains(target.BodyText, draft.Body)
				raw, err := st.GetMessageRaw(id)
				require.NoError(err)
				assert.Equal(sent.Raw, string(raw))
				afterStates, err := st.GetIMAPFolderStates(source.ID)
				require.NoError(err)
				require.Len(afterStates, 3)
				for _, state := range afterStates {
					if state.Mailbox == "INBOX" {
						assert.Equal(uint32(n+1), state.UIDNext)
						assert.Equal(uint64(n+1), state.HighestModSeq)
					}
				}
				edited.Mailboxes[1].VanishedUIDs = nil
				edited.Mailboxes[2].ChangedUIDs = nil
			}
		})
	}
}

func TestIMAPIdenticalSentKeepsPreferredLocation(t *testing.T) {
	for _, withSent := range []bool{false, true} {
		t.Run(fmt.Sprintf("sent=%v", withSent), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			msg := newScriptedRFC7162Message(1, "identical-outgoing@example.test", imapapi.FlagSeen)
			msg.Body = "finalsentword"
			baseline := scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "All Mail", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrAll}, UIDValidity: 99, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{msg}},
			}}
			if withSent {
				baseline.Mailboxes = append(baseline.Mailboxes, scriptedRFC7162Mailbox{
					Name: "Sent", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrSent}, UIDValidity: 88, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{msg},
				})
			}
			addr, server := startScriptedRFC7162Server(t, baseline)
			st := testutil.NewTestStore(t)
			const identifier = "imap://identical-outgoing@example.test"
			summary, err := runScriptedRelocationSyncSummary(t, st, identifier, addr)
			require.NoError(err)
			assert.Zero(summary.Errors)
			assert.Equal(int64(1), summary.MessagesAdded)
			assert.Zero(summary.MessagesUpdated, "identical content must not trigger relocation persistence")
			source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
			require.NoError(err)
			id, err := st.GetMessageIDByRFC822ID(source.ID, "<"+msg.MessageID+">")
			require.NoError(err)
			require.Positive(id)
			message, err := st.GetMessage(id)
			require.NoError(err)
			assert.Equal("All Mail|1", message.SourceMessageID)
			if !withSent {
				metadata, err := st.GetMessageMetadata(id)
				require.NoError(err)
				assert.False(metadata.Valid, "untrusted origin does not need a metadata write")
				return
			}
			for range 2 {
				client, err := runScriptedRelocationResync(t, st, identifier, addr)
				require.NoError(err)
				require.NoError(client.Close())
				message, err = st.GetMessage(id)
				require.NoError(err)
				assert.Equal("All Mail|1", message.SourceMessageID)
				assert.Contains(message.BodyText, msg.Body)
			}
			// The identical Sent copy must still establish content precedence:
			// losing both locations cannot let a stale draft replace the final bytes.
			draft := newScriptedRFC7162Message(1, msg.MessageID, imapapi.FlagDraft)
			draft.Body = "staledraftword"
			server.setSnapshot(scriptedRFC7162Snapshot{Capabilities: scriptedRFC7162Capabilities(), Mailboxes: []scriptedRFC7162Mailbox{
				{Name: "Drafts", Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrDrafts}, UIDValidity: 77, UIDNext: 2, HighestModSeq: 1, Messages: []scriptedRFC7162Message{draft}},
			}})
			client, _, err := runScriptedRFC7162Sync(t, st, identifier, addr)
			require.NoError(err)
			require.NoError(client.Close())
			message, err = st.GetMessage(id)
			require.NoError(err)
			assert.Equal("Drafts|1", message.SourceMessageID)
			assert.Contains(message.BodyText, msg.Body)
			assert.NotContains(message.BodyText, draft.Body)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(scriptedRFC7162RawMessage(msg), string(raw))
		})
	}
}
