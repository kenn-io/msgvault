package query

import (
	"bytes"
	"fmt"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSenderNameHintQueryFailure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	q := search.Parse("from:alex")
	requirements.NoError(env.DB.Close())
	var diagnostic bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostic)
	t.Cleanup(func() { log.SetOutput(previous) })
	assertions.Empty(env.Engine.MatchSenderName(env.Ctx, q))
	assertions.Contains(diagnostic.String(), "sender hint query failed")
}

func TestSenderNameHints(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	for i := range 4 {
		p := env.AddParticipant(dbtest.ParticipantOpts{Email: new(fmt.Sprintf("sender%d@example.com", i))})
		for j := 0; j <= i; j++ {
			id := env.AddMessage(dbtest.MessageOpts{FromID: p, Subject: "hintfixture"})
			// The participant was first seen without a name; the message supplies it.
			if j == 0 {
				env.SetFromName(id, "Alex Kim")
			}
		}
	}
	q := search.Parse("from:alex subject:hintfixture")
	hints := env.Engine.MatchSenderName(env.Ctx, q)
	requirements.Len(hints, 1)
	assertions.Equal(`from: matches addresses; senders matching "alex": try from:sender3@example.com subject:hintfixture (4 messages); other matching senders: sender2@example.com (3 messages), sender1@example.com (2 messages)`, hints[0].Message)
	results, err := env.Engine.Search(env.Ctx, search.Parse(hints[0].RetryWith.Query), 20, 0)
	requirements.NoError(err)
	assertions.Len(results, 4)
	for _, tc := range []struct {
		q             string
		opt           bool
		limit, offset int
		want          bool
	}{
		{"from:alex subject:hintfixture", true, 10, 0, true},
		{"from:example.co subject:hintfixture", true, 10, 0, true},
		{"from:alex subject:hintfixture", false, 10, 0, false},
		{"from:alex subject:hintfixture", true, 0, 0, false},
		{"from:alex subject:hintfixture", true, 10, 10, false},
		{"from:sender3@example.com subject:hintfixture", true, 10, 0, false},
		{"from:alex from:kim", true, 10, 0, false},
		{"from:alex | from:kim", true, 10, 0, false},
		{"from:alex subject:absent", true, 10, 0, false},
	} {
		t.Run(fmt.Sprintf("%s/%t/%d/%d", tc.q, tc.opt, tc.limit, tc.offset), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			result, err := env.Engine.SearchFastWithStats(env.Ctx, search.ParseOR(tc.q), tc.q, MessageFilter{IncludeHints: tc.opt}, ViewNoStats, tc.limit, tc.offset)
			requirements.NoError(err)
			assertions.Equal(tc.want, len(result.Hints) > 0)
		})
	}
}

func TestSenderNameHintLiteralAndDeletionScope(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	p := env.AddParticipant(dbtest.ParticipantOpts{Email: new("special@example.com"), DisplayName: new("Alex_% Kim")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: p, Subject: "hintfixture"})
	other := env.AddParticipant(dbtest.ParticipantOpts{Email: new("other@example.com"), DisplayName: new("Alex Other")})
	env.AddMessage(dbtest.MessageOpts{FromID: other, Subject: "hintfixture"})
	q := search.Parse("from:alex_% subject:hintfixture")
	hints := env.Engine.MatchSenderName(env.Ctx, q)
	requirements.Len(hints, 1)
	assertions.Equal("from:special@example.com subject:hintfixture", hints[0].RetryWith.Query)
	env.MarkDeletedByID(id)
	q.DeletionScope = search.DeletionScopeActive
	assertions.Nil(env.Engine.MatchSenderName(env.Ctx, q))
	q.DeletionScope = search.DeletionScopeAny
	hints = env.Engine.MatchSenderName(env.Ctx, q)
	requirements.Len(hints, 1)
	retry := search.Parse(hints[0].RetryWith.Query)
	retry.DeletionScope = q.DeletionScope
	results, err := env.Engine.Search(env.Ctx, retry, 10, 0)
	requirements.NoError(err)
	requirements.Len(results, 1)
	assertions.Equal(id, results[0].ID)
}

func TestSenderHintCandidatesRespectScope(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	for i := range 4 {
		sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new(fmt.Sprintf("hidden%d@example.com", i)), DisplayName: new("Alex")})
		env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture"})
	}
	source := env.AddSource(dbtest.SourceOpts{Type: "gmail", Identifier: "visible@example.com"})
	conv := env.AddConversation(dbtest.ConversationOpts{SourceID: source})
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("visible@example.com"), DisplayName: new("Jordan Lee")})
	visible := env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture", SourceID: source, ConversationID: conv})
	env.SetFromName(visible, "Alex Kim")
	env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture"})
	overridden := env.AddParticipant(dbtest.ParticipantOpts{Email: new("overridden@example.com"), DisplayName: new("Alex")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: overridden, Subject: "hintfixture", SourceID: source, ConversationID: conv})
	env.SetFromName(id, "Jordan")
	filter := MessageFilter{SourceID: &source, IncludeHints: true}
	q := search.Parse("from:alex subject:hintfixture")
	result, err := env.Engine.SearchFastWithStats(env.Ctx, q, "", filter, ViewNoStats, 10, 0)
	requirements.NoError(err)
	requirements.Len(result.Hints, 1)
	assertions.Equal(`from: matches addresses; senders matching "alex": try from:visible@example.com subject:hintfixture (1 message)`, result.Hints[0].Message)
	assertions.NotContains(result.Hints[0].Message, "hidden")
	assertions.NotContains(result.Hints[0].Message, "overridden")
	retry, err := env.Engine.SearchFastWithStats(env.Ctx, search.Parse(result.Hints[0].RetryWith.Query), "", filter, ViewNoStats, 10, 0)
	requirements.NoError(err)
	requirements.Len(retry.Messages, 1)
	assertions.Equal(visible, retry.Messages[0].ID)
}
