package personscope_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
)

func TestEmptyPersonPopulationProducesFalsePredicate(t *testing.T) {
	scope := personscope.Scope{Directions: []personscope.Direction{personscope.FromPerson}}
	require.NoError(t, personscope.Validate(scope))

	predicate, args := personscope.MessagePredicate(scope, "m", "c")

	assert.Equal(t, "FALSE", predicate)
	assert.Empty(t, args)
}

func TestPersonPredicateRejectsMissingDirectionsWithoutInvalidSQL(t *testing.T) {
	scope := personscope.Scope{ParticipantIDs: []int64{4}}
	require.Error(t, personscope.Validate(scope))

	predicate, args := personscope.MessagePredicate(scope, "m", "c")

	assert.Equal(t, "FALSE", predicate)
	assert.Empty(t, args)
}
