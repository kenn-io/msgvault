package loops_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const (
	me   = storetest.OwnerAddress
	alex = "alex@example.test"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

const day = 24 * time.Hour

func list(t *testing.T, f *storetest.Fixture, opts loops.Options) loops.Result {
	t.Helper()
	require := require.New(t)
	result, err := loops.List(t.Context(), f.Store, opts, now)
	require.NoError(err)
	return result
}

type originalEvidenceCountingStore struct {
	*store.Store

	reads map[int64]int
}

func (s *originalEvidenceCountingStore) LoopOriginalEvidenceContext(
	ctx context.Context, messageID int64,
) (mime.AutomationSignals, bool, bool, error) {
	s.reads[messageID]++
	return s.Store.LoopOriginalEvidenceContext(ctx, messageID)
}

func TestListReplyOwedAndAwaiting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: me, To: []string{alex}, Subject: "Hello", Body: "Hi", SentAt: ago(10 * day)})
	owed := f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: alex, To: []string{me}, Subject: "Contract draft", Body: "Can you send the contract?", SentAt: ago(3 * day)})
	waiting := f.PersistEmail(storetest.EmailSpec{Thread: "waiting", From: me, To: []string{"sam@example.test"}, Subject: "Lunch Friday?", Body: "Are you free?", SentAt: ago(5 * day)})

	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 2)
	assert.Equal(waiting, result.Loops[0].Anchor.ID, "oldest first")
	assert.Equal(loops.AwaitingReply, result.Loops[0].Kind)
	assert.Equal(owed, result.Loops[1].Anchor.ID)
	assert.Equal(loops.ReplyOwed, result.Loops[1].Kind)
	assert.Equal(store.ChannelEmail, result.Loops[1].Channel)
}

func TestListUsesStoredMIMEForAttachmentOnlyRepliesWithoutAttachmentRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{
		Thread: "mime-attachment-only", From: alex, To: []string{me},
		Body: "Can you send the contract?", SentAt: ago(5 * day),
	})
	reply := f.PersistEmail(storetest.EmailSpec{
		Thread: "mime-attachment-only", From: me, To: []string{alex},
		Raw: "MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=proof\r\n\r\n" +
			"--proof\r\n" +
			"Content-Type: application/pdf; name=contract.pdf\r\n" +
			"Content-Disposition: attachment; filename=contract.pdf\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			"cGRm\r\n--proof--\r\n",
		SentAt: ago(4 * day),
	})
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET has_attachments = TRUE, attachment_count = 1 WHERE id = ?`), reply)
	require.NoError(err)
	var attachmentRows int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), reply).Scan(&attachmentRows))
	require.Zero(attachmentRows, "the imported message has no exported attachment rows")

	counting := &originalEvidenceCountingStore{Store: f.Store, reads: map[int64]int{}}
	result, err := loops.List(t.Context(), counting, loops.Options{}, now)
	require.NoError(err)
	require.Empty(result.Loops, "the PDF reply settles the incoming question")
	assert.Equal(1, counting.reads[reply], "the attachment and automation checks share one MIME parse")
}

func TestListLimitDefersMIMEReadsForUnvisitedConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{
		Thread: "old-waiting", From: me, To: []string{alex}, Body: "Can you send the contract?", SentAt: ago(10 * day),
	})
	f.PersistEmail(storetest.EmailSpec{
		Thread: "next-waiting", From: me, To: []string{alex}, Body: "Can you review the proposal?", SentAt: ago(9 * day),
	})
	f.PersistEmail(storetest.EmailSpec{
		Thread: "newer-attachment-reply", From: alex, To: []string{me},
		Body: "Can you send the contract?", SentAt: ago(2 * day),
	})
	reply := f.PersistEmail(storetest.EmailSpec{
		Thread: "newer-attachment-reply", From: me, To: []string{alex},
		Raw: "MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=proof\r\n\r\n" +
			"--proof\r\n" +
			"Content-Type: application/pdf; name=contract.pdf\r\n" +
			"Content-Disposition: attachment; filename=contract.pdf\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			"cGRm\r\n--proof--\r\n",
		SentAt: ago(day),
	})
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET has_attachments = TRUE, attachment_count = 1 WHERE id = ?`), reply)
	require.NoError(err)

	counting := &originalEvidenceCountingStore{Store: f.Store, reads: map[int64]int{}}
	result, err := loops.List(t.Context(), counting, loops.Options{Limit: 1}, now)
	require.NoError(err)
	assert.Len(result.Loops, 1)
	assert.True(result.Truncated, "the second older loop determines that the result is truncated")
	assert.Zero(counting.reads[reply], "the later conversation's raw MIME is not read after the limit is satisfied")
}

func TestListFollowUpAcrossUnansweredForward(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(6 * day)})
	_, err := loops.Done(t.Context(), f.Store, first, ago(5*day))
	require.NoError(err)
	require.Empty(list(t, f, loops.Options{}).Loops, "the first question is done before the forward")
	f.PersistEmail(storetest.EmailSpec{From: me, To: []string{"sam@example.test"}, Body: "Can you take a look?", SentAt: ago(4 * day)})
	followUp := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})

	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 1)
	loop := result.Loops[0]
	assert.Equal(loops.ReplyOwed, loop.Kind)
	assert.Equal(alex, loop.Counterparty.Address)
	assert.Equal(first, loop.Anchor.ID)
	assert.True(ago(6*day).Equal(loop.OwedSince), "the oldest unanswered question starts the clock")
	assert.Equal(followUp, loop.LastAsk.ID)
	assert.True(ago(3*day).Equal(loop.LastAsk.OccurredAt), "the follow-up remains the newest question")
}

func TestListPersonFilterRetainsUnansweredQuestionAfterAnotherPersonIsAnswered(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{Thread: "two-people", From: alex, To: []string{me}, Body: "Can you review the contract?", SentAt: ago(6 * day)})
	f.PersistEmail(storetest.EmailSpec{Thread: "two-people", From: me, To: []string{alex}, Body: "I reviewed the contract.", SentAt: ago(5 * day)})
	samQuestion := f.PersistEmail(storetest.EmailSpec{Thread: "two-people", From: "sam@example.test", To: []string{me}, Body: "Can you send the budget?", SentAt: ago(4 * day)})

	alexParticipant := f.EnsureParticipant(alex, "", "example.test")
	alexPerson, _, err := f.Store.CreatePersonFromParticipant(alexParticipant)
	require.NoError(err)
	samParticipant := f.EnsureParticipant("sam@example.test", "", "example.test")
	samPerson, _, err := f.Store.CreatePersonFromParticipant(samParticipant)
	require.NoError(err)

	all := list(t, f, loops.Options{})
	require.Len(all.Loops, 1)
	assert.Equal(samQuestion, all.Loops[0].Anchor.ID)
	assert.Equal(loops.ReplyOwed, all.Loops[0].Kind)
	assert.Equal(samPerson.ID, all.Loops[0].Counterparty.PersonID)

	samOnly := list(t, f, loops.Options{PersonID: samPerson.ID})
	require.Len(samOnly.Loops, 1)
	assert.Equal(samQuestion, samOnly.Loops[0].Anchor.ID)
	assert.Empty(list(t, f, loops.Options{PersonID: alexPerson.ID}).Loops)
}

func TestListResolvesMissingSenderFromUnambiguousFromSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	question := f.PersistEmail(storetest.EmailSpec{
		Thread: "missing-sender-question", From: alex, To: []string{me},
		Body: "Can you send the contract?", SentAt: ago(2 * day),
	})
	f.PersistEmail(storetest.EmailSpec{
		Thread: "missing-sender-reply", From: me, To: []string{alex},
		Body: "Can you approve the contract?", SentAt: ago(4 * day),
	})
	reply := f.PersistEmail(storetest.EmailSpec{
		Thread: "missing-sender-reply", From: alex, To: []string{me},
		Body: "Approved.", SentAt: ago(3 * day),
	})
	for _, messageID := range []int64{question, reply} {
		_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id = NULL WHERE id = ?`), messageID)
		require.NoError(err)
	}

	alexParticipant := f.EnsureParticipant(alex, "", "example.test")
	alexPerson, _, err := f.Store.CreatePersonFromParticipant(alexParticipant)
	require.NoError(err)

	all := list(t, f, loops.Options{})
	require.Len(all.Loops, 1, "the reply with a missing sender must settle the request, while the incoming question remains open")
	assert.Equal(question, all.Loops[0].Anchor.ID)
	assert.Equal(loops.ReplyOwed, all.Loops[0].Kind)
	assert.Equal(alexPerson.ID, all.Loops[0].Counterparty.PersonID)

	byPerson := list(t, f, loops.Options{PersonID: alexPerson.ID})
	require.Len(byPerson.Loops, 1)
	assert.Equal(question, byPerson.Loops[0].Anchor.ID)
}

func TestListKeepsUnansweredRequesterAcrossDifferentCounterpartyBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	const thread = "cross-counterparty-boundary"
	first := f.PersistEmail(storetest.EmailSpec{Thread: thread, From: alex, To: []string{me}, Body: "Can you approve the contract?", SentAt: ago(8 * day)})
	f.PersistEmail(storetest.EmailSpec{Thread: thread, From: me, To: []string{"sam@example.test"}, Body: "Forwarding Alex's request.", SentAt: ago(6 * day)})
	f.PersistEmail(storetest.EmailSpec{Thread: thread, From: "sam@example.test", To: []string{me}, Body: "Could you send the latest version?", SentAt: ago(5 * day)})
	followUp := f.PersistEmail(storetest.EmailSpec{Thread: thread, From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})

	alexParticipant := f.EnsureParticipant(alex, "", "example.test")
	alexPerson, _, err := f.Store.CreatePersonFromParticipant(alexParticipant)
	require.NoError(err)
	samParticipant := f.EnsureParticipant("sam@example.test", "", "example.test")
	samPerson, _, err := f.Store.CreatePersonFromParticipant(samParticipant)
	require.NoError(err)

	all := list(t, f, loops.Options{})
	require.Len(all.Loops, 1)
	assert.Equal(first, all.Loops[0].Anchor.ID)
	assert.Equal(followUp, all.Loops[0].LastAsk.ID)
	assert.Equal(alexPerson.ID, all.Loops[0].Counterparty.PersonID)

	alexOnly := list(t, f, loops.Options{PersonID: alexPerson.ID})
	require.Len(alexOnly.Loops, 1)
	assert.Equal(first, alexOnly.Loops[0].Anchor.ID)
	assert.Empty(list(t, f, loops.Options{PersonID: samPerson.ID}).Loops)
}

func TestListReplyFromLinkedAddressSettles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	work := f.EnsureParticipant(alex, "", "example.test")
	home := f.EnsureParticipant("alex@home.example.test", "", "home.example.test")
	question := f.PersistEmail(storetest.EmailSpec{From: me, To: []string{alex}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
	f.PersistEmail(storetest.EmailSpec{From: "alex@home.example.test", To: []string{me}, Body: "Here is the contract.", SentAt: ago(4 * day)})

	unlinked := list(t, f, loops.Options{})
	require.Len(unlinked.Loops, 1)
	assert.Equal(question, unlinked.Loops[0].Anchor.ID)
	assert.Zero(unlinked.Loops[0].Counterparty.PersonID, "an unlinked address has no person")

	_, err := f.Store.LinkParticipants(work, home)
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(work)
	require.NoError(err)
	assert.Empty(list(t, f, loops.Options{}).Loops)

	f.PersistEmail(storetest.EmailSpec{Thread: "question", From: "alex@home.example.test", To: []string{me}, Body: "Can you send the contract?", SentAt: ago(3 * day)})
	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 1)
	assert.Equal(loops.ReplyOwed, result.Loops[0].Kind)
	assert.Equal(home, result.Loops[0].Counterparty.ParticipantID)
	assert.Equal(person.ID, result.Loops[0].Counterparty.PersonID)
	assert.Len(list(t, f, loops.Options{PersonID: person.ID}).Loops, 1)
	assert.Empty(list(t, f, loops.Options{PersonID: person.ID + 100}).Loops)
}

func TestListDecisionSurvivesQuestionDateCorrection(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Subject: "Contract draft", Body: "Can you send the contract?", SentAt: ago(5 * day)})
	_, err := loops.Done(t.Context(), f.Store, question, now)
	require.NoError(err)

	sourceMessageID, err := f.Store.GetMessageSourceID(question)
	require.NoError(err)
	id, err := f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID,
		SourceMessageID: sourceMessageID, MessageType: store.MessageTypeEmail,
		SenderID: sql.NullInt64{Int64: f.EnsureParticipant(alex, "", "example.test"), Valid: true},
		Subject:  sql.NullString{String: "Contract draft", Valid: true},
		SentAt:   sql.NullTime{Time: ago(3 * day), Valid: true}})
	require.NoError(err)
	require.Equal(question, id)
	assert.Empty(t, list(t, f, loops.Options{}).Loops, "the same question stays done after its date changes")
}

func TestMergedDecisionCoversQuestionAfterDateCorrection(t *testing.T) {
	for _, state := range []string{store.LoopStateDone, store.LoopStateSnoozed} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			survivor := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})
			anchor := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
			cutoff := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})

			var err error
			if state == store.LoopStateDone {
				_, err = loops.Done(t.Context(), f.Store, anchor, now)
			} else {
				_, err = loops.Snooze(t.Context(), f.Store, anchor, now.Add(5*day), now)
			}
			require.NoError(err)
			_, err = f.Store.MergeDuplicates(survivor, []int64{cutoff}, "loop-cutoff-date-correction")
			require.NoError(err)

			sourceMessageID, err := f.Store.GetMessageSourceID(survivor)
			require.NoError(err)
			id, err := f.Store.UpsertMessage(&store.Message{
				SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: sourceMessageID,
				MessageType: store.MessageTypeEmail,
				SenderID:    sql.NullInt64{Int64: f.EnsureParticipant(alex, "", "example.test"), Valid: true},
				SentAt:      sql.NullTime{Time: ago(time.Hour), Valid: true},
			})
			require.NoError(err)
			require.Equal(survivor, id)

			assert.Empty(list(t, f, loops.Options{}).Loops, "the decision still covers its merged question")
		})
	}
}

func TestDuplicateStateFollowsLaterSurvivorAndLeavesDistinctQuestionOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.PersistEmail(storetest.EmailSpec{
		Thread: "remapped-cutoff", From: alex, To: []string{me},
		Body: "Can you send the contract?", SentAt: ago(5 * day),
	})
	cutoff := f.PersistEmail(storetest.EmailSpec{
		Thread: "remapped-cutoff", From: alex, To: []string{me},
		Body: "Can you send the budget?", SentAt: ago(3 * day),
	})
	survivor := f.PersistEmail(storetest.EmailSpec{
		Thread: "remapped-cutoff", From: alex, To: []string{me},
		Body: "Can you send the budget?", SentAt: ago(3 * day),
	})
	_, err := f.Store.MarkLoopDoneContext(t.Context(), first, store.LoopMessage{
		ID: cutoff, OccurredAt: ago(3 * day),
	})
	require.NoError(err)
	_, err = f.Store.MergeDuplicates(survivor, []int64{cutoff}, "loop-cutoff-remap")
	require.NoError(err)

	marks, err := f.Store.LoopMarksContext(t.Context(), now)
	require.NoError(err)
	require.Len(marks, 1)
	conversation, err := f.Store.MessageConversationContext(t.Context(), first)
	require.NoError(err)
	mark, ok := marks[conversation.ConversationID]
	require.True(ok)
	assert.Equal(survivor, mark.MessageID, "the exact identity follows the surviving copy")
	assert.Equal(cutoff, mark.OrderID, "the original ordering boundary stays in place")

	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sent_at = ? WHERE id = ?`), ago(2*day), survivor)
	require.NoError(err)
	assert.Empty(list(t, f, loops.Options{}).Loops, "a confirmed duplicate keeps the question done even with a later token")
	newQuestion := f.PersistEmail(storetest.EmailSpec{Thread: "remapped-cutoff", From: alex, To: []string{me}, Body: "Can you send the invoice?", SentAt: ago(3 * day)})
	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 1, "a distinct question at the same instant stays open")
	assert.Equal(survivor, result.Loops[0].LastAsk.ID, "the corrected newest question stays covered while the distinct older question keeps the loop visible")
	assert.False(store.LoopCutoffCovers(mark, store.LoopMessage{ID: newQuestion, OrderID: newQuestion, OccurredAt: ago(3 * day)}))
}

func TestSnoozeAfterOlderImportReportsConversationDecision(t *testing.T) {
	for _, state := range []string{store.LoopStateDone, store.LoopStateSnoozed} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(3 * day)})
			var err error
			if state == store.LoopStateDone {
				_, err = loops.Done(t.Context(), f.Store, question, now)
			} else {
				_, err = loops.Snooze(t.Context(), f.Store, question, now.Add(5*day), now)
			}
			require.NoError(err)
			older := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send it?", SentAt: ago(5 * day)})
			until := now.Add(day)
			decision, err := loops.Snooze(t.Context(), f.Store, older, until, now)
			require.NoError(err)
			if state == store.LoopStateDone {
				assert.Equal(store.LoopStateDone, decision.State)
				assert.Nil(decision.SnoozedUntil)
			} else {
				assert.Equal(store.LoopStateSnoozed, decision.State)
				require.NotNil(decision.SnoozedUntil)
				assert.True(until.Equal(*decision.SnoozedUntil))
			}
			later, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(2*day))
			require.NoError(err)
			if state == store.LoopStateDone {
				assert.Empty(later.Loops)
			} else {
				assert.Len(later.Loops, 1, "the replacement snooze expires at its new deadline")
			}
		})
	}
}

func TestDecisionsHideOnlyTheQuestionsYouSaw(t *testing.T) {
	for _, tt := range []struct {
		name          string
		first, second time.Duration
		reopens       bool
	}{
		{name: "older import changes the anchor", first: -3 * day, second: -5 * day},
		{name: "identical sent time", first: -3 * day, second: -3 * day, reopens: true},
		{name: "milliseconds", first: -5*day + 100*time.Millisecond, second: -5*day + 600*time.Millisecond, reopens: true},
		{name: "below a millisecond", first: -5*day + 100100*time.Microsecond, second: -5*day + 100900*time.Microsecond, reopens: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: now.Add(tt.first)})
			_, err := loops.Done(t.Context(), f.Store, question, now)
			require.NoError(err)
			require.Empty(list(t, f, loops.Options{}).Loops, "the question you marked stays hidden")
			f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "And the invoice?", SentAt: now.Add(tt.second)})
			result := list(t, f, loops.Options{})
			if !tt.reopens {
				assert.Empty(result.Loops, "the done mark still covers the turn after importing an older question")
				return
			}
			require.Len(result.Loops, 1, "an unseen question brings the loop back")
			assert.Equal(question, result.Loops[0].Anchor.ID)
		})
	}
	for _, state := range []string{store.LoopStateDone, store.LoopStateSnoozed} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			mark := func(anchor int64) {
				var err error
				if state == store.LoopStateSnoozed {
					_, err = loops.Snooze(t.Context(), f.Store, anchor, now.Add(2*day), now)
				} else {
					_, err = loops.Done(t.Context(), f.Store, anchor, now)
				}
				require.NoError(err)
			}
			f.PersistEmail(storetest.EmailSpec{From: me, To: []string{alex}, Body: "Hi", SentAt: ago(20 * day)})
			question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
			mark(question)
			require.Empty(list(t, f, loops.Options{}).Loops, "the question you marked stays hidden")
			if state == store.LoopStateDone {
				later, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(20*day))
				require.NoError(err)
				assert.Empty(later.Loops, "a done loop stays done")
			}
			f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Just following up.", SentAt: ago(4 * day)})
			assert.Empty(list(t, f, loops.Options{}).Loops, "a follow-up that asks nothing leaves it hidden")
			f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "And the invoice?", SentAt: ago(3 * day)})
			result := list(t, f, loops.Options{})
			require.Len(result.Loops, 1, "an unseen question brings the loop back")
			assert.Equal(question, result.Loops[0].Anchor.ID)
			assert.True(result.Loops[0].OwedSince.Equal(ago(5*day)), "dated by the original question, got %s", result.Loops[0].OwedSince)
			mark(question)
			assert.Empty(list(t, f, loops.Options{}).Loops, "marking it again covers the newer question")
			f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you also send the deck?", SentAt: ago(day)})
			assert.Len(list(t, f, loops.Options{}).Loops, 1, "the next question brings it back again")
		})
	}
}

func TestDoneAcceptsWhatListShows(t *testing.T) {
	for _, tt := range []struct {
		name    string
		age     time.Duration
		advance time.Duration
		missing bool
		wantErr bool
	}{
		{name: "missing message", missing: true, wantErr: true},
		{name: "inside the grace period", age: 2 * time.Hour, wantErr: true},
		{name: "silent for 31 days", age: 31 * day, wantErr: true},
		{name: "listed", age: 3 * day},
		{name: "grace period passed", age: 2 * time.Hour, advance: 23 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			anchor := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(tt.age)})
			if tt.missing {
				_, err := f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id = ?`), anchor)
				require.NoError(err)
			}
			listed, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(tt.advance))
			require.NoError(err)
			if tt.wantErr {
				assert.Empty(listed.Loops)
			} else {
				require.Len(listed.Loops, 1)
				assert.Equal(anchor, listed.Loops[0].Anchor.ID)
			}
			_, err = loops.Done(t.Context(), f.Store, anchor, now.Add(tt.advance))
			if tt.wantErr {
				require.ErrorIs(err, loops.ErrNotOpen)
			} else {
				require.NoError(err)
			}
			if tt.wantErr {
				_, err = loops.Snooze(t.Context(), f.Store, anchor, now.Add(day), now)
				assert.ErrorIs(err, loops.ErrNotOpen)
			}
		})
	}
}

func TestListCountsUnreadableOriginals(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	asked := f.PersistEmail(storetest.EmailSpec{From: me, To: []string{alex}, Body: "Can you review it?", SentAt: ago(5 * day)})
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "[MIME parsing failed: expected slash after first token]", SentAt: ago(4 * day),
		Raw: "From: " + alex + "\r\nTo: " + me + "\r\nSubject: Re: Review\r\nMIME-Version: 1.0\r\nContent-Type: multipart mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: text/plain\r\n\r\nLooks good\r\n--outer--\r\n"})

	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 1)
	assert.Equal(asked, result.Loops[0].Anchor.ID)
	assert.Equal(1, result.SkippedUnparseable, "the list says one original could not be read")
}

// countingStore counts the originals List parses.
type countingStore struct {
	*store.Store

	originals int
	bodies    int
}

func (c *countingStore) GetMessageBodyTextContext(ctx context.Context, messageID int64) (string, error) {
	c.bodies++
	return c.Store.GetMessageBodyTextContext(ctx, messageID)
}

func (c *countingStore) LoopOriginalEvidenceContext(
	ctx context.Context, messageID int64,
) (mime.AutomationSignals, bool, bool, error) {
	c.originals++
	return c.Store.LoopOriginalEvidenceContext(ctx, messageID)
}

func TestListStopsReadingOnceTheOldestLoopsAreKnown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	var ids []int64
	for i := range 60 {
		ids = append(ids, f.PersistEmail(storetest.EmailSpec{Thread: "q" + strconv.Itoa(i), From: alex, To: []string{me}, Body: "Can you send the contract?",
			SentAt: ago(2*day + time.Duration(i)*7*time.Hour)}))
	}
	// A thread that started long ago and was nudged lately lists with its old anchor.
	old := f.PersistEmail(storetest.EmailSpec{Thread: "old", From: alex, To: []string{me}, Body: "Can you send the deck?", SentAt: ago(45 * day)})
	f.PersistEmail(storetest.EmailSpec{Thread: "old", From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})
	_, err := loops.Done(t.Context(), f.Store, ids[len(ids)-1], now)
	require.NoError(err)

	all := list(t, f, loops.Options{Limit: 1000})
	require.Len(all.Loops, 60)
	assert.Equal(old, all.Loops[0].Anchor.ID, "the old question lists first")
	assert.False(all.Truncated)

	for _, limit := range []int{1, 5, 59, 60} {
		counted := &countingStore{Store: f.Store}
		result, err := loops.List(t.Context(), counted, loops.Options{Limit: limit}, now)
		require.NoError(err)
		require.Len(result.Loops, limit)
		for i, l := range result.Loops {
			assert.Equal(all.Loops[i].Anchor.ID, l.Anchor.ID, "limit %d, loop %d", limit, i)
		}
		assert.Equal(limit < 60, result.Truncated, "limit %d", limit)
		if limit <= 5 {
			assert.Less(counted.originals, 25, "limit %d reads %d originals, not all 61 threads", limit, counted.originals)
		}
	}
}

func TestListSkipsSettledPeopleAcrossPagedEmailHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	for i := range 14 {
		inboundAt := ago(time.Duration(48-2*i) * day)
		outboundAt := ago(time.Duration(47-2*i) * day)
		f.PersistEmail(storetest.EmailSpec{
			Thread: "paged-settled-person", From: alex, To: []string{me},
			Body: "Can you review this draft?", SentAt: inboundAt, FileName: "draft.pdf",
		})
		f.PersistEmail(storetest.EmailSpec{
			Thread: "paged-settled-person", From: me, To: []string{alex},
			Body: "I reviewed it; see the attached copy.", SentAt: outboundAt, FileName: "reviewed.pdf",
		})
	}
	latest := f.PersistEmail(storetest.EmailSpec{
		Thread: "paged-settled-person", From: me, To: []string{"bob@example.test"},
		Body: "Can you send your estimate?", SentAt: ago(4 * day),
	})

	counted := &countingStore{Store: f.Store}
	result, err := loops.List(t.Context(), counted, loops.Options{Limit: 100}, now)
	require.NoError(err)
	require.Len(result.Loops, 1)
	assert.Equal(latest, result.Loops[0].Anchor.ID)
	assert.Equal(loops.AwaitingReply, result.Loops[0].Kind)
	assert.LessOrEqual(counted.originals, 4, "verified completed exchanges should not parse older replies")
	assert.LessOrEqual(counted.bodies, 3, "settled messages on older pages should not load their bodies")
}

func TestSnoozeReplacesDoneOnceAFollowUpReopens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{From: me, To: []string{alex}, Body: "Hi", SentAt: ago(20 * day)})
	question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
	_, err := loops.Done(t.Context(), f.Store, question, now)
	require.NoError(err)
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "And the invoice?", SentAt: ago(2 * day)})
	until := now.Add(day)
	decision, err := loops.Snooze(t.Context(), f.Store, question, until, now)
	require.NoError(err)
	require.NotNil(decision.SnoozedUntil)
	assert.Equal(store.LoopStateSnoozed, decision.State)
	assert.True(until.Equal(*decision.SnoozedUntil))
	var state string
	var storedUntil, through sql.NullTime
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT state, snoozed_until, through_at FROM loop_conversation_decisions
		WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
	`), question).Scan(&state, &storedUntil, &through))
	assert.Equal(store.LoopStateSnoozed, state)
	require.True(storedUntil.Valid)
	require.True(through.Valid)
	assert.True(until.Equal(storedUntil.Time))
	assert.True(ago(2 * day).Equal(through.Time))
}

func TestKnownOpenQuestionSurvivesCoveredDateReordering(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(6 * day)})
	_, err := loops.Done(t.Context(), f.Store, first, now)
	require.NoError(err)
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the budget?", SentAt: ago(4 * day)})
	// Snapshot the open follow-up during a duplicate transfer before dates change.
	copyID := f.PersistEmail(storetest.EmailSpec{Thread: "copy", From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(6 * day)})
	_, err = f.Store.MergeDuplicates(first, []int64{copyID}, "snapshot-known-questions")
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sent_at = ? WHERE id = ?`), ago(2*day), first)
	require.NoError(err)
	require.Len(list(t, f, loops.Options{}).Loops, 1, "the distinct unanswered follow-up stays open")
	decision, err := loops.Snooze(t.Context(), f.Store, list(t, f, loops.Options{}).Loops[0].Anchor.ID, now.Add(day), now)
	require.NoError(err)
	assert.Equal(store.LoopStateSnoozed, decision.State, "a done newest question cannot override an open older question")
}

func TestDistinctSnoozeDeadlinesSurviveSourceMerge(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			first := f.PersistEmail(storetest.EmailSpec{Thread: "merge-deadlines", From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(6 * day)})
			second := f.PersistEmail(storetest.EmailSpec{Thread: "merge-deadlines", From: alex, To: []string{me}, Body: "Can you send the budget?", SentAt: ago(4 * day)})
			other, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
			require.NoError(err)
			conv, err := f.Store.EnsureConversation(other.ID, "merge-deadlines", "email_thread")
			require.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_id = ?, conversation_id = ? WHERE id = ?`), other.ID, conv, second)
			require.NoError(err)
			_, err = loops.Snooze(t.Context(), f.Store, first, now.Add(day), now)
			require.NoError(err)
			_, err = loops.Snooze(t.Context(), f.Store, second, now.Add(3*day), now)
			require.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE sources SET source_type = 'beeper'`)
			require.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE conversations SET conversation_type = 'direct_chat', source_conversation_id = '!loop:example.test' WHERE source_conversation_id = 'merge-deadlines'`)
			require.NoError(err)
			from, into := other.ID, f.Source.ID
			if reverse {
				from, into = into, from
			}
			_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into})
			require.NoError(err)
			firstRef, err := f.Store.MessageConversationContext(t.Context(), first)
			require.NoError(err)
			secondRef, err := f.Store.MessageConversationContext(t.Context(), second)
			require.NoError(err)
			require.Equal(firstRef.ConversationID, secondRef.ConversationID, "both questions now belong to the same chat")
			marks, err := f.Store.LoopMarksContext(t.Context(), now.Add(2*day))
			require.NoError(err)
			ref, err := f.Store.MessageConversationContext(t.Context(), second)
			require.NoError(err)
			assert.True(store.LoopCutoffCovers(marks[ref.ConversationID], store.LoopMessage{ID: second, OrderID: second, OccurredAt: ago(4 * day)}), "the second question retains its longer snooze")
			result, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(2*day))
			require.NoError(err)
			assert.Len(result.Loops, 1, "the first question reopens at its own deadline")
		})
	}
}

func TestDuplicateTransferKeepsFallbackForLaterHistoricalImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	marked := f.PersistEmail(storetest.EmailSpec{Thread: "marked", From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
	_, err := loops.Done(t.Context(), f.Store, marked, now)
	require.NoError(err)
	survivor := f.PersistEmail(storetest.EmailSpec{Thread: "survivor", From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
	_, err = f.Store.MergeDuplicates(survivor, []int64{marked}, "late-import-cutoff")
	require.NoError(err)
	f.PersistEmail(storetest.EmailSpec{Thread: "survivor", From: alex, To: []string{me}, Body: "Can you send the earlier draft?", SentAt: ago(7 * day)})
	assert.Empty(list(t, f, loops.Options{}).Loops, "the copied fallback still covers a late historical question")
}
