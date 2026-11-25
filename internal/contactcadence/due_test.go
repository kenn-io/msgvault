package contactcadence_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/contactcadence"
)

func TestDueWithinUTCDateHorizon(t *testing.T) {
	assert := assert.New(t)
	evaluatedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.FixedZone("source", -7*60*60))

	assert.True(contactcadence.DueWithinUTCDateHorizon(
		time.Date(2026, 8, 20, 23, 59, 59, 0, time.UTC), evaluatedAt, 0,
	))
	assert.False(contactcadence.DueWithinUTCDateHorizon(
		time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC), evaluatedAt, 0,
	))
	assert.True(contactcadence.DueWithinUTCDateHorizon(
		time.Date(2026, 8, 21, 23, 59, 59, 0, time.UTC), evaluatedAt, 1,
	))
	assert.False(contactcadence.DueWithinUTCDateHorizon(
		time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), evaluatedAt, 1,
	))
}
