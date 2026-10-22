package store_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const (
	loopOwner = storetest.OwnerAddress
	loopAlex  = "alex@example.test"
	loopDay   = 24 * time.Hour
)

var loopNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func loopAgo(d time.Duration) time.Time { return loopNow.Add(-d) }

func listLoops(t *testing.T, f *storetest.Fixture, at time.Time) []loops.Loop {
	t.Helper()
	result, err := loops.List(t.Context(), f.Store, loops.Options{}, at)
	require.NoError(t, err)
	return result.Loops
}

type loopBodyCountingStore struct {
	*store.Store

	bodyReads []int64
}

type loopChatMessage struct {
	fromOwner bool
	body      string
}

func persistLoopDirectChat(t *testing.T, f *storetest.Fixture, sourceConversation string, messages []loopChatMessage) []int64 {
	t.Helper()
	require := require.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, loopOwner, "manual"))
	owner := f.EnsureParticipant(loopOwner, "", "example.com")
	friend := f.EnsureParticipant(loopAlex, "", "example.test")
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, sourceConversation, "direct_chat", "Long chat")
	require.NoError(err)
	require.NoError(f.Store.EnsureConversationParticipant(conversation, owner, "member"))
	require.NoError(f.Store.EnsureConversationParticipant(conversation, friend, "member"))

	ids := make([]int64, 0, len(messages))
	for i, message := range messages {
		sender := friend
		if message.fromOwner {
			sender = owner
		}
		id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{
				SourceID: f.Source.ID, ConversationID: conversation, MessageType: "whatsapp",
				SourceMessageID: sourceConversation + "-" + strconv.Itoa(i),
				SenderID:        sql.NullInt64{Int64: sender, Valid: true},
				SentAt:          sql.NullTime{Time: loopAgo(time.Duration(len(messages)-i) * 4 * loopDay), Valid: true},
			},
			BodyText: sql.NullString{String: message.body, Valid: true},
		})
		require.NoError(err)
		ids = append(ids, id)
	}
	return ids
}

func (s *loopBodyCountingStore) GetMessageBodyTextContext(ctx context.Context, messageID int64) (string, error) {
	s.bodyReads = append(s.bodyReads, messageID)
	return s.Store.GetMessageBodyTextContext(ctx, messageID)
}

func conversationOf(t *testing.T, f *storetest.Fixture, messageID int64) int64 {
	t.Helper()
	ref, err := f.Store.MessageConversationContext(t.Context(), messageID)
	require.NoError(t, err)
	return ref.ConversationID
}

func TestLoopConversationWalksNewestFirstAcrossPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	gone := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Old news.", SentAt: loopAgo(10 * loopDay)})
	in := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Subject: "Contract", Body: "Can you send it?", SentAt: loopAgo(72 * time.Hour)})
	out := f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex}, Body: "Sure.", FileName: "deck.pdf", SentAt: loopAgo(24 * time.Hour)})
	tie := f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex}, Body: "And this.", ListID: "list.example.test", SentAt: loopAgo(24 * time.Hour)})
	between := f.PersistEmail(storetest.EmailSpec{From: "sam@example.test", To: []string{loopAlex}, Body: "Fyi", SentAt: loopAgo(2 * time.Hour)})
	more := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Thanks", SentAt: loopAgo(time.Hour)})
	deleted := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Gone?", SentAt: loopAgo(30 * time.Minute)})
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = ? WHERE id IN (?, ?)`), loopNow, deleted, gone)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at = ? WHERE id IN (?, ?)`), loopNow, in, out)
	require.NoError(err)
	draft := f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex}, Body: "Draft?", SentAt: loopAgo(10 * time.Minute)})
	labels := f.EnsureLabels(map[string]string{"DRAFT": "Draft"}, "system")
	require.NoError(f.Store.LinkMessageLabel(draft, labels["DRAFT"]))

	var got []store.LoopMessage
	var first, seen int
	for conv, err := range f.Store.LoopConversationsContext(t.Context(), store.LoopQuery{ConversationID: conversationOf(t, f, in)}) {
		require.NoError(err)
		seen++
		assert.True(conv.FirstAt.Equal(loopAgo(3*loopDay)), "FirstAt %s skips the deleted message", conv.FirstAt)
		first = len(conv.Messages)
		for m, err := range conv.Walk() {
			require.NoError(err)
			got = append(got, m)
		}
	}
	require.Equal(1, seen)
	assert.Equal(4, first, "the conversation comes with its newest four")
	require.Len(got, 5, "locally deleted messages and a draft stay out; upstream-deleted questions and answers remain")
	assert.Equal([]int64{more, between, tie, out, in}, []int64{got[0].ID, got[1].ID, got[2].ID, got[3].ID, got[4].ID}, "newest first across the page boundary, ties by ID")
	assert.Equal(store.DirectionObserved, got[1].Direction, "mail between two other people")
	assert.Equal(store.DirectionOutbound, got[3].Direction)
	assert.Equal(store.DirectionInbound, got[4].Direction)
	assert.Equal(loopAlex, got[4].Sender.Address)
	assert.True(got[4].Recipients[0].IsOwner)
	assert.Equal("Contract", got[4].Subject)
	assert.True(got[3].HasFileAttachment)
	assert.False(got[4].HasFileAttachment)
	assert.Equal("list.example.test", got[2].ListID)
}

func TestLoopAutomationSignalsReadStoredRaw(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	auto := f.PersistEmail(storetest.EmailSpec{Thread: "a", From: loopAlex, To: []string{loopOwner}, Body: "Back Monday", SentAt: loopNow,
		Raw: "From: " + loopAlex + "\r\nTo: " + loopOwner + "\r\nAuto-Submitted: auto-replied\r\nSubject: Out of office\r\n\r\nBack Monday\r\n"})
	broken := f.PersistEmail(storetest.EmailSpec{Thread: "b", From: loopAlex, To: []string{loopOwner}, Body: "Back Monday", SentAt: loopNow,
		Raw: malformedAutoReply})
	quotedPlaceholder := f.PersistEmail(storetest.EmailSpec{Thread: "placeholder", From: loopAlex, To: []string{loopOwner}, SentAt: loopNow,
		Body: "[MIME parsing failed: expected slash after first token] is what the archive showed me. Can you send the contract?"})
	human := f.PersistEmail(storetest.EmailSpec{Thread: "c", From: loopAlex, To: []string{loopOwner}, Body: "Hi", SentAt: loopNow})
	noRaw := f.PersistEmail(storetest.EmailSpec{Thread: "d", From: loopAlex, To: []string{loopOwner}, Body: "Hi", SentAt: loopNow, NoRaw: true})

	corrupt := f.PersistEmail(storetest.EmailSpec{Thread: "e", From: loopAlex, To: []string{loopOwner}, Body: "Looks good to me.", SentAt: loopNow})
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`), []byte("invalid zlib data"), corrupt)
	require.NoError(err)

	for _, tt := range []struct {
		name       string
		id         int64
		unreadable bool
		auto       bool
	}{
		{"auto-reply", auto, false, true},
		{"original that fails to parse", broken, true, false},
		{"human mail", human, false, false},
		{"human mail quoting the parse failure placeholder", quotedPlaceholder, false, false},
		{"no stored original", noRaw, false, false},
		{"invalid compressed original", corrupt, true, false},
	} {
		signals, _, unreadable, err := f.Store.LoopOriginalEvidenceContext(t.Context(), tt.id)
		require.NoError(err, tt.name)
		assert.Equal(tt.unreadable, unreadable, tt.name)
		assert.Equal(tt.auto, signals.AutoSubmitted, tt.name)
	}
}

// malformedAutoReply fails MIME parsing and names an automatic reply in a header.
const malformedAutoReply = "From: " + loopAlex + "\r\nTo: " + loopOwner + "\r\nAuto-Submitted: auto-replied\r\n" +
	"Subject: Out of office\r\nMIME-Version: 1.0\r\nContent-Type: multipart mixed; boundary=outer\r\n\r\n" +
	"--outer\r\nContent-Type: text/plain\r\n\r\nBack Monday\r\n--outer--\r\n"

func TestLoopIgnoresAddressesLinkedToTheOwner(t *testing.T) {
	const alias = "me@old.example.test"
	for _, account := range []bool{false, true} {
		name := "linked owner addresses"
		if account {
			name = "account identities"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			if account {
				other, err := f.Store.GetOrCreateSource("gmail", alias)
				require.NoError(err)
				require.NoError(f.Store.AddAccountIdentity(other.ID, alias, "manual"))
			} else {
				owner := f.EnsureParticipant(loopOwner, "", "example.com")
				linked := f.EnsureParticipant(alias, "", "old.example.test")
				_, err := f.Store.LinkParticipants(owner, linked)
				require.NoError(err)
				_, _, err = f.Store.CreatePersonFromParticipant(owner)
				require.NoError(err)
			}
			toAlias := f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{alias}, Body: "Can you review it?", SentAt: loopAgo(5 * loopDay)})
			fromAlias := f.PersistEmail(storetest.EmailSpec{From: alias, To: []string{loopOwner}, Body: "Can you send it?", SentAt: loopAgo(4 * loopDay)})
			question := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{alias}, Body: "Can you send the contract?", SentAt: loopAgo(3 * loopDay)})
			reply := f.PersistEmail(storetest.EmailSpec{From: alias, To: []string{loopAlex, loopOwner}, Body: "Sent, see attached.", SentAt: loopAgo(loopDay)})
			messages := map[int64]store.LoopMessage{}
			for conv, err := range f.Store.LoopConversationsContext(t.Context(), store.LoopQuery{ConversationID: conversationOf(t, f, question)}) {
				require.NoError(err)
				for m, err := range conv.Walk() {
					require.NoError(err)
					messages[m.ID] = m
				}
			}
			require.Len(messages, 4)
			for _, id := range []int64{toAlias, fromAlias, reply} {
				assert.True(messages[id].Sender.IsOwner, "sender of message %d", id)
			}
			assert.False(messages[question].Sender.IsOwner)
			for _, id := range []int64{toAlias, fromAlias, question} {
				require.Len(messages[id].Recipients, 1)
				assert.True(messages[id].Recipients[0].IsOwner, "recipient of message %d", id)
			}
			require.Len(messages[reply].Recipients, 2)
			for _, r := range messages[reply].Recipients {
				assert.Equal(r.Address == loopOwner, r.IsOwner, r.Address)
			}
		})
	}
}

func TestLoopFollowsTheArchiveWithoutARefresh(t *testing.T) {
	for _, tt := range []struct {
		name         string
		setup        func(f *storetest.Fixture) int64
		write        func(t *testing.T, f *storetest.Fixture, id int64)
		before, want int
	}{
		{"an attachment added after Thanks answers the request",
			func(f *storetest.Fixture) int64 {
				f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex}, Body: "Can you send the deck?", SentAt: loopAgo(5 * loopDay)})
				return f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Thanks!", SentAt: loopAgo(4 * loopDay)})
			},
			func(t *testing.T, f *storetest.Fixture, id int64) {
				t.Helper()
				_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO attachments (message_id, filename, storage_path, attachment_role)
					VALUES (?, 'deck.pdf', 'ab/deck', 'standalone')`), id)
				require.NoError(t, err)
			}, 1, 0},
		{"a body edit shows on the next list",
			func(f *storetest.Fixture) int64 {
				return f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Here are my notes.", SentAt: loopAgo(4 * loopDay), NoRaw: true})
			},
			func(t *testing.T, f *storetest.Fixture, id int64) {
				t.Helper()
				require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: "Can you send the signed contract?", Valid: true}, sql.NullString{}))
			}, 0, 1},
		{"a raw-MIME backfill that marks it automated hides it",
			func(f *storetest.Fixture) int64 {
				return f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Can you send the deck?", SentAt: loopAgo(4 * loopDay), NoRaw: true})
			},
			func(t *testing.T, f *storetest.Fixture, id int64) {
				t.Helper()
				require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("From: "+loopAlex+"\r\nTo: "+loopOwner+
					"\r\nAuto-Submitted: auto-generated\r\nSubject: Deck\r\n\r\nCan you send the deck?\r\n")))
			}, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := storetest.New(t)
			id := tt.setup(f)
			require.Len(t, listLoops(t, f, loopNow), tt.before, "before the write")
			tt.write(t, f, id)
			assert.Len(t, listLoops(t, f, loopNow), tt.want)
		})
	}
}

func TestLoopDirectChatListsWithoutAnyJobRunning(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, loopOwner, "manual"))
	owner := f.EnsureParticipant(loopOwner, "", "example.com")
	friend := f.EnsureParticipant(loopAlex, "", "example.test")
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "chat-1", "direct_chat", "Alex")
	require.NoError(err)
	require.NoError(f.Store.EnsureConversationParticipant(conversation, owner, "member"))
	require.NoError(f.Store.EnsureConversationParticipant(conversation, friend, "member"))
	id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			SourceID: f.Source.ID, ConversationID: conversation, MessageType: "whatsapp", SourceMessageID: "chat-msg-1",
			SenderID: sql.NullInt64{Int64: friend, Valid: true}, SentAt: sql.NullTime{Time: loopAgo(3 * loopDay), Valid: true},
		},
		BodyText: sql.NullString{String: "are you coming on Saturday?", Valid: true},
	})
	require.NoError(err)

	listed := listLoops(t, f, loopNow)
	require.Len(listed, 1)
	assert.Equal(id, listed[0].Anchor.ID)
	assert.Equal(store.ChannelChat, listed[0].Channel)
	assert.Equal(loops.ReplyOwed, listed[0].Kind)
}

func TestLoopDirectChatStopsAtSubstantiveBoundaryAcrossPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, loopOwner, "manual"))
	owner := f.EnsureParticipant(loopOwner, "", "example.com")
	friend := f.EnsureParticipant(loopAlex, "", "example.test")
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "long-chat", "direct_chat", "Long chat")
	require.NoError(err)
	require.NoError(f.Store.EnsureConversationParticipant(conversation, owner, "member"))
	require.NoError(f.Store.EnsureConversationParticipant(conversation, friend, "member"))

	messages := []struct {
		fromOwner bool
		body      string
	}{
		{body: "Can you review the first draft?"},
		{fromOwner: true, body: "I sent my notes."},
		{body: "Can you check the budget?"},
		{fromOwner: true, body: "The budget looks right."},
		{body: "Can you confirm the date?"},
		{fromOwner: true, body: "The date works."},
		{body: "Can you review the new draft?"},
		{fromOwner: true, body: "Can you check the contract?"},
	}
	ids := make([]int64, 0, len(messages))
	for i, message := range messages {
		sender := friend
		if message.fromOwner {
			sender = owner
		}
		id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{
				SourceID: f.Source.ID, ConversationID: conversation, MessageType: "whatsapp",
				SourceMessageID: "long-chat-" + strconv.Itoa(i),
				SenderID:        sql.NullInt64{Int64: sender, Valid: true},
				SentAt:          sql.NullTime{Time: loopAgo(time.Duration(len(messages)-i) * 4 * loopDay), Valid: true},
			},
			BodyText: sql.NullString{String: message.body, Valid: true},
		})
		require.NoError(err)
		ids = append(ids, id)
	}

	counted := &loopBodyCountingStore{Store: f.Store}
	result, err := loops.List(t.Context(), counted, loops.Options{}, loopNow)
	require.NoError(err)
	require.Len(result.Loops, 1)
	assert.Equal(ids[len(ids)-1], result.Loops[0].Anchor.ID)
	newestPage := ids[len(ids)-4:]
	for _, id := range counted.bodyReads {
		assert.Contains(newestPage, id, "detection must not read bodies from older settled history")
	}
}

func TestLoopDirectChatStopsAtOutboundAnswerBeforeOlderExchanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	messages := make([]loopChatMessage, 0, 24)
	for i := range 12 {
		messages = append(messages,
			loopChatMessage{body: "Can you review draft " + strconv.Itoa(i) + "?"},
			loopChatMessage{fromOwner: true, body: "I reviewed the draft."},
		)
	}
	ids := persistLoopDirectChat(t, f, "outbound-answer-boundary", messages)

	counted := &loopBodyCountingStore{Store: f.Store}
	result, err := loops.List(t.Context(), counted, loops.Options{}, loopNow)
	require.NoError(err)
	assert.Empty(result.Loops)
	newestPage := ids[len(ids)-4:]
	for _, id := range counted.bodyReads {
		assert.Contains(newestPage, id, "an outbound answer ends the direct-chat scan before older exchanges")
	}
}

func TestLoopDirectChatStopsAtBoundaryBeyondInitialPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	messages := make([]loopChatMessage, 0, 29)
	for range 24 {
		messages = append(messages, loopChatMessage{body: "The earlier report is ready."})
	}
	messages = append(messages,
		loopChatMessage{body: "The prior version is ready."},
		loopChatMessage{fromOwner: true, body: "I reviewed the notes."},
		loopChatMessage{fromOwner: true, body: "I sent the revised copy."},
		loopChatMessage{fromOwner: true, body: "The date works for me."},
		loopChatMessage{fromOwner: true, body: "Can you confirm the new date?"},
	)
	ids := persistLoopDirectChat(t, f, "older-boundary-beyond-window", messages)

	counted := &loopBodyCountingStore{Store: f.Store}
	result, err := loops.List(t.Context(), counted, loops.Options{}, loopNow)
	require.NoError(err)
	require.Len(result.Loops, 1)
	assert.Equal(ids[len(ids)-1], result.Loops[0].Anchor.ID)
	newestPageAndBoundary := ids[len(ids)-5:]
	for _, id := range counted.bodyReads {
		assert.Contains(newestPageAndBoundary, id, "a direct-chat boundary beyond the initial window stops older body reads")
	}
}

// bulkThreads archives one email thread per entry of ages. A thread is Alex's
// question to you, or with settled, your question to Alex and Alex's answer a
// minute later. It returns the ID of each thread's newest message, by direct
// inserts so thousands stay fast.
func bulkThreads(tb testing.TB, st *store.Store, sourceID int64, ages []time.Duration, settled bool) []int64 {
	tb.Helper()
	require := require.New(tb)
	require.NoError(st.AddAccountIdentity(sourceID, loopOwner, "manual"))
	owner, err := st.EnsureParticipant(loopOwner, "", "example.com")
	require.NoError(err)
	alex, err := st.EnsureParticipant(loopAlex, "", "example.test")
	require.NoError(err)
	tx, err := st.DB().BeginTx(tb.Context(), nil)
	require.NoError(err)
	add := func(conversation int64, key string, from, to int64, fromAddress, toAddress string, age time.Duration, body string) int64 {
		var message int64
		require.NoError(tx.QueryRowContext(tb.Context(), st.Rebind(`INSERT INTO messages (conversation_id, source_id, source_message_id, message_type, sender_id, sent_at)
			VALUES (?, ?, ?, 'email', ?, ?) RETURNING id`), conversation, sourceID, key, from, loopAgo(age)).Scan(&message))
		_, err = tx.ExecContext(tb.Context(), st.Rebind(`INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
			VALUES (?, ?, 'from', ?), (?, ?, 'to', ?)`), message, from, fromAddress, message, to, toAddress)
		require.NoError(err)
		_, err = tx.ExecContext(tb.Context(), st.Rebind(`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`), message, body)
		require.NoError(err)
		return message
	}
	ids := make([]int64, 0, len(ages))
	for i, age := range ages {
		var conversation int64
		require.NoError(tx.QueryRowContext(tb.Context(), st.Rebind(`INSERT INTO conversations (source_id, source_conversation_id, conversation_type)
			VALUES (?, ?, 'email_thread') RETURNING id`), sourceID, "bulk-"+strconv.Itoa(i)).Scan(&conversation))
		if settled {
			add(conversation, "bulk-ask-"+strconv.Itoa(i), owner, alex, loopOwner, loopAlex, age+time.Minute, "Can you review it?")
			ids = append(ids, add(conversation, "bulk-answer-"+strconv.Itoa(i), alex, owner, loopAlex, loopOwner, age, "Looks good to me."))
			continue
		}
		ids = append(ids, add(conversation, "bulk-msg-"+strconv.Itoa(i), alex, owner, loopAlex, loopOwner, age, "Can you review it?"))
	}
	require.NoError(tx.Commit())
	return ids
}

func TestLoopConversationsLoadInChunksWithoutACap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	const total = 3000
	ages := make([]time.Duration, total)
	for i := range ages {
		ages[i] = time.Duration(i+1) * time.Minute
	}
	bulkThreads(t, f.Store, f.Source.ID, ages, false)

	var conversations, messages int
	for conv, err := range f.Store.LoopConversationsContext(t.Context(), store.LoopQuery{Since: loopAgo(30 * loopDay)}) {
		require.NoError(err)
		conversations++
		messages += len(conv.Messages)
	}
	assert.Equal(total, conversations, "no cap on candidate conversations")
	assert.Equal(total, messages)
}

func TestLoopListingUsesChronologicalKeysForMixedOffsetTimestamps(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	firstAnswer := f.PersistEmail(storetest.EmailSpec{
		Thread: "mixed-offset-first", From: loopOwner, To: []string{loopAlex},
		Body: "I sent the contract.", SentAt: loopAgo(4*loopDay - time.Hour),
	})
	firstQuestion := f.PersistEmail(storetest.EmailSpec{
		Thread: "mixed-offset-first", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the contract?", SentAt: loopAgo(4 * loopDay),
	})
	secondQuestion := f.PersistEmail(storetest.EmailSpec{
		Thread: "mixed-offset-second", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the budget?", SentAt: loopAgo(4*loopDay - 30*time.Minute),
	})
	secondAnswer := f.PersistEmail(storetest.EmailSpec{
		Thread: "mixed-offset-second", From: loopOwner, To: []string{loopAlex},
		Body: "I sent the budget.", SentAt: loopAgo(4*loopDay - 90*time.Minute),
	})
	oldQuestion := f.PersistEmail(storetest.EmailSpec{
		Thread: "mixed-offset-before-since", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the invoice?", SentAt: loopAgo(4 * loopDay),
	})

	setSentAt := func(id int64, value string) {
		t.Helper()
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`UPDATE messages SET sent_at = ? WHERE id = ?`), value, id)
		require.NoError(err)
	}
	setSentAt(firstQuestion, "2026-10-15T08:00:00.000001+02:00") // 06:00:00.000001 UTC
	setSentAt(firstAnswer, "2026-10-15T06:00:00.000002Z")
	setSentAt(secondQuestion, "2026-10-15T08:30:00.000000000+02:00") // 06:30 UTC
	setSentAt(secondAnswer, "2026-10-15T07:30:00.000000000Z")
	setSentAt(oldQuestion, "2026-10-15T07:00:00.000000000+02:00") // 05:00 UTC

	for _, name := range []string{"mixed-offset-first", "mixed-offset-second"} {
		for _, hour := range []int{8, 9, 10, 11} {
			id := f.PersistEmail(storetest.EmailSpec{
				Thread: name, From: loopAlex, To: []string{loopOwner},
				Body: "Weekly update.", ListID: "updates.example.test",
				SentAt: time.Date(2026, 10, 15, hour, 0, 0, 0, time.UTC),
			})
			setSentAt(id, time.Date(2026, 10, 15, hour, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
		}
	}

	since := time.Date(2026, 10, 15, 5, 30, 0, 0, time.UTC)
	var heads []store.LoopConversation
	for conversation, err := range f.Store.LoopConversationsContext(t.Context(), store.LoopQuery{Since: since}) {
		require.NoError(err)
		heads = append(heads, conversation)
	}
	require.Len(heads, 2, "the 05:00 UTC conversation is before Since")
	assert.Equal(conversationOf(t, f, firstQuestion), heads[0].ConversationID)
	assert.True(heads[0].FirstAt.Equal(time.Date(2026, 10, 15, 6, 0, 0, 1_000, time.UTC)), "FirstAt must preserve the earliest instant's microsecond precision")
	assert.Equal(conversationOf(t, f, secondQuestion), heads[1].ConversationID)
	assert.True(heads[1].FirstAt.Equal(time.Date(2026, 10, 15, 6, 30, 0, 0, time.UTC)))

	result := listLoops(t, f, loopNow)
	require.Len(result, 1, "the two answered conversations must stay closed across page boundaries")
	assert.Equal(oldQuestion, result[0].Anchor.ID)
}

func persistLoopBeeperMessage(t *testing.T, st *store.Store, sourceID, conversationID, senderID int64, sourceMessageID, body string, sentAt time.Time) int64 {
	t.Helper()
	messageID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			SourceID: sourceID, ConversationID: conversationID, SourceMessageID: sourceMessageID,
			MessageType: "beeper", SenderID: sql.NullInt64{Int64: senderID, Valid: true},
			SentAt: sql.NullTime{Time: sentAt, Valid: true},
		},
		BodyText: sql.NullString{String: body, Valid: true},
	})
	require.NoError(t, err)
	return messageID
}

func TestMergeDuplicatesPreservesSourceConversationLoopDecision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	from, err := f.Store.GetOrCreateSource("beeper", "history-loop")
	require.NoError(err)
	into, err := f.Store.GetOrCreateSource("beeper", "live-loop")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(into.ID, loopOwner, "manual"))
	owner := f.EnsureParticipant(loopOwner, "Example Owner", "example.test")
	alex := f.EnsureParticipant(loopAlex, "Alex Example", "example.test")

	room := "!shared-loop:beeper.local"
	historyConversation, err := f.Store.EnsureConversationWithType(from.ID, room, "direct_chat", "Shared loop")
	require.NoError(err)
	liveConversation, err := f.Store.EnsureConversationWithType(into.ID, room, "direct_chat", "Shared loop")
	require.NoError(err)
	for _, conversationID := range []int64{historyConversation, liveConversation} {
		require.NoError(f.Store.EnsureConversationParticipant(conversationID, owner, "member"))
		require.NoError(f.Store.EnsureConversationParticipant(conversationID, alex, "member"))
	}

	firstAt, secondAt := loopAgo(5*loopDay), loopAgo(3*loopDay)
	historyFirst := persistLoopBeeperMessage(t, f.Store, from.ID, historyConversation, alex, "loop-question-1", "Can you send the contract?", firstAt)
	historySecond := persistLoopBeeperMessage(t, f.Store, from.ID, historyConversation, alex, "loop-question-2", "Can you send the budget?", secondAt)
	older := persistLoopBeeperMessage(t, f.Store, from.ID, historyConversation, alex, "loop-question-older", "Can you send the invoice?", loopAgo(7*loopDay))
	liveFirst := persistLoopBeeperMessage(t, f.Store, into.ID, liveConversation, alex, "loop-question-1", "Can you send the contract?", firstAt)

	_, err = f.Store.MarkLoopDoneContext(t.Context(), historyFirst, []store.LoopMessage{{ID: historyFirst}, {ID: historySecond}, {ID: older}})
	require.NoError(err)
	_, err = f.Store.MergeDuplicates(liveFirst, []int64{historyFirst}, "loop-source-decision")
	require.NoError(err)
	assert.Empty(listLoops(t, f, loopNow), "the source's remaining covered questions and the duplicate stay cleared")
	var state string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT state FROM loop_decisions WHERE message_id = ?`), historySecond).Scan(&state))
	assert.Equal(store.LoopStateDone, state, "the mark remains attached to the source conversation with other messages")
}

func TestDoneDecisionKeepsCoveredQuestionsAfterTimestampReordering(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.PersistEmail(storetest.EmailSpec{
		Thread: "timestamp-reordered", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay),
	})
	secondAt := loopAgo(3 * loopDay)
	second := f.PersistEmail(storetest.EmailSpec{
		Thread: "timestamp-reordered", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the budget?", SentAt: secondAt,
	})
	_, err := f.Store.MarkLoopDoneContext(t.Context(), first, []store.LoopMessage{{ID: first}, {ID: second, OccurredAt: secondAt}})
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET sent_at = ? WHERE id = ?`), loopAgo(2*loopDay), first)
	require.NoError(err)
	assert.Empty(listLoops(t, f, loopNow), "the first question stays covered after it moves past the saved timestamp boundary")
}

func TestMarkedQuestionsFollowDuplicateSurvivors(t *testing.T) {
	for _, kind := range []string{"duplicates", "sources"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			from := f.Source.ID
			drop := f.PersistEmail(storetest.EmailSpec{Thread: "first", From: loopAlex, To: []string{loopOwner}, Body: "Can you send it?", SentAt: loopAgo(5 * loopDay)})
			dropThrough := f.PersistEmail(storetest.EmailSpec{Thread: "first", From: loopAlex, To: []string{loopOwner}, Body: "Any update?", SentAt: loopAgo(3 * loopDay)})
			_, err := loops.Done(t.Context(), f.Store, drop, loopNow)
			require.NoError(err)
			if kind == "sources" {
				f.Source, err = f.Store.GetOrCreateSource("gmail", "live@example.test")
				require.NoError(err)
			}
			keep := f.PersistEmail(storetest.EmailSpec{Thread: "second", From: loopAlex, To: []string{loopOwner}, Body: "Can you send it?", SentAt: loopAgo(5 * loopDay)})
			keepThrough := f.PersistEmail(storetest.EmailSpec{Thread: "second", From: loopAlex, To: []string{loopOwner}, Body: "Any update?", SentAt: loopAgo(3 * loopDay)})
			if kind == "sources" {
				result, err := f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: f.Source.ID})
				require.NoError(err)
				require.Equal(int64(2), result.DuplicatesHidden)
			} else {
				_, err = f.Store.MergeDuplicates(keep, []int64{drop}, "loop-anchor")
				require.NoError(err)
				_, err = f.Store.MergeDuplicates(keepThrough, []int64{dropThrough}, "loop-through")
				require.NoError(err)
			}
			result := listLoops(t, f, loopNow)
			assert.Empty(result, "the same marked questions stay covered whichever duplicate copy survives")
		})
	}
}

func TestDuplicateDecisionLeavesSameTimeQuestionOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.PersistEmail(storetest.EmailSpec{
		Thread: "default-thread", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay),
	})
	keptCutoff := f.PersistEmail(storetest.EmailSpec{
		Thread: "default-thread", From: loopAlex, To: []string{loopOwner},
		Body: "And can you send the budget?", SentAt: loopAgo(3 * loopDay),
	})
	coveredAfterSurvivor := f.PersistEmail(storetest.EmailSpec{
		Thread: "default-thread", From: loopAlex, To: []string{loopOwner},
		Body: "Can you send the revised budget?", SentAt: loopAgo(3 * loopDay),
	})
	duplicateCutoff := f.PersistEmail(storetest.EmailSpec{
		Thread: "default-thread", From: loopAlex, To: []string{loopOwner},
		Body: "And can you send the budget?", SentAt: loopAgo(3 * loopDay),
	})
	assert.Less(keptCutoff, coveredAfterSurvivor)
	assert.Less(coveredAfterSurvivor, duplicateCutoff)
	_, err := f.Store.MarkLoopDoneContext(t.Context(), first, []store.LoopMessage{{ID: first}, {ID: duplicateCutoff}})
	require.NoError(err)

	_, err = f.Store.MergeDuplicates(keptCutoff, []int64{duplicateCutoff}, "loop-cutoff-order")
	require.NoError(err)

	marks, err := f.Store.LoopMarksContext(t.Context())
	require.NoError(err)
	mark, ok := marks[keptCutoff]
	require.True(ok)
	assert.Equal(store.LoopStateDone, mark.State)
	assert.Equal(store.LoopStateDone, marks[duplicateCutoff].State)
	listed := listLoops(t, f, loopNow)
	require.Len(listed, 1)
	assert.Equal(coveredAfterSurvivor, listed[0].Anchor.ID, "the distinct same-time question stays open")
}

func loopDecisionState(t *testing.T, f *storetest.Fixture, anchorID int64) string {
	t.Helper()
	var state string
	err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT state FROM loop_decisions WHERE message_id = ?`), anchorID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return state
}

// loopMark is one question's stored decision.
type loopMark struct {
	State string
	Until time.Time
}

func doneMark() loopMark { return loopMark{State: store.LoopStateDone} }

func snoozedUntil(until time.Time) loopMark {
	return loopMark{State: store.LoopStateSnoozed, Until: until}
}

func putLoopMark(t *testing.T, st *store.Store, id int64, m loopMark) {
	t.Helper()
	var err error
	if m.State == store.LoopStateDone {
		_, err = st.MarkLoopDoneContext(t.Context(), id, []store.LoopMessage{{ID: id}})
	} else {
		_, err = st.SnoozeLoopContext(t.Context(), id, m.Until, []store.LoopMessage{{ID: id}})
	}
	require.NoError(t, err)
}

func storedLoopMark(t *testing.T, st *store.Store, id int64) loopMark {
	t.Helper()
	var m loopMark
	var until sql.NullTime
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT state, snoozed_until FROM loop_decisions WHERE message_id = ?`), id).Scan(&m.State, &until))
	m.Until = until.Time.UTC()
	return m
}

// mergedMark is two copies of one message holding different marks. The mark
// that wins is the same whichever copy survives.
type mergedMark struct {
	name             string
	drop, keep, want loopMark
}

func mergedMarkCases() []mergedMark {
	soon, later := loopNow.Add(2*loopDay), loopNow.Add(5*loopDay)
	return []mergedMark{
		{"done beats the survivor's snooze", doneMark(), snoozedUntil(soon), doneMark()},
		{"done beats the duplicate's snooze", snoozedUntil(soon), doneMark(), doneMark()},
		{"the duplicate's later snooze wins", snoozedUntil(later), snoozedUntil(soon), snoozedUntil(later)},
		{"the survivor's later snooze stays", snoozedUntil(soon), snoozedUntil(later), snoozedUntil(later)},
	}
}

func TestMergeDuplicatesKeepsTheStrongestLoopMark(t *testing.T) {
	for _, tt := range mergedMarkCases() {
		t.Run(tt.name, func(t *testing.T) {
			f := storetest.New(t)
			drop := f.PersistEmail(storetest.EmailSpec{Thread: "first", From: loopAlex, To: []string{loopOwner}, Body: "Can you send it?", SentAt: loopAgo(5 * loopDay)})
			keep := f.PersistEmail(storetest.EmailSpec{Thread: "second", From: loopAlex, To: []string{loopOwner}, Body: "Can you send it?", SentAt: loopAgo(5 * loopDay)})
			putLoopMark(t, f.Store, drop, tt.drop)
			putLoopMark(t, f.Store, keep, tt.keep)

			_, err := f.Store.MergeDuplicates(keep, []int64{drop}, "batch-loop-marks")
			require.NoError(t, err)
			got := storedLoopMark(t, f.Store, keep)
			assert.Equal(t, tt.want.State, got.State)
			assert.Equal(t, tt.want.Until, got.Until)
		})
	}
}

func TestDoneOutlivesTheAnchorLeavingAndReturning(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex}, Body: "Hi", SentAt: loopAgo(20 * loopDay)})
	question := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay)})
	_, err := loops.Done(t.Context(), f.Store, question, loopNow)
	require.NoError(err)

	reply := f.PersistEmail(storetest.EmailSpec{From: loopOwner, To: []string{loopAlex}, Body: "Sent, see attached.", SentAt: loopAgo(loopDay)})
	assert.Empty(listLoops(t, f, loopNow), "listing while the reply is live, the question is no longer an open anchor")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), loopNow, reply)
	require.NoError(err)

	assert.Equal(store.LoopStateDone, loopDecisionState(t, f, question))
	assert.Empty(listLoops(t, f, loopNow), "the question anchors again and its done mark still holds")
}

// realisticMail uses the shared MIME builder, with distinct attachment bytes
// per message so the benchmark cannot compress them to a repeat.
func realisticMail(i int, body string, attachment []byte) []byte {
	message := email.NewMessage().From("Alex Example <"+loopAlex+">").To(loopOwner).
		Subject("Contract draft "+strconv.Itoa(i)).Date("Mon, 19 Oct 2026 09:00:00 +0000").
		Header("Message-ID", "<bench-"+strconv.Itoa(i)+"@example.test>").Body(body).CRLF()
	if attachment != nil {
		payload := append([]byte(nil), attachment...)
		binary.BigEndian.PutUint64(payload, uint64(i))
		message.WithAttachment("contract.pdf", "application/pdf", payload)
	}
	return message.Bytes()
}

// BenchmarkListLoops lists loops over 3,000 conversations active in the
// lookback, each with its stored original.
func BenchmarkListLoops(b *testing.B) {
	attachment := make([]byte, 96<<10)
	_, err := rand.NewChaCha8([32]byte{1}).Read(attachment)
	require.NoError(b, err)
	for _, tt := range []struct {
		name       string
		settled    bool
		attachment []byte
		wantLoops  int
	}{
		{"open questions with 96 KiB attachments", false, attachment, loops.DefaultLimit},
		{"answered threads, plain text", true, nil, 0},
		{"answered threads with 96 KiB attachments", true, attachment, 0},
	} {
		b.Run(tt.name, func(b *testing.B) {
			st, err := store.Open(filepath.Join(b.TempDir(), "loops.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, st.Close()) })
			require.NoError(b, st.InitSchema())
			source, err := st.GetOrCreateSource("gmail", loopOwner)
			require.NoError(b, err)
			ages := make([]time.Duration, 3000)
			for i := range ages {
				ages[i] = loopDay + time.Duration(i+1)*time.Minute
			}
			body := "Can you review it?"
			if tt.settled {
				body = "Looks good to me."
			}
			for i, id := range bulkThreads(b, st, source.ID, ages, tt.settled) {
				require.NoError(b, st.UpsertMessageRaw(id, realisticMail(i, body, tt.attachment)))
			}
			b.ResetTimer()
			for b.Loop() {
				result, err := loops.List(b.Context(), st, loops.Options{}, loopNow)
				require.NoError(b, err)
				require.Len(b, result.Loops, tt.wantLoops)
			}
		})
	}
}

func TestLoopMultiFromUsesStoredSender(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	question := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay)})
	alexID := f.EnsureParticipant(loopAlex, "", "example.test")
	samID := f.EnsureParticipant("sam@example.test", "", "example.test")
	require.NoError(f.Store.ReplaceMessageRecipients(question, "from", []int64{alexID, samID}, []string{loopAlex, "sam@example.test"}))

	var messages []store.LoopMessage
	for conv, err := range f.Store.LoopConversationsContext(t.Context(), store.LoopQuery{ConversationID: conversationOf(t, f, question)}) {
		require.NoError(err)
		messages = append(messages, conv.Messages...)
	}
	require.Len(messages, 1)
	assert.Equal(question, messages[0].ID)
	assert.Equal(alexID, messages[0].Sender.ParticipantID)
	assert.Equal(loopAlex, messages[0].Sender.Address)
}

func TestSourceMergeKeepsEachKnownQuestionState(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			t.Run(strconv.FormatBool(reverse)+"/duplicate="+strconv.FormatBool(duplicate), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := storetest.New(t)
				from, err := f.Store.GetOrCreateSource("beeper", "history@example.test")
				require.NoError(err)
				into, err := f.Store.GetOrCreateSource("beeper", "live@example.test")
				require.NoError(err)
				sender := f.EnsureParticipant(loopAlex, "", "example.test")
				left, err := f.Store.EnsureConversationWithType(from.ID, "!questions:example.test", "direct_chat", "Questions")
				require.NoError(err)
				right, err := f.Store.EnsureConversationWithType(into.ID, "!questions:example.test", "direct_chat", "Questions")
				require.NoError(err)
				done := persistLoopBeeperMessage(t, f.Store, from.ID, left, sender, "done", "Can you send the contract?", loopAgo(2*loopDay))
				_, err = loops.Done(t.Context(), f.Store, done, loopNow)
				require.NoError(err)
				snoozed := persistLoopBeeperMessage(t, f.Store, into.ID, right, sender, "snoozed", "Can you send the budget?", loopAgo(5*loopDay))
				until := loopNow.Add(loopDay)
				_, err = loops.Snooze(t.Context(), f.Store, snoozed, until, loopNow)
				require.NoError(err)
				open := persistLoopBeeperMessage(t, f.Store, into.ID, right, sender, "open", "Can you send the invoice?", loopAgo(4*loopDay))
				var shared int64
				if duplicate {
					shared = persistLoopBeeperMessage(t, f.Store, into.ID, right, sender, "done", "Can you send the contract?", loopAgo(2*loopDay))
				}
				if reverse {
					from, into = into, from
				}
				result, err := f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
				require.NoError(err)
				if duplicate {
					assert.Equal(int64(1), result.DuplicatesHidden)
				} else {
					assert.Zero(result.DuplicatesHidden)
				}
				for _, at := range []time.Time{loopNow, until} {
					marks, err := f.Store.LoopMarksContext(t.Context())
					require.NoError(err)
					assert.Equal(store.LoopStateDone, marks[done].State)
					require.NotNil(marks[snoozed].SnoozedUntil)
					assert.Equal(at.Before(until), at.Before(*marks[snoozed].SnoozedUntil))
					assert.NotContains(marks, open)
					if duplicate && !reverse {
						assert.Equal(store.LoopStateDone, marks[shared].State)
					}
					assert.Len(listLoops(t, f, at), 1)
				}
			})
		}
	}
}

func TestDuplicateUndoKeepsBothQuestionDecisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	duplicate := f.PersistEmail(storetest.EmailSpec{Thread: "duplicate", From: loopAlex, To: []string{loopOwner}, Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay)})
	survivor := f.PersistEmail(storetest.EmailSpec{Thread: "survivor", From: loopAlex, To: []string{loopOwner}, Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay)})
	_, err := loops.Done(t.Context(), f.Store, duplicate, loopNow)
	require.NoError(err)
	_, err = f.Store.MergeDuplicates(survivor, []int64{duplicate}, "loop-undo")
	require.NoError(err)
	assert.Empty(listLoops(t, f, loopNow))
	restored, err := f.Store.UndoDedup("loop-undo")
	require.NoError(err)
	assert.Equal(int64(1), restored)
	assert.Empty(listLoops(t, f, loopNow), "both restored and surviving questions keep their decisions")
	marks, err := f.Store.LoopMarksContext(t.Context())
	require.NoError(err)
	assert.Equal(store.LoopStateDone, marks[duplicate].State)
	assert.Equal(store.LoopStateDone, marks[survivor].State)
}

func TestUnmarkedSourceMergeCreatesNoLoopDecisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	firstSource := f.Source
	f.PersistEmail(storetest.EmailSpec{Thread: "unmarked", From: loopAlex, To: []string{loopOwner}, Body: "Can you send the contract?", SentAt: loopAgo(5 * loopDay)})
	other, err := f.Store.GetOrCreateSource("gmail", "history@example.test")
	require.NoError(err)
	f.Source = other
	f.PersistEmail(storetest.EmailSpec{Thread: "unmarked", From: loopAlex, To: []string{loopOwner}, Body: "Can you send the report?", SentAt: loopAgo(4 * loopDay)})
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: other.ID, IntoSourceID: firstSource.ID})
	require.NoError(err)
	marks, err := f.Store.LoopMarksContext(t.Context())
	require.NoError(err)
	assert.Empty(marks)
}

func TestLoopGroupMembershipUsesOneMetadataQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messages := make([]loopChatMessage, 60)
	for i := range messages {
		messages[i].body = "Can you send the contract?"
	}
	for _, room := range []string{"group-one", "group-two"} {
		ids := persistLoopDirectChat(t, f, room, messages)
		ref, err := f.Store.MessageConversationContext(t.Context(), ids[0])
		require.NoError(err)
		extra := f.EnsureParticipant("sam@example.test", "", "example.test")
		require.NoError(f.Store.EnsureConversationParticipant(ref.ConversationID, extra, "member"))
	}
	logs := captureAttachmentQueryLogs(t)
	var groups int
	for conv, err := range f.Store.LoopConversationsContext(t.Context(), store.LoopQuery{}) {
		require.NoError(err)
		if conv.ConversationType == "direct_chat" {
			assert.True(conv.GroupChat)
			groups++
		}
	}
	assert.Equal(2, groups)
	var memberships, details int
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var event struct {
			Stmt  string `json:"stmt"`
			Nargs int    `json:"nargs"`
		}
		require.NoError(json.Unmarshal([]byte(line), &event))
		if strings.Contains(event.Stmt, "FROM conversation_participants cp LEFT JOIN person_participants") {
			memberships++
		}
		if strings.Contains(event.Stmt, "SELECT m.id, COALESCE(m.sender_id") {
			details++
			assert.LessOrEqual(event.Nargs, 8, "only four newest messages per conversation are hydrated")
		}
	}
	assert.Equal(1, memberships)
	assert.Equal(1, details)
}

func TestRecipientlessOutboundKeepsQuestionOpen(t *testing.T) {
	f := storetest.New(t)
	question := f.PersistEmail(storetest.EmailSpec{From: loopAlex, To: []string{loopOwner}, Body: "Can you approve the contract?", SentAt: loopAgo(5 * loopDay)})
	f.PersistEmail(storetest.EmailSpec{From: loopOwner, Body: "The report is ready for review.", SentAt: loopAgo(3 * loopDay)})
	result := listLoops(t, f, loopNow)
	require.Len(t, result, 1)
	assert.Equal(t, question, result[0].Anchor.ID)
}
