package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The threaded fixture is a small, checked-in, msgvault-shaped mailbox: real
// In-Reply-To/References chains, quoted replies, varied dates and multiple
// participants. The TREC legal collection msgvault is otherwise evaluated
// against is flat — exactly one message per judged document — so it cannot
// exercise any of the threading behaviour these tests cover.

const fixtureDir = "testdata/threaded"

type fixtureMessage struct {
	MessageID      string   `json:"message_id"`
	ConversationID string   `json:"conversation_id"`
	InReplyTo      string   `json:"in_reply_to"`
	References     []string `json:"references"`
	Date           string   `json:"date"`
	From           string   `json:"from"`
	To             []string `json:"to"`
	Cc             []string `json:"cc"`
	Subject        string   `json:"subject"`
	Body           string   `json:"body"`
}

type fixtureMailbox struct {
	Description string           `json:"description"`
	Messages    []fixtureMessage `json:"messages"`
}

func loadFixture(t *testing.T) fixtureMailbox {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "mailbox.json"))
	require.NoError(t, err)
	var mb fixtureMailbox
	require.NoError(t, json.Unmarshal(raw, &mb))
	require.NotEmpty(t, mb.Messages)
	return mb
}

// TestFixture_IsThreadShaped guards the fixture itself. If someone flattens it
// (one message per thread, no quotes, one participant) it silently stops
// testing the thing it exists to test, so assert its shape explicitly.
func TestFixture_IsThreadShaped(t *testing.T) {
	mb := loadFixture(t)

	byID := make(map[string]fixtureMessage, len(mb.Messages))
	threads := make(map[string]int)
	senders := make(map[string]struct{})
	dates := make(map[string]struct{})
	quoted, replies := 0, 0

	for _, m := range mb.Messages {
		require.NotEmpty(t, m.MessageID, "every message needs a Message-ID")
		require.NotEmpty(t, m.ConversationID, "every message needs a conversation")
		require.NotEmpty(t, m.Date, "every message needs a date")
		byID[m.MessageID] = m
		threads[m.ConversationID]++
		senders[m.From] = struct{}{}
		dates[m.Date] = struct{}{}
		if strings.Contains(m.Body, "\n>") {
			quoted++
		}
		if m.InReplyTo != "" {
			replies++
		}
	}

	// Threading: replies must point at a real parent in the same thread, and
	// References must contain the parent.
	for _, m := range mb.Messages {
		if m.InReplyTo == "" {
			assert.Empty(t, m.References, "root %s should have no References", m.MessageID)
			continue
		}
		parent, ok := byID[m.InReplyTo]
		require.True(t, ok, "%s replies to unknown parent %s", m.MessageID, m.InReplyTo)
		assert.Equal(t, parent.ConversationID, m.ConversationID,
			"%s must share its parent's conversation", m.MessageID)
		assert.Contains(t, m.References, m.InReplyTo,
			"%s References must include its parent", m.MessageID)
	}

	// Shape: multi-message threads, singletons, quoted replies, many people.
	var multi, single int
	for _, n := range threads {
		if n > 1 {
			multi++
		} else {
			single++
		}
	}
	assert.GreaterOrEqual(t, multi, 2, "need several multi-message threads")
	assert.GreaterOrEqual(t, single, 1, "need at least one singleton thread")
	assert.GreaterOrEqual(t, replies, 4, "need real reply chains")
	assert.GreaterOrEqual(t, quoted, 4, "need quoted-reply bodies")
	assert.GreaterOrEqual(t, len(senders), 4, "need multiple participants")
	assert.Equal(t, len(mb.Messages), len(dates), "dates should be varied, not cloned")
}

// TestFixture_QrelsMatchMailbox keeps the judgments and the mailbox in step:
// every judged id must exist, so a renamed message or thread fails loudly
// instead of quietly scoring zero.
func TestFixture_QrelsMatchMailbox(t *testing.T) {
	mb := loadFixture(t)
	msgIDs := make(map[string]struct{}, len(mb.Messages))
	convIDs := make(map[string]struct{})
	for _, m := range mb.Messages {
		msgIDs[m.MessageID] = struct{}{}
		convIDs[m.ConversationID] = struct{}{}
	}

	topics, err := LoadTopics(filepath.Join(fixtureDir, "topics.tsv"))
	require.NoError(t, err)
	require.Len(t, topics, 3)

	// The fixture deliberately mixes labeled and unlabeled topics: the
	// optional category column (pointed vs spanning question shape) must
	// coexist with plain two-column lines in one file.
	assert.Equal(t, "spanning", topics[0].Category,
		"q1's relevant set is a whole thread, not one message")
	assert.Equal(t, "pointed", topics[1].Category)
	assert.Empty(t, topics[2].Category, "q3 pins the unlabeled two-column form")

	for _, tc := range []struct {
		file  string
		valid map[string]struct{}
	}{
		{"qrels_message.txt", msgIDs},
		{"qrels_conversation.txt", convIDs},
	} {
		q, err := LoadQrels(filepath.Join(fixtureDir, tc.file))
		require.NoError(t, err)
		for _, top := range topics {
			require.NotEmpty(t, q[top.ID], "%s: topic %s has no judgments", tc.file, top.ID)
			for docID := range q[top.ID] {
				assert.Contains(t, tc.valid, docID, "%s: judged id %q is not in the mailbox", tc.file, docID)
			}
			assert.NotEmpty(t, q.RelevantSet(top.ID), "%s: topic %s has no relevant docs", tc.file, top.ID)
		}
	}
}

// TestThreadCollapsing_ConversationKey is the regression the flat TREC corpus
// cannot catch. msgvault retrieves messages; with --doc-key=conversation the
// judged unit is the thread. A four-message thread filling the top of the
// ranking is ONE retrieved thread, not four. Scoring the un-collapsed list
// counts it four times, which inflates precision and drives recall above 1.0.
func TestThreadCollapsing_ConversationKey(t *testing.T) {
	qrels, err := LoadQrels(filepath.Join(fixtureDir, "qrels_conversation.txt"))
	require.NoError(t, err)
	rel := qrels.RelevantSet("q1")
	require.Equal(t, 1, len(rel), "q1 has exactly one relevant thread")

	// A message-level engine answering q1: the whole renewal thread ranks
	// first, then unrelated threads.
	perMessage := []string{
		"thread-renewal", "thread-renewal", "thread-renewal", "thread-renewal",
		"thread-insurance", "thread-forklift", "thread-newsletter", "thread-offsite",
	}

	// Un-collapsed: visibly broken.
	assert.InDelta(t, 0.4, PrecisionAt(perMessage, rel, 10), 1e-9,
		"un-collapsed P@10 counts the same thread four times")
	assert.InDelta(t, 4.0, RecallAt(perMessage, rel, 100), 1e-9,
		"un-collapsed recall exceeds 1.0, which is impossible")

	// Collapsed: one thread, one slot, at its best rank.
	collapsed := DedupeKeys(perMessage)
	assert.Equal(t, []string{
		"thread-renewal", "thread-insurance", "thread-forklift",
		"thread-newsletter", "thread-offsite",
	}, collapsed)
	assert.InDelta(t, 0.1, PrecisionAt(collapsed, rel, 10), 1e-9)
	assert.InDelta(t, 1.0, RecallAt(collapsed, rel, 100), 1e-9, "recall must never exceed 1.0")
	assert.InDelta(t, 1.0, ReciprocalRank(collapsed, rel), 1e-9)
	assert.InDelta(t, 1.0, NDCGAt(collapsed, rel, 10), 1e-9)
}

// TestDedupeKeys_PreservesBestRank pins the collapse rule: first occurrence
// wins (that is the thread's best rank) and empty keys — hits that carry no id
// for the chosen doc-key — are dropped rather than scored as a document.
func TestDedupeKeys_PreservesBestRank(t *testing.T) {
	assert.Equal(t, []string{"b", "a", "c"}, DedupeKeys([]string{"b", "a", "b", "c", "a"}))
	assert.Equal(t, []string{"a"}, DedupeKeys([]string{"", "a", "", "a"}))
	assert.Empty(t, DedupeKeys(nil))
	assert.Empty(t, DedupeKeys([]string{"", ""}))
}

// TestQuotedReplyDistractor_HurtsPrecision covers the other product-specific
// failure the legal corpus misses. Every reply in this fixture quotes its
// parent, so an engine that indexes quoted text without attributing it to the
// original message will surface a reply from an unrelated thread whose only
// match is the quotation. Judged non-relevant, so a quote-stripping regression
// shows up as a precision drop rather than passing silently.
func TestQuotedReplyDistractor_HurtsPrecision(t *testing.T) {
	qrels, err := LoadQrels(filepath.Join(fixtureDir, "qrels_message.txt"))
	require.NoError(t, err)
	rel := qrels.RelevantSet("q1")

	// t2-b is in the forklift thread and merely mentions the lease renewal
	// signing; t5-a is a newsletter whose subject contains the query words.
	assert.NotContains(t, rel, "<t2-b@example.com>")
	assert.NotContains(t, rel, "<t5-a@example.com>")

	clean := []string{"<t1-a@example.com>", "<t1-b@example.com>", "<t1-c@example.com>", "<t1-d@example.com>"}
	leaky := []string{"<t5-a@example.com>", "<t2-b@example.com>", "<t1-a@example.com>", "<t1-b@example.com>"}

	cleanScore := Evaluate(clean, rel)
	leakyScore := Evaluate(leaky, rel)

	assert.InDelta(t, 0.4, cleanScore.P10, 1e-9)
	assert.InDelta(t, 0.2, leakyScore.P10, 1e-9)
	assert.Less(t, leakyScore.NDCG10, cleanScore.NDCG10, "quote leakage must cost nDCG")
	assert.InDelta(t, 1.0, cleanScore.MRR, 1e-9)
	assert.InDelta(t, 1.0/3.0, leakyScore.MRR, 1e-9)
}
