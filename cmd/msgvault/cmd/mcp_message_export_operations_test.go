package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Execute the real export Cobra handler and Store producer. Only subprocess
// transport and opening the isolated fixture Store replace the installed CLI.
type inProcessMessageExportDaemonStore struct {
	*storeAPIAdapter

	mu       sync.Mutex
	requests []api.CLIRunRequest
	outputs  []string
}

func (s *inProcessMessageExportDaemonStore) RunCLICommand(ctx context.Context, request api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	root := &cobra.Command{Use: "msgvault", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) { return s.store, func() {}, nil }}))
	root.SetArgs(request.Args)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(ctx)
	s.mu.Lock()
	s.outputs = append(s.outputs, stdout.String())
	s.mu.Unlock()
	for _, event := range []api.CLIRunEvent{{Type: cliStreamStdout, Data: stdout.String()}, {Type: cliStreamStderr, Data: stderr.String()}} {
		if event.Data != "" {
			if emitErr := emit(event); emitErr != nil {
				return emitErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("execute native message export: %w", err)
	}
	return nil
}
func (s *inProcessMessageExportDaemonStore) exportRequests() []api.CLIRunRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

type observedMessageExportGate struct {
	*api.SerialOperationGate

	entries atomic.Int64
}

func (g *observedMessageExportGate) BeginRequestWorkContext(ctx context.Context, label string) (func(), bool) {
	g.entries.Add(1)
	return g.SerialOperationGate.BeginRequestWorkContext(ctx, label)
}

func TestMCPPersonMessageExportUsesNativePersonWindowAndProvenance(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStore(t)
	first, err := st.EnsureParticipant("one@example.test", "Synthetic Observed Sender", "example.test")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("two@example.test", "Synthetic Observed Alias", "example.test")
	requirements.NoError(err)
	outsider, err := st.EnsureParticipant("unrelated@example.test", "Synthetic Unrelated Sender", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(first)
	requirements.NoError(err)
	_, err = st.LinkParticipants(first, second)
	requirements.NoError(err)
	person, err = st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Len(person.ParticipantIDs, 2)
	name := "Curated Synthetic Sender"
	person, err = st.UpdatePersonDisplayName(person.ID, person.Revision, &name)
	requirements.NoError(err)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, item := range []struct {
		kind, identifier, provider, typ, body string
		sender                                int64
		at                                    time.Time
		deleted                               bool
	}{
		{"gmail", "archive@example.test", "synthetic-mail", "email", "Synthetic mail text", first, start, false},
		{"discord", "synthetic-guild", "synthetic-chat", "discord", "Synthetic chat text", second, start.Add(time.Minute), true},
		{"gmail", "archive@example.test", "synthetic-before", "email", "Outside lower bound", first, start.Add(-time.Nanosecond), false},
		{"discord", "synthetic-guild", "synthetic-end", "discord", "Outside upper bound", second, end, false},
		{"imap", "unrelated-archive@example.test", "synthetic-unrelated", "email", "Unrelated person", outsider, start, false},
	} {
		source, err := st.GetOrCreateSource(item.kind, item.identifier)
		requirements.NoError(err)
		conversationType := "email_thread"
		if item.kind == "discord" {
			conversationType = "channel"
		}
		conversation, err := st.EnsureConversationWithType(source.ID, "synthetic-thread", conversationType, "Synthetic conversation")
		requirements.NoError(err)
		if item.kind == "discord" {
			requirements.NoError(st.SetConversationMetadata(conversation, sql.NullString{String: `{"discord_channel_type":11,"parent_channel_id":"synthetic-parent"}`, Valid: true}))
		}
		recipients := []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{first, second}}}
		if item.sender == outsider {
			recipients = []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{outsider}}}
		}
		_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: item.provider, MessageType: item.typ, SenderID: sql.NullInt64{Int64: item.sender, Valid: true}, SentAt: sql.NullTime{Time: item.at, Valid: true}}, BodyText: sql.NullString{String: item.body, Valid: true}, Recipients: recipients})
		requirements.NoError(err)
		if item.deleted {
			requirements.NoError(st.MarkMessageDeleted(source.ID, item.provider))
		}
	}
	adapter := &inProcessMessageExportDaemonStore{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg, mcpCommands: registeredMCPCommandDescriptors()}}
	gate := &observedMessageExportGate{SerialOperationGate: api.NewSerialOperationGate()}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	requirements.Contains(backend.capabilities(), "export_person_messages")
	session := operationMCPSession(t, backend, nil, nil)
	args := map[string]any{"person_id": person.ID, "start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)}
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "export_person_messages", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var result struct {
		Complete bool             `json:"complete"`
		Records  []jsontext.Value `json:"records"`
	}
	requirements.NoError(json.Unmarshal(data, &result))
	assertions.True(result.Complete)
	requirements.Len(result.Records, 8)
	messages := map[string]exportMessagesMessageRecord{}
	for _, raw := range result.Records {
		var tag struct {
			RecordType string `json:"record_type"`
		}
		requirements.NoError(json.Unmarshal(raw, &tag))
		if tag.RecordType == "message" {
			var message exportMessagesMessageRecord
			requirements.NoError(json.Unmarshal(raw, &message))
			messages[message.ID] = message
		}
	}
	assertions.Len(messages, 2)
	assertions.Equal("Synthetic mail text", messages["synthetic-mail"].Text)
	assertions.Equal("Synthetic chat text", messages["synthetic-chat"].Text)
	assertions.True(messages["synthetic-chat"].DeletedFromSource)
	requirements.NotNil(messages["synthetic-mail"].Author)
	assertions.Equal(name, messages["synthetic-mail"].Author.DisplayName)
	assertions.Equal("one@example.test", messages["synthetic-mail"].Author.Address)
	assertions.Equal(int64(1), gate.entries.Load(), "native export retains daemon write admission")
	requests := adapter.exportRequests()
	requirements.Len(requests, 1)
	assertions.Empty(requests[0].Env)
	assertions.Empty(requests[0].Cwd)
	args["sources"] = []map[string]any{{"source_type": "gmail", "identifier": "archive@example.test"}}
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "export_person_messages", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	data, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(data, &result))
	assertions.True(result.Complete)
	assertions.Len(result.Records, 5)
}

func TestMCPPersonMessageExportEmptyBindingsAndBudgets(t *testing.T) {
	for _, item := range []struct {
		name          string
		size          int
		emptyBindings bool
		code          string
	}{
		{"empty bindings", 64, true, "message_export_incomplete"},
		{"empty window", 0, false, ""},
		{"structured budget", 600 << 10, false, "output_limit_exceeded"},
		{"wire budget", 1100 << 10, false, "output_limit_exceeded"},
	} {
		t.Run(item.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			st := testutil.NewSQLiteTestStore(t)
			participant, err := st.EnsureParticipant("scope@example.test", "Synthetic Export Scope", "example.test")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			if item.size > 0 {
				source, err := st.GetOrCreateSource("gmail", "budget@example.test")
				requirements.NoError(err)
				conversation, err := st.EnsureConversationWithType(source.ID, "synthetic-budget-thread", "email_thread", "Synthetic budget")
				requirements.NoError(err)
				_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "synthetic-budget-message", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: start, Valid: true}}, BodyText: sql.NullString{String: strings.Repeat("x", item.size), Valid: true}})
				requirements.NoError(err)
			}
			if item.emptyBindings {
				// A person without bindings is a reachable imported contact state. Retain
				// the archive row while removing its binding; the native resolver owns scope.
				_, err = st.DB().Exec("DELETE FROM person_participants WHERE person_id = ?", person.ID)
				requirements.NoError(err)
			}
			adapter := &inProcessMessageExportDaemonStore{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg, mcpCommands: registeredMCPCommandDescriptors()}}
			backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)}))
			args := map[string]any{"person_id": person.ID, "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339)}
			result, err := backend.ExecuteOperation(t.Context(), mcpPersonExportTool, args)
			requirements.NotNil(result)
			if item.code != "" {
				assertions.True(result.IsError)
				data, marshalErr := json.Marshal(result.Output)
				requirements.NoError(marshalErr)
				var failure struct {
					Error    string `json:"error"`
					Complete bool   `json:"complete"`
				}
				requirements.NoError(json.Unmarshal(data, &failure))
				assertions.Equal(item.code, failure.Error)
				assertions.False(failure.Complete)
			} else {
				requirements.NoError(err)
				requirements.False(result.IsError)
				data, marshalErr := json.Marshal(result.Output)
				requirements.NoError(marshalErr)
				var output struct {
					Complete bool             `json:"complete"`
					Records  []jsontext.Value `json:"records"`
				}
				requirements.NoError(json.Unmarshal(data, &output))
				assertions.True(output.Complete)
				assertions.Len(output.Records, 2, "empty scope never widens to the archive")
			}
			adapter.mu.Lock()
			raw := adapter.outputs[0]
			adapter.mu.Unlock()
			if item.size == 0 {
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				interrupted, interruptedErr := backend.ExecuteOperation(canceled, mcpPersonExportTool, args)
				requirements.ErrorIs(interruptedErr, context.Canceled)
				requirements.NotNil(interrupted)
				assertions.True(interrupted.IsError)
				missing := map[string]any{"person_id": int64(999), "start": args["start"], "end": args["end"]}
				absent, _ := backend.ExecuteOperation(t.Context(), mcpPersonExportTool, missing)
				requirements.NotNil(absent)
				assertions.True(absent.IsError)
				input, _, parseErr := mcpPersonExportArguments(args)
				requirements.NoError(parseErr)
				for _, wire := range []string{
					strings.Split(raw, "\n")[0] + "\n",       // Native completion missing.
					raw + strings.Split(raw, "\n")[0] + "\n", // Output after completion.
					strings.Replace(raw, `"messages":0`, `"messages":1`, 1),
					strings.Replace(raw, `,"messages":0`, "", 1),
					strings.Replace(raw, `"sources":0`, `"sources":null`, 1),
					strings.Replace(raw, `"message_types":[]`, `"message_types":null`, 1),
				} {
					_, parseErr := validateMCPPersonExport(wire, input)
					assertions.Error(parseErr, "%s", wire)
				}
			}
		})
	}
}

func TestMCPPersonMessageExportRejectsUnsafeArguments(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	valid := map[string]any{"person_id": int64(1), "start": "2026-09-01T00:00:00Z", "end": "2026-09-02T00:00:00Z"}
	for _, patch := range []map[string]any{
		{"person_id": int64(0)}, {"person_id": int64(1 << 53)}, {"person_id": nil},
		{"start": "yesterday"}, {"end": "2026-09-01T00:00:00Z"}, {"end": "2026-08-31T00:00:00Z"},
		{"args": []string{"--person-id=2"}}, {"cwd": "/tmp"}, {"destination": "/tmp/synthetic"}, {"env": map[string]string{}},
		{"message_types": []string{""}}, {"sources": []map[string]any{{"source_type": "gmail", "identifier": ""}}},
	} {
		args := map[string]any{}
		maps.Copy(args, valid)
		maps.Copy(args, patch)
		_, _, err := mcpPersonExportArguments(args)
		requirements.Error(err)
	}
	input, argv, err := mcpPersonExportArguments(map[string]any{"person_id": 1, "start": valid["start"], "end": valid["end"], "sources": []map[string]any{{"source_type": "gmail", "identifier": "--format=other:synthetic"}}})
	requirements.NoError(err)
	assertions.Len(input.Sources, 1)
	assertions.Contains(argv, "--source=gmail:--format=other:synthetic")
}

func TestMCPPersonMessageExportCapabilityRequiresExactOwnerFlags(t *testing.T) {
	descriptors := registeredMCPCommandDescriptors()
	require.Contains(t, messageExportMCPCapabilities(descriptors), mcpPersonExportTool)
	for _, descriptor := range descriptors {
		if descriptor.Name != "export-messages" {
			continue
		}
		for _, flag := range []string{"person-id", "start", "end", "format", "message-type", "source"} {
			copyDescriptor := descriptor
			copyDescriptor.Flags = slices.DeleteFunc(slices.Clone(descriptor.Flags), func(value string) bool { return value == flag })
			assert.Empty(t, messageExportMCPCapabilities([]apiprotocol.MCPCommandDescriptor{copyDescriptor}), flag)
		}
		descriptor.Delegated = true
		assert.Empty(t, messageExportMCPCapabilities([]apiprotocol.MCPCommandDescriptor{descriptor}))
	}
}

// Corruption and interrupted terminal events are transport-contract fixtures:
// execute the real daemon/Cobra/Store producer first, then change only its wire.
func TestMCPPersonMessageExportRequiresBothCompletionLayers(t *testing.T) {
	for _, kind := range []string{"missing export completion", "missing daemon completion", "daemon error after export completion", "output after daemon completion"} {
		t.Run(kind, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			st := testutil.NewSQLiteTestStore(t)
			participant, err := st.EnsureParticipant("terminal@example.test", "Synthetic Terminal", "example.test")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			adapter := &inProcessMessageExportDaemonStore{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg, mcpCommands: registeredMCPCommandDescriptors()}}
			owner := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
			router := owner.Router()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/cli/run" {
					router.ServeHTTP(w, r)
					return
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, r)
				w.WriteHeader(recorder.Code)
				decoder := jsontext.NewDecoder(strings.NewReader(recorder.Body.String()))
				for {
					var event api.CLIRunEvent
					decodeErr := json.UnmarshalDecode(decoder, &event)
					if errors.Is(decodeErr, io.EOF) {
						break
					}
					if !assertions.NoError(decodeErr) {
						return
					}
					if kind == "missing export completion" && event.Type == cliStreamStdout {
						lines := strings.Split(strings.TrimSpace(event.Data), "\n")
						event.Data = strings.Join(lines[:len(lines)-1], "\n") + "\n"
					}
					if event.Type == "complete" {
						if kind == "missing daemon completion" {
							continue
						}
						if kind == "daemon error after export completion" {
							event.Type = "error"
							event.Error = "synthetic private diagnostic /private/example"
						}
					}
					_ = json.MarshalWrite(w, event)
				}
				if kind == "output after daemon completion" {
					_ = json.MarshalWrite(w, api.CLIRunEvent{Type: cliStreamStdout, Data: "synthetic after terminal"})
				}
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(t.Context())
			requirements.NoError(err)
			backend := newDaemonMCPOperations(client, capabilities)
			session := operationMCPSession(t, backend, nil, nil)
			called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpPersonExportTool, Arguments: map[string]any{"person_id": person.ID, "start": "2026-09-01T00:00:00Z", "end": "2026-09-02T00:00:00Z"}})
			requirements.NoError(err)
			requirements.True(called.IsError)
			data, err := json.Marshal(called.StructuredContent)
			requirements.NoError(err)
			var result map[string]jsontext.Value
			requirements.NoError(json.Unmarshal(data, &result))
			assertions.Contains(result, "error")
			assertions.NotContains(result, "records")
			assertions.NotContains(result, "complete")
			assertions.NotContains(string(data), "/private/example")
			assertions.Len(adapter.exportRequests(), 1)
		})
	}
}
