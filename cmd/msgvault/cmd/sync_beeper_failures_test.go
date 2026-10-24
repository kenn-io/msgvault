package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperSyncAccountsListsPartialFailuresAndContinues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	var out bytes.Buffer
	var attempted []string
	errs := runBeeperSyncAccounts(t.Context(), &out, st, []string{"signal", "fatal", "clean"},
		func(_ context.Context, account string) (*beeper.ImportSummary, error) {
			attempted = append(attempted, account)
			switch account {
			case "signal":
				return &beeper.ImportSummary{SourceID: src.ID, ChatsProcessed: 2, Errors: 6, FailedChats: map[string]*beeper.ChatFailure{
					"!club:example.test": {FetchErrors: 5, MediaErrors: 1},
				}}, &beeper.PartialSyncError{}
			case "fatal":
				return nil, errors.New("account unavailable")
			default:
				return &beeper.ImportSummary{}, nil
			}
		})
	assert.Equal([]string{"signal", "fatal", "clean"}, attempted)
	require.Len(errs, 2)
	assert.Contains(errs[0], "signal: partial Beeper sync")
	assert.Equal("fatal: account unavailable", errs[1])
	text := out.String()
	assert.Contains(text, "signal done in 0s: 2 chats, 0 messages (0 added), 6 errors\n")
	assert.Contains(text, "Failed chats (")
	assert.Contains(text, "Retry with the same global options (such as --local):")
	assert.Contains(text, "fetch errors: msgvault sync-beeper --account signal")
	assert.Contains(text, "attachment errors: msgvault backfill-beeper-media --account signal")
	assert.Contains(text, "clean done in")
}

func TestWriteBeeperRunFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	_, err = st.EnsureConversation(src.ID, "!top\n:example.test", "Club\x1b[31m\nTitle")
	require.NoError(err)
	sum := &beeper.ImportSummary{SourceID: src.ID, Errors: 25, FailedChats: map[string]*beeper.ChatFailure{
		"!top\n:example.test": {MediaErrors: 3, LastError: "bad\r\n" + strings.Repeat("界", 170)},
	}}
	for i := range 21 {
		sum.FailedChats[fmt.Sprintf("!%02d:example.test", i)] = &beeper.ChatFailure{MediaErrors: 1, LastError: "failed"}
	}
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	var out bytes.Buffer
	writeBeeperRunFailures(t.Context(), &out, st, sum, "signal\n\x1b[0m", "backfill-beeper-media")
	var query struct {
		NArgs int `json:"nargs"`
	}
	require.NoError(json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &query))
	assert.Equal(21, query.NArgs, "look up only the 20 displayed chats, plus the source ID")
	text := out.String()
	assert.Contains(text, "Failed chats (24 errors in 22 chats):")
	assert.Contains(text, "Club Title (!top :example.test): 3 attachment errors; last: bad  "+strings.Repeat("界", 152)+"...")
	assert.NotContains(text, strings.Repeat("界", 153))
	assert.NotContains(text, "\x1b")
	assert.Contains(text, "    !18:example.test: 1 attachment error; last: failed\n")
	assert.NotContains(text, "!19:example.test:")
	assert.Contains(text, "...and 2 more chats")
	assert.Contains(text, "  1 other error isn't listed by chat; re-run to retry.\n")
	assert.NotContains(text, "fetch errors:")
	assert.Contains(text, "attachment errors: msgvault backfill-beeper-media --account signal \n")

	out.Reset()
	writeBeeperRunFailures(t.Context(), &out, st, &beeper.ImportSummary{Errors: 4}, "signal", "sync-beeper")
	assert.Equal("  4 other errors aren't listed by chat; re-run to retry.\n", out.String())
	out.Reset()
	writeBeeperRunFailures(t.Context(), &out, st, &beeper.ImportSummary{}, "signal", "sync-beeper")
	assert.Empty(out.String())

	// A canceled lookup falls back to IDs while retaining counts and retry advice.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	writeBeeperRunFailures(ctx, &out, st, sum, "signal", "backfill-beeper-media")
	assert.Contains(out.String(), "!top :example.test: 3 attachment errors")
	assert.NotContains(out.String(), "Club Title")

	sum.FailedChats["!top\n:example.test"].FetchErrors = 2
	sum.Errors += 2
	sum.FailedChats["!mixed:example.test"] = &beeper.ChatFailure{MediaErrors: 1, FetchErrors: 1, LastError: "failed"}
	sum.Errors += 2
	out.Reset()
	writeBeeperRunFailures(t.Context(), &out, st, sum, "signal", "backfill-beeper-media")
	assert.Contains(out.String(), "    !mixed:example.test: 1 attachment, 1 fetch errors; last: failed\n")
	_, retry, found := strings.Cut(out.String(), "  Retry with the same global options (such as --local):\n")
	require.True(found)
	assert.Equal("    msgvault backfill-beeper-media --account signal\n", retry)
}
