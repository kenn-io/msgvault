package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/dedup"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCallsDiscordThroughDaemon(t *testing.T) {
	for _, exportFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(exportFirst), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "discord")
			require.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(peer)
			require.NoError(err)
			source, err := st.GetOrCreateSource("discord", "example-guild")
			require.NoError(err)
			conversation, err := st.EnsureConversation(source.ID, "200", "Example")
			require.NoError(err)
			archived, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "100"})
			require.NoError(err)
			const native = `{"id":"100","channel_id":"200","type":3,"timestamp":"2026-10-01T00:00:00Z","call":{"participants":["1","2"]}}`
			require.NoError(st.UpsertMessageRawWithFormat(archived, []byte(native), "discord_json"))
			cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-discord-calls-key"}}
			adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
			server := httptest.NewServer(api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router())
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
			require.NoError(err)
			defer func() { _ = client.Close() }()
			run := func(input string, args ...string) (string, error) {
				command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
				var output bytes.Buffer
				command.SetOut(&output)
				command.SetErr(&output)
				command.SetIn(strings.NewReader(input))
				command.SetArgs(args)
				err := command.ExecuteContext(t.Context())
				return output.String(), err
			}
			requests := []string{
				fmt.Sprintf(`{"format":"discord_archived","account":"1","record":{"message_id":%d},"bindings":{"2":%d}}`, archived, peer),
				fmt.Sprintf(`{"format":"discord","account":"1","record":%s,"bindings":{"2":%d}}`, native, peer),
				`{"format":"discord_chat_exporter","account":"1","record":{"id":"100","type":"Call","timestamp":"2026-10-01T00:00:00Z","callEndedTimestamp":null}}`,
			}
			if exportFirst {
				requests = []string{requests[2], requests[0], requests[1]}
			}
			for i, input := range requests {
				for range 2 {
					output, err := run(input, "import", "-")
					require.NoError(err, output)
				}
				if strings.Contains(input, `"format":"discord_archived"`) {
					page, err := client.ListCallObservations(t.Context(), store.CallQuery{Limit: 50})
					require.NoError(err)
					var archivedCall *store.CallObservationView
					for j := range page.Items {
						if page.Items[j].RecordID == "native/100" {
							archivedCall = &page.Items[j]
							break
						}
					}
					require.NotNil(archivedCall)
					assert.Equal([]callhistory.Participant{{ID: peer, Participation: "joined"}}, archivedCall.Participants)
					before, err := client.SummarizeCalls(t.Context(), store.CallQuery{PersonID: person.ID, Limit: 50})
					require.NoError(err)
					require.Len(before.Items, 1)
					assert.Equal(1, before.Items[0].Unknown)
					assert.Zero(before.Items[0].Answered)
					engine := dedup.NewEngine(st, dedup.Config{
						ContentHashFallback: true,
						AccountSourceIDs:    []int64{source.ID, archivedCall.SourceID},
						Account:             "example",
						ScopeIsCollection:   true,
					}, slog.New(slog.DiscardHandler))
					report, err := engine.Scan(t.Context())
					require.NoError(err)
					execution, err := engine.Execute(t.Context(), report, fmt.Sprintf("discord-call-dedup-%t", exportFirst))
					require.NoError(err)
					assert.Zero(execution.GroupsMerged)
					after, err := client.SummarizeCalls(t.Context(), store.CallQuery{PersonID: person.ID, Limit: 50})
					require.NoError(err)
					assert.Equal(before.Items, after.Items, "content-hash dedup preserves call summaries")
				}
				if i == 0 && exportFirst {
					result, err := client.SummarizeCalls(t.Context(), store.CallQuery{Limit: 50})
					require.NoError(err)
					assert.Empty(result.Items)
				}
			}
			page, err := client.ListCallObservations(t.Context(), store.CallQuery{Limit: 50})
			require.NoError(err)
			require.Len(page.Items, 2)
			for _, item := range page.Items {
				raw, err := st.GetMessageRaw(item.MessageID)
				require.NoError(err)
				if item.RecordID == "native/100" {
					assert.JSONEq(native, string(raw))
					assert.Equal([]callhistory.Participant{{ID: peer, Participation: "joined"}}, item.Participants)
				}
			}
			summary, err := client.SummarizeCalls(t.Context(), store.CallQuery{PersonID: person.ID, Limit: 50})
			require.NoError(err)
			require.Len(summary.Items, 1)
			assert.Equal(1, summary.Items[0].Unknown)
			assert.Zero(summary.Items[0].Answered)
			_, err = run(`{"format":"discord_archived","account":"1","record":{"message_id":9223372036854775807}}`, "import", "-")
			require.Error(err)
			_, err = run(`{"format":"discord_archived","account":"1","record":{"message_id":"100"}}`, "import", "-")
			require.ErrorIs(err, callhistory.ErrInvalid)
			after, err := client.ListCallObservations(t.Context(), store.CallQuery{Limit: 50})
			require.NoError(err)
			assert.Len(after.Items, 2)
		})
	}
}
