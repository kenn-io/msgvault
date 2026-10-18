package discord

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPDiscordNativeLiveCoverage(t *testing.T) {
	for _, initiallyEmpty := range []bool{false, true} {
		name := "history"
		if initiallyEmpty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			enableNativeDiscordEvents(t, st)
			api := newImporterFakeAPI(importerTestChannel("300", "Synthetic channel"))
			if !initiallyEmpty {
				api.messages["300"] = []Message{importerTestMessage("501", "300", "Synthetic history")}
			}
			opts := ImportOptions{GuildID: "200"}
			_, err := newTestImporter(st, api).Import(t.Context(), opts)
			require.NoError(err)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events, "first successful coverage is historical")
			api.messages["300"] = append(api.messages["300"], importerTestMessage("502", "300", "Synthetic live arrival"))
			_, err = newTestImporter(st, api).Import(t.Context(), opts)
			require.NoError(err)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(1, events, "completed coverage permits one later arrival")
			opts.Full = true
			api.messages["300"] = append(api.messages["300"], importerTestMessage("503", "300", "Synthetic full history"))
			_, err = newTestImporter(st, api).Import(t.Context(), opts)
			require.NoError(err)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(1, events, "explicit full import stays silent")
		})
	}
}

func TestMCPDiscordInterruptedFullCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	api := newImporterFakeAPI(importerTestChannel("300", "Synthetic channel"))
	api.messages["300"] = []Message{importerTestMessage("501", "300", "Synthetic baseline")}
	opts := ImportOptions{GuildID: "200"}
	_, err := newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	api.messages["300"] = append(api.messages["300"], importerTestMessage("502", "300", "Synthetic pending history"))
	api.messageHook = func(_ string, _ MessageQuery) ([]Message, error, bool) {
		return nil, errors.New("synthetic interruption"), true
	}
	opts.Full = true
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.Error(err)
	api.messageHook = nil
	opts.Full = false
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events, "ordinary resume must not relabel an interrupted full import")
	api.messages["300"] = append(api.messages["300"], importerTestMessage("503", "300", "Synthetic new arrival"))
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, events)
}

func TestMCPDiscordLegacyCoverageIsConservative(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	state := NewSyncState()
	state.Containers["300"] = ContainerState{HighWater: "501", BackfillComplete: true}
	blob, err := state.Marshal()
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, blob))
	api := newImporterFakeAPI(importerTestChannel("300", "Synthetic channel"))
	api.messages["300"] = []Message{importerTestMessage("502", "300", "Synthetic legacy gap")}
	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}

func TestMCPDiscordInterruptedLiveCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	api := newImporterFakeAPI(importerTestChannel("300", "Synthetic channel"))
	api.messages["300"] = []Message{importerTestMessage("501", "300", "Synthetic baseline")}
	opts := ImportOptions{GuildID: "200"}
	_, err := newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	for _, id := range []string{"502", "503", "504"} {
		api.messages["300"] = append(api.messages["300"], importerTestMessage(id, "300", "Synthetic arrival"))
	}
	api.messageHook = func(_ string, q MessageQuery) ([]Message, error, bool) {
		if q.After == "503" {
			return nil, errors.New("synthetic interruption"), true
		}
		return nil, nil, false
	}
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.Error(err)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(2, events)
	api.messageHook = nil
	api.messages["300"] = append(api.messages["300"], importerTestMessage("505", "300", "Synthetic later arrival"))
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(4, events, "live checkpoint resumes without silencing remaining arrivals")
}

func TestMCPDiscordNewContainerDoesNotBorrowCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	api := newImporterFakeAPI(importerTestChannel("300", "Synthetic covered channel"))
	opts := ImportOptions{GuildID: "200"}
	_, err := newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	api.channels = append(api.channels, importerTestChannel("301", "Synthetic new channel"))
	api.messages["300"] = []Message{importerTestMessage("502", "300", "Synthetic arrival")}
	api.messages["301"] = []Message{importerTestMessage("501", "301", "Synthetic old history")}
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, events)
	api.messages["301"] = append(api.messages["301"], importerTestMessage("503", "301", "Synthetic arrival"))
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(2, events)
}

func TestMCPDiscordChangedWindowDoesNotBorrowCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	api := newImporterFakeAPI(importerTestChannel("300", "Synthetic channel"))
	lower := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	messageID, err := SnowflakeFromTimestamp(lower.Add(48 * time.Hour))
	require.NoError(err)
	api.messages["300"] = []Message{importerTestMessage(messageID, "300", "Synthetic history")}
	opts := ImportOptions{GuildID: "200", After: lower}
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	messageID, err = SnowflakeFromTimestamp(lower.Add(72 * time.Hour))
	require.NoError(err)
	api.messages["300"] = append(api.messages["300"], importerTestMessage(messageID, "300", "Synthetic changed-window gap"))
	opts.After = lower.Add(time.Hour)
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
	messageID, err = SnowflakeFromTimestamp(lower.Add(96 * time.Hour))
	require.NoError(err)
	api.messages["300"] = append(api.messages["300"], importerTestMessage(messageID, "300", "Synthetic later arrival"))
	_, err = newTestImporter(st, api).Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, events)
}

func TestMCPDiscordArrivalBetweenForwardAndRepair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeDiscordEvents(t, st)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	baseline := importerTestSnowflake(t, now.Add(-2*time.Hour), 1)
	repaired := importerTestSnowflake(t, now.Add(-3*time.Hour), 2)
	arrival := importerTestSnowflake(t, now.Add(-time.Hour), 3)
	api := newImporterFakeAPI(importerTestChannel("300", "Synthetic channel"))
	api.messages["300"] = []Message{importerTestMessage(baseline, "300", "Synthetic baseline")}
	imp := newTestImporter(st, api)
	imp.now = func() time.Time { return now }
	opts := ImportOptions{GuildID: "200", EditRescanWindow: 24 * time.Hour}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	forwarded, injected := false, false
	api.messageHook = func(_ string, q MessageQuery) ([]Message, error, bool) {
		if q.After == baseline {
			forwarded = true
		}
		if forwarded && !injected && q.Before == "" && q.After == "" && q.Limit == 1 {
			injected = true
			api.messages["300"] = append(api.messages["300"], importerTestMessage(arrival, "300", "Synthetic late arrival"), importerTestMessage(repaired, "300", "Synthetic recovered history"))
		}
		return nil, nil, false
	}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.True(injected)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events, "edit/history recovery remains silent")
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{arrival, repaired})
	require.NoError(err)
	assert.NotZero(ids[repaired], "older history is still repaired")
	assert.Zero(ids[arrival], "repair cannot consume an arrival beyond the completed forward range")
	api.messageHook = nil
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, events, "the next forward fetch emits the late arrival exactly once")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, events)
}
