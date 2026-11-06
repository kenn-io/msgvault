package query

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSenderNameHints(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	for i := range 4 {
		p := env.AddParticipant(dbtest.ParticipantOpts{Email: new(fmt.Sprintf("alex%d@example.com", i))})
		for j := 0; j <= i; j++ {
			id := env.AddMessage(dbtest.MessageOpts{FromID: p, Subject: "hintfixture"})
			// The participant was first seen without a name; the message supplies it.
			if j == 0 {
				env.SetFromName(id, "Alex Kim")
			}
		}
	}
	q := search.Parse("from:alex subject:hintfixture")
	hint := env.Engine.MatchSenderName(env.Ctx, q, MessageFilter{})
	requirements.NotNil(hint)
	assertions.Equal(`from: matches addresses; senders matching "alex": alex3@example.com (4 messages), alex2@example.com (3 messages), alex1@example.com (2 messages); try from:alex3@example.com subject:hintfixture`, hint.Message)
	results, err := env.Engine.Search(env.Ctx, search.Parse(hint.Query), 20, 0)
	requirements.NoError(err)
	assertions.Len(results, 4)
	for _, tc := range []struct {
		q             string
		limit, offset int
		opt           bool
		want          bool
	}{
		{"from:alex subject:hintfixture", 10, 0, true, true},
		{"from:alex subject:hintfixture", 10, 0, false, false},
		{"from:alex subject:hintfixture", 0, 0, true, false},
		{"from:alex subject:hintfixture", 10, 10, true, false},
		{"from:alex3@example.com subject:hintfixture", 10, 0, true, false},
		{"from:@example.com", 10, 0, true, false},
		{"from:alex from:kim", 10, 0, true, false},
		{"from:alex | from:kim", 10, 0, true, false},
		{"from:alex subject:absent", 10, 0, true, false},
	} {
		t.Run(fmt.Sprintf("%s/%d/%d/%t", tc.q, tc.limit, tc.offset, tc.opt), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			result, err := env.Engine.SearchFastWithStats(env.Ctx, search.ParseOR(tc.q), tc.q, MessageFilter{IncludeSenderNameHint: tc.opt}, ViewNoStats, tc.limit, tc.offset)
			requirements.NoError(err)
			assertions.Equal(tc.want, result.SenderNameHint != nil)
		})
	}
	source := int64(99)
	hint = env.Engine.MatchSenderName(env.Ctx, q, MessageFilter{SourceID: &source})
	assertions.Nil(hint)
}

func TestSenderNameHintLiteralAndDeletionScope(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	p := env.AddParticipant(dbtest.ParticipantOpts{Email: new("special@example.com"), DisplayName: new("Alex_% Kim")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: p, Subject: "hintfixture"})
	other := env.AddParticipant(dbtest.ParticipantOpts{Email: new("other@example.com"), DisplayName: new("Alex Other")})
	env.AddMessage(dbtest.MessageOpts{FromID: other, Subject: "hintfixture"})
	q := search.Parse("from:alex_% subject:hintfixture")
	hint := env.Engine.MatchSenderName(env.Ctx, q, MessageFilter{})
	requirements.NotNil(hint)
	assertions.Equal("from:special@example.com subject:hintfixture", hint.Query)
	env.MarkDeletedByID(id)
	q.DeletionScope = search.DeletionScopeActive
	hint = env.Engine.MatchSenderName(env.Ctx, q, MessageFilter{})
	assertions.Nil(hint)
	q.DeletionScope = search.DeletionScopeAny
	hint = env.Engine.MatchSenderName(env.Ctx, q, MessageFilter{})
	requirements.NotNil(hint)
}

func TestSenderNameHintMatchesPartialAddress(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("alex.k@example.com"), DisplayName: new("Jordan Lee")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture"})
	hint := env.Engine.MatchSenderName(env.Ctx, search.Parse("from:alex.k subject:hintfixture"), MessageFilter{})
	requirements.NotNil(hint)
	assertions.Equal("from:alex.k@example.com subject:hintfixture", hint.Query)
	results, err := env.Engine.Search(env.Ctx, search.Parse(hint.Query), 10, 0)
	requirements.NoError(err)
	requirements.Len(results, 1)
	assertions.Equal(id, results[0].ID)
}
