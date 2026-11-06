package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestPostgresWrapperForwardsSenderNameHint(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("alex@example.com"), DisplayName: new("Alex Kim")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "wrapped"})
	// Use the real SQL matcher through the PostgreSQL capability wrapper.
	var engine Engine = &pgEngine{Engine: env.Engine}
	matcher, ok := engine.(SenderNameMatcher)
	requirements.True(ok)
	q := search.Parse("from:alex subject:wrapped")
	hint := matcher.MatchSenderName(env.Ctx, q)
	requirements.NotNil(hint)
	assertions.Equal("from:alex@example.com subject:wrapped", hint.RetryWith.Query)
	messages, err := engine.Search(env.Ctx, search.Parse(hint.RetryWith.Query), 10, 0)
	requirements.NoError(err)
	requirements.Len(messages, 1)
	assertions.Equal(id, messages[0].ID)
	q.AccountIDs = []int64{99}
	assertions.Nil(matcher.MatchSenderName(env.Ctx, q))
}
