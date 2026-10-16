package beeper

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunFailuresNamesFailedChats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	oldBackoff := fetchRetryBackoff
	fetchRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { fetchRetryBackoff = oldBackoff })

	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	healthy := budgetTestChat("!healthy:beeper.local", 3, base)
	flaky := budgetTestChat("!flaky:beeper.local", 3, base)
	flaky.Title = "Book Club"
	f.addChat(healthy)
	f.addChat(flaky)
	imp, _, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	require.NotZero(sum.SyncRunID)
	clean, err := imp.RunFailures(context.Background(), sum.SourceID, sum.SyncRunID, 100)
	require.NoError(err)
	assert.Zero(clean.Total)
	assert.Empty(clean.Chats)

	// An archived chat and a chat seen for the first time both fail; the
	// healthy chat keeps syncing.
	now := time.Now().UTC().Truncate(time.Second)
	f.appendMsg("!healthy:beeper.local", fakeMsg{ID: "h-new", SortKey: 10, Timestamp: now,
		Text: "new", SenderID: "@signal_ann:beeper.local", SenderName: "Ann"})
	f.appendMsg("!flaky:beeper.local", fakeMsg{ID: "f-new", SortKey: 10, Timestamp: now,
		Text: "missed", SenderID: "@signal_ann:beeper.local", SenderName: "Ann"})
	fresh := budgetTestChat("!fresh:beeper.local", 2, now.Add(-time.Hour))
	fresh.Title = ""
	f.addChat(fresh)
	f.setMessageListFailure("!flaky:beeper.local", true)
	f.setMessageListFailure("!fresh:beeper.local", true)

	sum, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	var partial *PartialSyncError
	require.ErrorAs(err, &partial)
	require.NotZero(sum.SyncRunID)

	failures, err := imp.RunFailures(context.Background(), sum.SourceID, sum.SyncRunID, 100)
	require.NoError(err)
	assert.Equal(sum.FetchErrors, failures.Total)
	assert.EqualValues(failures.Total, failures.Listed)

	byID := map[string]ChatFailure{}
	for _, cf := range failures.Chats {
		byID[cf.ChatID] = cf
	}
	require.Len(byID, 2, "only the failing chats are listed: %+v", failures.Chats)
	assert.NotContains(byID, "!healthy:beeper.local")

	book := byID["!flaky:beeper.local"]
	assert.Equal("Book Club", book.Title, "archived chats are named from the archive")
	assert.Positive(book.Errors)
	assert.Equal([]string{"fetch"}, book.Phases)
	assert.NotEmpty(book.LastError)

	newChat, ok := byID["!fresh:beeper.local"]
	require.True(ok, "a chat with no archived history is still listed by ID")
	assert.Empty(newChat.Title)
	assert.Positive(newChat.Errors)

	// Bounded listings report how much they grouped.
	limited, err := imp.RunFailures(context.Background(), sum.SourceID, sum.SyncRunID, 1)
	require.NoError(err)
	assert.Equal(failures.Total, limited.Total)
	assert.Equal(1, limited.Listed)
	assert.Len(limited.Chats, 1)
}
