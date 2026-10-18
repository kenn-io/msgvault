package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopApplyUsesMessageOrderForTerminalBoundary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	boundaryAt := base.Add(-48 * time.Hour)
	ids := []int64{
		loopMessageAt(t, f, boundaryAt, true),
		loopMessageAt(t, f, boundaryAt, true),
		loopMessageAt(t, f, boundaryAt, false),
		loopMessageAt(t, f, boundaryAt.Add(time.Hour), true),
	}

	done := loopOpenChronology(t, f, ids[0], ids[1], boundaryAt, boundaryAt)
	applyLoop(t, f, done, base)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	_, err = f.Store.CloseLoopContext(t.Context(), rows[0].ID, "handled", base)
	requirements.NoError(err)

	newerTurn := loopOpenChronology(t, f, ids[3], ids[3], boundaryAt.Add(time.Hour), boundaryAt.Add(time.Hour))
	newerTurn.PreviousTurnKnown = true
	newerTurn.PreviousTurnAt = boundaryAt
	applyLoop(t, f, newerTurn, base)

	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1, "a later opposing message at the same timestamp proves a new turn")
	assertions.Equal(ids[3], *rows[0].AnchorMessageID)
}
