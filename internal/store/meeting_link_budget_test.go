package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestLinkBudgetReservesBoundedRecurrenceWork(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	start := time.Date(2027, 3, 7, 10, 0, 0, 0, time.UTC)
	set, err := calrecur.ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=WEEKLY"})
	requirements.NoError(err)
	budget := 100
	allowance, err := reserveLinkRecurrence(context.Background(), set, start.AddDate(0, 0, 8), &budget)
	requirements.NoError(err)
	assertions.Equal(12, allowance)
	assertions.Equal(88, budget)
	_, err = calrecur.Expand(context.Background(), set, time.Hour, 0, nil, start, start.AddDate(0, 0, 8), allowance)
	assertions.NoError(err)
}

func TestLinkBudgetCapsOldSeriesAndHonorsCancellation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	start := time.Date(1800, 1, 1, 0, 0, 0, 0, time.UTC)
	set, err := calrecur.ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=DAILY"})
	requirements.NoError(err)
	budget := calrecur.RequestReadBudget
	allowance, err := reserveLinkRecurrence(context.Background(), set, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), &budget)
	requirements.NoError(err)
	assertions.Equal(calrecur.SeriesReadBudget, allowance)
	err = calrecur.Iter(context.Background(), set, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), allowance, func(calrecur.Instance) bool { return true })
	requirements.ErrorIs(err, calrecur.ErrBudgetExceeded)
	budget = 0
	_, err = reserveLinkRecurrence(context.Background(), set, start, &budget)
	requirements.ErrorIs(err, ErrQueryTooBroad)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget = calrecur.RequestReadBudget
	_, err = reserveLinkRecurrence(ctx, set, start, &budget)
	requirements.ErrorIs(err, context.Canceled)
}

func TestLinkBudgetAllowsMovedExceptionOfFinishedMaster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	start := time.Date(1800, 1, 1, 10, 0, 0, 0, time.UTC)
	set, err := calrecur.ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=DAILY;COUNT=6"})
	requirements.NoError(err)
	moved := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	end := moved.Add(time.Hour)
	budget := 50
	allowance, err := reserveLinkRecurrence(context.Background(), set, end.Add(24*time.Hour), &budget)
	requirements.NoError(err)
	assertions.Equal(7, allowance)
	occurrences, err := calrecur.Expand(context.Background(), set, time.Hour, 0, []calrecur.Exception{{Original: calrecur.Identity{Instant: start}, Start: &moved, End: &end}}, moved.Add(-time.Hour), end.Add(time.Hour), allowance)
	requirements.NoError(err)
	requirements.Len(occurrences, 1)
	assertions.Equal(moved, occurrences[0].Start)
}
