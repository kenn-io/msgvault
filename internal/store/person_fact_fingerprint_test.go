package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactInvalidClaimSubmittedNumberReplayIsLossless(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	prepare := func(submitted string) personfacts.PreparedGeneration {
		claim := personFactLedgerClaim(personID, "large-integer", submitted, "large-integer")
		claim.Target.ValueType = personfacts.ValueInteger
		prepared := preparePersonFactLedgerGeneration(
			t, personID, "large-integer", []personfacts.ProposedClaim{claim}, nil)
		requirements.Len(prepared.Claims(), 1)
		requirements.NotNil(prepared.Claims()[0].Failure)
		return prepared
	}

	firstPrepared := prepare(`9223372036854775808000`)
	first := persistPersonFactLedgerGeneration(t, st, firstPrepared, nil)

	equivalentPrepared := prepare(`9.223372036854775808e21`)
	assertions.Equal(firstPrepared.GenerationKey(), equivalentPrepared.GenerationKey())
	equivalent := persistPersonFactLedgerGeneration(t, st, equivalentPrepared, nil)
	assertions.Equal(first.Generation.ID, equivalent.Generation.ID,
		"equivalent submitted number must replay the generation")

	distinctPrepared := prepare(`9223372036854775808001`)
	assertions.NotEqual(firstPrepared.GenerationKey(), distinctPrepared.GenerationKey())
	distinct := persistPersonFactLedgerGeneration(t, st, distinctPrepared, nil)
	assertions.NotEqual(first.Generation.ID, distinct.Generation.ID,
		"distinct submitted number must create a generation")
}
