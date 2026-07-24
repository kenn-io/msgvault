package query

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type mixedArchiveBrowserFixture struct {
	RawChatMessageCount   int64            `json:"rawChatMessageCount"`
	ChatConversationCount int              `json:"chatConversationCount"`
	LogicalRows           []EntryRow       `json:"logicalRows"`
	FirstPage             *ExploreResponse `json:"firstPage"`
}

func TestWriteMixedArchiveBrowserFixture(t *testing.T) {
	require := require.New(t)
	fixture := buildMixedArchiveBrowserFixture(t)
	require.Equal(int64(100_000), fixture.RawChatMessageCount)
	require.Equal(100, fixture.ChatConversationCount)
	require.Len(fixture.FirstPage.Rows, 50)
	require.Equal(int64(len(fixture.LogicalRows)), fixture.FirstPage.TotalCount)

	kinds := make(map[EntryKind]int)
	for _, row := range fixture.LogicalRows {
		kinds[row.Kind]++
	}
	require.Equal(100, kinds[EntryConversation])
	require.Positive(kinds[EntryEmail])
	require.Positive(kinds[EntryEvent])
	require.Positive(kinds[EntryMeeting])
	require.Positive(kinds[EntryItem])

	if output := os.Getenv("MSGVAULT_MIXED_ARCHIVE_FIXTURE"); output != "" {
		require.NoError(writeMixedArchiveBrowserFixture(output, fixture))
	}
}

func buildMixedArchiveBrowserFixture(t *testing.T) mixedArchiveBrowserFixture {
	t.Helper()
	engine := buildBenchData(t)
	all, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 500}})
	require.NoError(t, err)
	first, err := engine.Explore(context.Background(), ExploreRequest{Page: PageSpec{Limit: 50}})
	require.NoError(t, err)
	return mixedArchiveBrowserFixture{
		RawChatMessageCount: benchChatMessageCount, ChatConversationCount: 100,
		LogicalRows: all.Rows, FirstPage: first,
	}
}

func writeMixedArchiveBrowserFixture(path string, fixture mixedArchiveBrowserFixture) error {
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}
