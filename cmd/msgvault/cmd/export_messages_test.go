package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestExportMessagesCommandWritesContractOrderAndCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "archive")
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "Example Archive"))
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	conversationID := insertExportMessagesConversation(
		t, st, source.ID, "conversation", "Example Conversation",
	)
	insertExportMessagesMessage(
		t, st, source.ID, conversationID, "message", start, "Complete body <safe>&",
	)

	cmd := newExportMessagesLocalCmd(exportMessagesDeps{
		openStore: func() (*store.Store, func(), error) {
			return st, func() {}, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--start", "2026-07-19T20:00:00-04:00",
		"--end", "2026-07-21T00:00:00Z",
	})
	require.NoError(cmd.Execute())
	assert.NotContains(output.String(), `\u003c`)
	assert.NotContains(output.String(), `\u0026`)

	records := decodeExportMessageRecords(t, output.Bytes())
	require.Len(records, 5)
	assert.Equal([]string{
		"manifest", "source", "conversation", "message", "complete",
	}, exportMessageRecordTypes(records))

	assert.Equal(messageExportSchema, records[0]["schema"])
	assert.Equal(Version, records[0]["msgvault_version"])
	window, ok := records[0]["window"].(map[string]any)
	require.True(ok)
	assert.Equal("2026-07-20T00:00:00Z", window["start"])
	assert.Equal("2026-07-21T00:00:00Z", window["end"])

	assert.Equal("text", records[1]["source_type"])
	assert.Equal("archive", records[1]["identifier"])
	assert.Equal("Example Archive", records[1]["display_name"])
	assert.Nil(records[1]["last_successful_sync_at"])

	assert.Equal("conversation", records[2]["id"])
	assert.Equal("direct_chat", records[2]["conversation_type"])
	assert.Nil(records[2]["parent_id"])

	assert.Equal("message", records[3]["id"])
	assert.Equal("Complete body <safe>&", records[3]["text"])
	assert.Equal("2026-07-20T00:00:00Z", records[3]["occurred_at"])
	assert.Nil(records[3]["author"])

	counts, ok := records[4]["counts"].(map[string]any)
	require.True(ok)
	assert.InDelta(1, counts["sources"], 0)
	assert.InDelta(1, counts["conversations"], 0)
	assert.InDelta(1, counts["messages"], 0)
}

func TestExportMessagesCommandNormalizesFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("text", "alpha:one")
	require.NoError(err)
	_, err = st.GetOrCreateSource("fixture", "beta")
	require.NoError(err)

	cmd := newExportMessagesLocalCmd(exportMessagesDeps{
		openStore: func() (*store.Store, func(), error) {
			return st, func() {}, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--start", "2026-07-20T00:00:00Z",
		"--end", "2026-07-21T00:00:00Z",
		"--source", "text:alpha:one",
		"--source", "fixture:beta",
		"--source", "text:alpha:one",
		"--message-type", "sms",
		"--message-type", "email",
		"--message-type", "sms",
		"--format", "jsonl",
	})
	require.NoError(cmd.Execute())

	records := decodeExportMessageRecords(t, output.Bytes())
	manifestFilters, ok := records[0]["filters"].(map[string]any)
	require.True(ok)
	assert.Equal([]any{"email", "sms"}, manifestFilters["message_types"])
	assert.Equal([]any{
		map[string]any{"source_type": "fixture", "identifier": "beta"},
		map[string]any{"source_type": "text", "identifier": "alpha:one"},
	}, manifestFilters["sources"])
	assert.Equal([]string{"manifest", "source", "source", "complete"}, exportMessageRecordTypes(records))
}

func TestExportMessagesCommandRejectsPreflightErrorsWithoutOutput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "invalid bounds",
			args: []string{
				"--start", "2026-07-21T00:00:00Z",
				"--end", "2026-07-20T00:00:00Z",
			},
		},
		{
			name: "missing source",
			args: []string{
				"--start", "2026-07-20T00:00:00Z",
				"--end", "2026-07-21T00:00:00Z",
				"--source", "text:missing",
			},
		},
		{
			name: "invalid source selector",
			args: []string{
				"--start", "2026-07-20T00:00:00Z",
				"--end", "2026-07-21T00:00:00Z",
				"--source", "missing-separator",
			},
		},
		{
			name: "empty message type",
			args: []string{
				"--start", "2026-07-20T00:00:00Z",
				"--end", "2026-07-21T00:00:00Z",
				"--message-type", " ",
			},
		},
		{
			name: "unsupported format",
			args: []string{
				"--start", "2026-07-20T00:00:00Z",
				"--end", "2026-07-21T00:00:00Z",
				"--format", "json",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			cmd := newExportMessagesLocalCmd(exportMessagesDeps{
				openStore: func() (*store.Store, func(), error) {
					return st, func() {}, nil
				},
			})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			require.Error(cmd.Execute())
			require.Empty(output.String())
		})
	}
}

type failAfterFirstExportWrite struct {
	output bytes.Buffer
	writes int
}

func (w *failAfterFirstExportWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("synthetic writer failure")
	}
	_, _ = w.output.Write(p)
	return len(p), nil
}

func TestExportMessagesCommandOmitsCompletionAfterStreamFailure(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "archive")
	require.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	conversationID := insertExportMessagesConversation(
		t, st, source.ID, "conversation", "",
	)
	insertExportMessagesMessage(
		t, st, source.ID, conversationID, "message", start, "body",
	)

	cmd := newExportMessagesLocalCmd(exportMessagesDeps{
		openStore: func() (*store.Store, func(), error) {
			return st, func() {}, nil
		},
	})
	writer := &failAfterFirstExportWrite{}
	cmd.SetOut(writer)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--start", "2026-07-20T00:00:00Z",
		"--end", "2026-07-21T00:00:00Z",
	})
	require.Error(cmd.Execute())
	require.Contains(writer.output.String(), `"record_type":"manifest"`)
	require.NotContains(writer.output.String(), `"record_type":"complete"`)
}

func TestExportMessagesCommandRoutesThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(
		t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal([]string{
				"export-messages",
				"--end=2026-07-21T00:00:00Z",
				"--message-type=email",
				"--source=text:archive",
				"--start=2026-07-20T00:00:00Z",
			}, req.Args)
		},
		"{\"type\":\"stdout\",\"data\":\"{\\\"record_type\\\":\\\"complete\\\"}\\n\"}\n"+
			"{\"type\":\"complete\"}\n",
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	cmd := newExportMessagesCmd(exportMessagesDeps{})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--start", "2026-07-20T00:00:00Z",
		"--end", "2026-07-21T00:00:00Z",
		"--source", "text:archive",
		"--message-type", "email",
	})
	require.NoError(cmd.Execute())
	assert.Equal(1, int(requests.Load()))
	assert.JSONEq("{\"record_type\":\"complete\"}", output.String())
}

func decodeExportMessageRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	require := require.New(t)
	var records []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var record map[string]any
		require.NoError(json.Unmarshal(scanner.Bytes(), &record))
		records = append(records, record)
	}
	require.NoError(scanner.Err())
	return records
}

func exportMessageRecordTypes(records []map[string]any) []string {
	types := make([]string, len(records))
	for i, record := range records {
		types[i], _ = record["record_type"].(string)
	}
	return types
}

func insertExportMessagesConversation(
	t *testing.T,
	st *store.Store,
	sourceID int64,
	sourceConversationID, title string,
) int64 {
	t.Helper()
	require := require.New(t)
	var conversationID int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO conversations (
			source_id, source_conversation_id, conversation_type, title
		) VALUES (?, ?, 'direct_chat', ?)
		RETURNING id
	`), sourceID, sourceConversationID, title).Scan(&conversationID)
	require.NoError(err)
	return conversationID
}

func insertExportMessagesMessage(
	t *testing.T,
	st *store.Store,
	sourceID, conversationID int64,
	sourceMessageID string,
	occurredAt time.Time,
	body string,
) {
	t.Helper()
	require := require.New(t)
	var messageID int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO messages (
			conversation_id, source_id, source_message_id, message_type,
			sent_at, received_at
		) VALUES (?, ?, ?, 'text', ?, ?)
		RETURNING id
	`), conversationID, sourceID, sourceMessageID, occurredAt, occurredAt).
		Scan(&messageID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)
	`), messageID, body)
	require.NoError(err)
}
