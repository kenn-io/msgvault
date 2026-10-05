package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmationChallengeCrossesStatelessRequestsWithinAuthenticatedSession(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	confirmations := newConfirmationChallenges()
	arguments := map[string]any{"candidate_id": float64(7)}
	const sessionKey = "authenticated-session"
	token, err := confirmations.issue(nil, sessionKey, ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)

	asserts.True(confirmations.consume(token, nil, sessionKey, ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, sessionKey, ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"),
		"confirmation challenges must be one-time")
}

func TestConfirmationChallengeCannotCrossAuthenticatedSessionsOrActions(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	confirmations := newConfirmationChallenges()
	arguments := map[string]any{"candidate_id": float64(7)}
	token, err := confirmations.issue(nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)

	asserts.False(confirmations.consume(token, nil, "session-b", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"),
		"an attempted use from another session must consume the one-time challenge")
	token, err = confirmations.issue(nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)
	asserts.False(confirmations.consume(token, nil, "session-a", ToolRejectIdentityMatch, arguments, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
	token, err = confirmations.issue(nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?")
	requires.NoError(err)
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch,
		map[string]any{"candidate_id": float64(8)}, "Accept candidate 7?"))
	asserts.False(confirmations.consume(token, nil, "session-a", ToolAcceptIdentityMatch, arguments, "Accept candidate 7?"))
}
