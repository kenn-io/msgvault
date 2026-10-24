package calevent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/teambition/rrule-go"
)

func TestICSVerificationStopsOnCancellation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rule, err := rrule.NewRRule(rrule.ROption{Freq: rrule.DAILY, Count: 50000, Dtstart: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
	requirements.NoError(err)
	budget := NewICSParseBudget()
	next := rule.Iterator()
	_, ok, err := budget.nextVerification(ctx, next)
	requirements.NoError(err)
	requirements.True(ok)
	cancel()
	_, ok, err = budget.nextVerification(ctx, next)
	requirements.ErrorIs(err, context.Canceled)
	assertions.False(ok)
}
