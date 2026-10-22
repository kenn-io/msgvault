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
	f.PersistEmail(storetest.EmailSpec{Thread: "question", From: alex, To: []string{me}, Body: "Can you also send the report?", SentAt: ago(2 * day)})
	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 1)
	assert.Equal(loops.ReplyOwed, result.Loops[0].Kind)
	assert.Len(result.Loops[0].Questions, 2, "linked senders share one current-person turn")
	assert.Equal(home, result.Loops[0].Counterparty.ParticipantID)
	assert.Equal(person.ID, result.Loops[0].Counterparty.PersonID)
}

func TestMergedDecisionCoversQuestionAfterDateCorrection(t *testing.T) {
	for _, state := range []string{store.LoopStateDone, store.LoopStateSnoozed} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			survivor := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})
			anchor := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
			duplicate := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Any update?", SentAt: ago(3 * day)})

			var err error
			if state == store.LoopStateDone {
				_, err = loops.Done(t.Context(), f.Store, anchor, now)
			} else {
				_, err = loops.Snooze(t.Context(), f.Store, anchor, now.Add(5*day), now)
			}
			require.NoError(err)
			_, err = f.Store.MergeDuplicates(survivor, []int64{duplicate}, "loop-question-date-correction")
			require.NoError(err)

			sourceMessageID, err := f.Store.GetMessageSourceID(survivor)
			require.NoError(err)
			id, err := f.Store.UpsertMessage(&store.Message{
				SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: sourceMessageID,
				MessageType: store.MessageTypeEmail,
				SenderID:    sql.NullInt64{Int64: f.EnsureParticipant(alex, "", "example.test"), Valid: true},
				SentAt:      sql.NullTime{Time: ago(2 * day), Valid: true},
			})
			require.NoError(err)
			require.Equal(survivor, id)

			assert.Empty(list(t, f, loops.Options{}).Loops, "the decision still covers its merged question")
		})
	}
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
			assert.Equal(store.LoopStateSnoozed, decision.State)
			require.NotNil(decision.SnoozedUntil)
			assert.True(until.Equal(*decision.SnoozedUntil))
			later, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(2*day))
			require.NoError(err)
			require.Len(later.Loops, 1, "the imported question reopens at its own deadline")
			assert.Equal(older, later.Loops[0].Anchor.ID)
			marks, err := f.Store.LoopMarksContext(t.Context())
			require.NoError(err)
			assert.Equal(state, marks[question].State, "the earlier decision stays intact")
		})
	}
}

func TestDecisionsHideOnlyTheQuestionsYouSaw(t *testing.T) {
	for _, tt := range []struct {
		name          string
		first, second time.Duration
	}{
		{name: "older import changes the anchor", first: -3 * day, second: -5 * day},
		{name: "identical sent time", first: -3 * day, second: -3 * day},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the contract?", SentAt: now.Add(tt.first)})
			_, err := loops.Done(t.Context(), f.Store, question, now)
			require.NoError(err)
			require.Empty(list(t, f, loops.Options{}).Loops, "the question you marked stays hidden")
			imported := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "And the invoice?", SentAt: now.Add(tt.second)})
			result := list(t, f, loops.Options{})
			require.Len(result.Loops, 1, "an unseen question brings the loop back")
			assert.Equal(imported, result.Loops[0].Anchor.ID)
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
			followUp := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "And the invoice?", SentAt: ago(3 * day)})
			result := list(t, f, loops.Options{})
			require.Len(result.Loops, 1, "an unseen question brings the loop back")
			assert.Equal(followUp, result.Loops[0].Anchor.ID)
			assert.True(result.Loops[0].OwedSince.Equal(ago(3 * day)))
			mark(followUp)
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

func TestDistinctSnoozeDeadlinesSurviveSourceMerge(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			history, err := f.Store.GetOrCreateSource("beeper", "history@example.test")
			require.NoError(err)
			live, err := f.Store.GetOrCreateSource("beeper", "live@example.test")
			require.NoError(err)
			sender := f.EnsureParticipant(alex, "", "example.test")
			var questions []int64
			for i, sourceID := range []int64{history.ID, live.ID} {
				conversation, err := f.Store.EnsureConversationWithType(sourceID, "!questions:example.test", "direct_chat", "Questions")
				require.NoError(err)
				question, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
					Message: &store.Message{
						SourceID: sourceID, ConversationID: conversation, MessageType: "beeper", SourceMessageID: "question-" + strconv.Itoa(i),
						SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: ago(time.Duration(6-2*i) * day), Valid: true},
					},
					BodyText: sql.NullString{String: "Can you send the contract?", Valid: true},
				})
				require.NoError(err)
				questions = append(questions, question)
				_, err = loops.Snooze(t.Context(), f.Store, question, now.Add(time.Duration(1+2*i)*day), now)
				require.NoError(err)
			}
			from, into := history.ID, live.ID
			if reverse {
				from, into = into, from
			}
			_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into})
			require.NoError(err)
			firstRef, err := f.Store.MessageConversationContext(t.Context(), questions[0])
			require.NoError(err)
			secondRef, err := f.Store.MessageConversationContext(t.Context(), questions[1])
			require.NoError(err)
			require.Equal(firstRef.ConversationID, secondRef.ConversationID, "both questions now belong to the same chat")
			marks, err := f.Store.LoopMarksContext(t.Context())
			require.NoError(err)
			require.NotNil(marks[questions[1]].SnoozedUntil)
			assert.True(now.Add(3*day).Equal(*marks[questions[1]].SnoozedUntil), "the second question retains its longer snooze")
			result, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(2*day))
			require.NoError(err)
			require.Len(result.Loops, 1, "the first question reopens at its own deadline")
			assert.Equal(questions[0], result.Loops[0].Anchor.ID)
			require.Len(result.Loops[0].Questions, 1, "the second question is still snoozed")
			assert.Equal(questions[0], result.Loops[0].Questions[0].ID)
		})
	}
}

func TestRequesterDecisionsStayIndependent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you approve the contract?", SentAt: ago(8 * day)})
	second := f.PersistEmail(storetest.EmailSpec{From: "sam@example.test", To: []string{me}, Body: "Could you send the report?", SentAt: ago(5 * day)})
	result := list(t, f, loops.Options{})
	require.Len(result.Loops, 2)
	assert.Equal(first, result.Loops[0].Anchor.ID)
	assert.Equal(second, result.Loops[1].Anchor.ID)
	_, err := loops.Done(t.Context(), f.Store, first, now)
	require.NoError(err)
	result = list(t, f, loops.Options{})
	require.Len(result.Loops, 1)
	assert.Equal(second, result.Loops[0].Anchor.ID)
	older := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you check the older draft?", SentAt: ago(10 * day)})
	result = list(t, f, loops.Options{})
	require.Len(result.Loops, 2)
	assert.Equal(older, result.Loops[0].Anchor.ID)
	assert.Equal(second, result.Loops[1].Anchor.ID)
}

func TestAwaitedReplyOnBccSettles(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{From: me, To: []string{alex}, Body: "Can you send the contract?", SentAt: ago(5 * day)})
	reply := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Here is the contract.", SentAt: ago(3 * day)})
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET recipient_type = ? WHERE message_id = ?`), "bcc", reply)
	require.NoError(err)
	assert.Empty(t, list(t, f, loops.Options{}).Loops, "a stored Bcc reply from the awaited person settles the question")
}

func TestExpiredSnoozeOnlyRestoresItsQuestion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	alexQuestion := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you approve the contract?", SentAt: ago(25 * day)})
	samQuestion := f.PersistEmail(storetest.EmailSpec{From: "sam@example.test", To: []string{me}, Body: "Can you send the report?", SentAt: ago(24 * day)})
	_, err := loops.Snooze(t.Context(), f.Store, alexQuestion, now.Add(30*day), now)
	require.NoError(err)
	before, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(15*day))
	require.NoError(err)
	assert.Empty(before.Loops)
	_, err = loops.Done(t.Context(), f.Store, samQuestion, now.Add(15*day))
	require.ErrorIs(err, loops.ErrNotOpen)
	after, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(31*day))
	require.NoError(err)
	require.Len(after.Loops, 1)
	assert.Equal(alexQuestion, after.Loops[0].Anchor.ID)
	olderAlexQuestion := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you review the invoice?", SentAt: ago(26 * day)})
	quiet, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(31*day))
	require.NoError(err)
	require.Len(quiet.Loops, 1, "an expired snooze restores only the exact marked question")
	assert.Equal(alexQuestion, quiet.Loops[0].Anchor.ID)
	_, err = loops.Done(t.Context(), f.Store, olderAlexQuestion, now.Add(31*day))
	require.ErrorIs(err, loops.ErrNotOpen)
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "The revised schedule is ready for review.", SentAt: now.Add(30 * day)})
	active, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(31*day))
	require.NoError(err)
	require.Len(active.Loops, 1, "recent requester activity keeps all their unanswered questions")
	assert.Equal(olderAlexQuestion, active.Loops[0].Anchor.ID)
	assert.Len(active.Loops[0].Questions, 2)

	_, err = loops.Done(t.Context(), f.Store, olderAlexQuestion, now.Add(31*day))
	require.NoError(err)
}

func TestNeutralRequesterAcknowledgementKeepsOlderQuestionOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	question := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you approve the contract?", SentAt: ago(40 * day)})
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Thanks!", SentAt: ago(day)})

	result, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
	require.NoError(err)
	require.Len(result.Loops, 1, "a recent human acknowledgement keeps this requester's unanswered question eligible")
	assert.Equal(question, result.Loops[0].Anchor.ID)
	_, err = loops.Done(t.Context(), f.Store, question, now)
	assert.NoError(err, "an eligible old question can still be marked done")
}

func TestLoopActionLeavesQuestionInsideItsGraceUnmarked(t *testing.T) {
	for _, action := range []string{"done", "snooze"} {
		t.Run(action, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			old := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you approve the contract?", SentAt: ago(5 * day)})
			fresh := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the report?", SentAt: ago(time.Hour)})
			before := list(t, f, loops.Options{})
			require.Len(before.Loops, 1)
			assert.Equal(old, before.Loops[0].Anchor.ID)

			var err error
			if action == "done" {
				_, err = loops.Done(t.Context(), f.Store, old, now)
			} else {
				_, err = loops.Snooze(t.Context(), f.Store, old, now.Add(2*day), now)
			}
			require.NoError(err)
			assert.Empty(list(t, f, loops.Options{}).Loops, "the fresh question stays hidden inside its own grace period")
			marks, err := f.Store.LoopMarksContext(t.Context())
			require.NoError(err)
			assert.NotContains(marks, fresh, "the action covers questions whose own grace period has elapsed")

			after, err := loops.List(t.Context(), f.Store, loops.Options{}, now.Add(day))
			require.NoError(err)
			require.Len(after.Loops, 1, "the fresh question appears after its own grace, including while the older question is snoozed")
			assert.Equal(fresh, after.Loops[0].Anchor.ID)
		})
	}
}
