package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestOccurrencesStandaloneRequestBudget(t *testing.T) {
	start := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	rows := []CalendarEventRow{
		{MessageID: 1, SourceID: 1, Event: calevent.Event{Kind: "single", HasBounds: true, Start: &start, End: &end}},
		{MessageID: 2, SourceID: 1, Event: calevent.Event{Kind: "single", HasBounds: true, Start: &start, End: &end}},
	}
	// Timed standalone rows need no additional SQL; exercise the production
	// expander with the budget supplied by its meeting-record caller.
	budget := 1
	items, _, err := expandRowsToOccurrences(t.Context(), nil, rows, start, end, &budget)
	require.ErrorIs(t, err, ErrQueryTooBroad)
	assert.Empty(t, items)
}
