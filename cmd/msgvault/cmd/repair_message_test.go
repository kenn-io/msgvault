package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	syncer "go.kenn.io/msgvault/internal/sync"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestRepairMessageCommandValidatesModeAndSourceScope(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing repair reference", want: "requires exactly one message reference unless --audit is set"},
		{name: "multiple repair references", args: []string{"one", "two"}, want: "requires exactly one message reference unless --audit is set"},
		{name: "audit rejects reference", args: []string{"--audit", "one"}, want: "--audit does not accept a message reference"},
		{name: "zero source", args: []string{"one", "--source-id", "0"}, want: "--source-id must be a positive integer"},
		{name: "negative source", args: []string{"--audit", "--source-id", "-1"}, want: "--source-id must be a positive integer"},
		{name: "repair rejects json", args: []string{"one", "--json"}, want: "--json requires --audit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened := false
			command := newRepairMessageCmd(repairMessageCommandDeps{
				openWritableStore: func() (*store.Store, func(), error) {
					opened = true
					return nil, func() {}, errors.New("must not open store")
				},
				openReadOnlyStore: func() (*store.Store, func(), error) {
					opened = true
					return nil, func() {}, errors.New("must not open store")
				},
			})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(test.args)

			err := command.Execute()
			require.ErrorContains(t, err, test.want)
			assert.False(t, opened)
			assert.False(t, command.SilenceUsage, "invocation errors keep usage visible")
		})
	}
}

func TestRepairMessageCommandTreatsHyphenReferenceAfterTerminatorAsRepair(t *testing.T) {
	storeErr := errors.New("writable store opened for repair")
	auditOpened := false
	command := newRepairMessageCmd(repairMessageCommandDeps{
		isDaemonSubprocess: func() bool { return true },
		openWritableStore: func() (*store.Store, func(), error) {
			return nil, func() {}, storeErr
		},
		openReadOnlyStore: func() (*store.Store, func(), error) {
			auditOpened = true
			return nil, func() {}, errors.New("must not audit")
		},
	})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--", "--audit"})

	require.ErrorIs(t, command.Execute(), storeErr)
	assert.False(t, auditOpened, "a reference spelled like a flag must not select audit mode")
}

func TestRepairMessageCommandRunsProductionRepairAndPrintsStableResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("gmail-42")
	var lifecycle []string
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: fixture.Source.Identifier}
	mock.AddMessage("gmail-42", testemail.NewMessage().
		From("alice@example.com").To("bob@example.com").
		Subject("Repaired subject").Body("Repaired body").Bytes(), []string{"INBOX"})
	command := newRepairMessageCmd(repairMessageCommandDeps{
		isDaemonSubprocess: func() bool { return true },
		openWritableStore: func() (*store.Store, func(), error) {
			return fixture.Store, func() { lifecycle = append(lifecycle, "store closed") }, nil
		},
		newGmailClient: func(context.Context, *store.Source) (gmail.API, error) {
			return mock, nil
		},
		refreshCache: func() error {
			lifecycle = append(lifecycle, "cache refreshed")
			return nil
		},
		attachmentsDir: filepath.Join(t.TempDir(), "attachments"),
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"gmail-42", "--source-id", "1"})

	require.NoError(command.Execute())
	assert.Equal(
		"Repaired message "+jsonNumber(messageID)+" (source 1, Gmail gmail-42): Repaired subject\n",
		output.String())
	assert.Equal([]string{"gmail-42"}, mock.GetMessageCalls)
	assert.Equal([]string{"store closed", "cache refreshed"}, lifecycle)
	var subject string
	require.NoError(fixture.Store.DB().QueryRow(
		fixture.Store.Rebind(`SELECT subject FROM messages WHERE id = ?`), messageID,
	).Scan(&subject))
	assert.Equal("Repaired subject", subject)
}

func TestRepairMessageCommandPropagatesCacheRefreshFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := storetest.New(t)
	fixture.CreateMessage("gmail-42")
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: fixture.Source.Identifier}
	mock.AddMessage("gmail-42", testemail.NewMessage().
		From("alice@example.com").To("bob@example.com").
		Subject("Repaired subject").Body("Repaired body").Bytes(), []string{"INBOX"})
	storeClosed := false
	cacheErr := errors.New("synthetic cache refresh failure")
	command := newRepairMessageCmd(repairMessageCommandDeps{
		isDaemonSubprocess: func() bool { return true },
		openWritableStore: func() (*store.Store, func(), error) {
			return fixture.Store, func() { storeClosed = true }, nil
		},
		newGmailClient: func(context.Context, *store.Source) (gmail.API, error) {
			return mock, nil
		},
		refreshCache: func() error {
			assert.True(storeClosed, "writable store must close before cache refresh")
			return cacheErr
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"gmail-42", "--source-id", "1"})

	require.ErrorIs(command.Execute(), cacheErr)
	assert.True(storeClosed)
	assert.NotContains(output.String(), "Repaired message", "do not report complete success before cache refresh")
}

func TestRepairMessageCommandUsesDedicatedRemoteEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantBody   generated.CLIRepairMessageRequest
		stdoutLine string
		stderrLine string
	}{
		{
			name: "repair",
			args: []string{"gmail-42", "--source-id", "7"},
			wantBody: generated.CLIRepairMessageRequest{
				Reference: new("gmail-42"),
				SourceID:  new(int64(7)),
			},
			stdoutLine: "repaired remote message\n",
			stderrLine: "repair warning\n",
		},
		{
			name: "audit JSON",
			args: []string{"--audit", "--source-id", "7", "--json"},
			wantBody: generated.CLIRepairMessageRequest{
				Audit:    new(true),
				JSON:     new(true),
				SourceID: new(int64(7)),
			},
			stdoutLine: `{"internal_id":42,"status":"mismatch"}` + "\n",
			stderrLine: "audit warning\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var paths []string
			var preflightCalls atomic.Int32
			var readOnlyCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/api/v1/health":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.15.0"}`))
				case "/api/v1/cli/repair-message":
					assert.Equal(http.MethodPost, r.Method)
					assert.Empty(r.URL.RawQuery)
					var got generated.CLIRepairMessageRequest
					if !assert.NoError(json.NewDecoder(r.Body).Decode(&got)) {
						return
					}
					assert.Equal(test.wantBody, got)
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = w.Write([]byte(`{"type":"stdout","data":` + string(marshalRepairCommandJSON(t, test.stdoutLine)) + `}` + "\n"))
					_, _ = w.Write([]byte(`{"type":"stderr","data":` + string(marshalRepairCommandJSON(t, test.stderrLine)) + `}` + "\n"))
					_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
				default:
					http.Error(w, "unexpected endpoint", http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			configureRemoteSyncTest(t, server.URL)
			deps := defaultRepairMessageCommandDeps()
			deps.preflightReauth = func(context.Context, *daemonclient.Client, HTTPStoreInfo, int64) error {
				preflightCalls.Add(1)
				return errors.New("configured remote must not run local OAuth preflight")
			}
			deps.openReadOnlyStore = func() (*store.Store, func(), error) {
				readOnlyCalls.Add(1)
				return nil, func() {}, errors.New("configured remote must not require the local archive")
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command := newRepairMessageCmd(deps)
			command.SetOut(&stdout)
			command.SetErr(&stderr)
			command.SetArgs(test.args)

			require.NoError(command.Execute())
			assert.Equal([]string{"/api/v1/health", "/api/v1/cli/repair-message"}, paths)
			assert.Equal(test.stdoutLine, stdout.String())
			assert.Equal(test.stderrLine, stderr.String())
			assert.Zero(preflightCalls.Load())
			assert.Zero(readOnlyCalls.Load())
		})
	}
}

func TestRepairMessageCommandPreflightsOAuthForSelectedLocalSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var sequence atomic.Int32
	var capabilityStep atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			capabilityStep.Store(sequence.Add(1))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.15.0"}`))
			return
		}
		http.Error(w, "repair must not start", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(err)

	var selectedSourceID int64
	var preflightStep atomic.Int32
	command := newRepairMessageCmd(repairMessageCommandDeps{
		isDaemonSubprocess: func() bool { return false },
		openHTTPStore: func(context.Context) (*daemonclient.Client, HTTPStoreInfo, error) {
			return client, HTTPStoreInfo{Kind: HTTPStoreLocalDaemon}, nil
		},
		preflightReauth: func(_ context.Context, _ *daemonclient.Client, _ HTTPStoreInfo, sourceID int64) error {
			preflightStep.Store(sequence.Add(1))
			selectedSourceID = sourceID
			return errors.New("reauth failed")
		},
	})
	command.SilenceUsage = true
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"gmail-42", "--source-id", "42"})

	err = command.Execute()
	require.EqualError(err, "reauth failed")
	assert.Equal(int64(42), selectedSourceID)
	assert.Equal(int32(1), capabilityStep.Load())
	assert.Equal(int32(2), preflightStep.Load())
	assert.True(command.SilenceUsage, "runtime preflight errors hide usage")
}

func TestRepairMessageCommandIncompatibleLocalDaemonFailsBeforeOAuthOrMutation(t *testing.T) {
	tests := []struct {
		name          string
		healthPayload string
	}{
		{name: "missing version", healthPayload: `{"status":"ok"}`},
		{name: "malformed version", healthPayload: `{"status":"ok","api_schema_version":"2.13"}`},
		{name: "older version", healthPayload: `{"status":"ok","api_schema_version":"2.13.0"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var nonHealthRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(test.healthPayload))
					return
				}
				nonHealthRequests.Add(1)
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{
				URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
			})
			require.NoError(err)

			var preflightCalls atomic.Int32
			command := newRepairMessageCmd(repairMessageCommandDeps{
				isDaemonSubprocess: func() bool { return false },
				openHTTPStore: func(context.Context) (*daemonclient.Client, HTTPStoreInfo, error) {
					return client, HTTPStoreInfo{Kind: HTTPStoreLocalDaemon}, nil
				},
				preflightReauth: func(context.Context, *daemonclient.Client, HTTPStoreInfo, int64) error {
					preflightCalls.Add(1)
					return nil
				},
			})
			command.SilenceUsage = true
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs([]string{"gmail-42", "--source-id", "42"})

			err = command.Execute()
			require.ErrorContains(err, "requires daemon API schema 2.15.0")
			assert.Zero(preflightCalls.Load())
			assert.Zero(nonHealthRequests.Load())
			assert.True(command.SilenceUsage, "runtime compatibility errors hide usage")
		})
	}
}

func TestRepairMessageCommandResolvesOmittedLocalSourceBeforeScopedOAuthPreflight(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	targetSource, err := fixture.Store.GetOrCreateSource("gmail", "target@example.com")
	require.NoError(err)
	targetConversation, err := fixture.Store.EnsureConversation(targetSource.ID, "target-thread", "")
	require.NoError(err)
	_, err = fixture.Store.UpsertMessage(&store.Message{
		ConversationID:  targetConversation,
		SourceID:        targetSource.ID,
		SourceMessageID: "gmail-target",
		MessageType:     "email",
	})
	require.NoError(err)

	var sequence atomic.Int32
	var capabilityStep atomic.Int32
	var repairStep atomic.Int32
	requestBody := make(chan generated.CLIRepairMessageRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			capabilityStep.Store(sequence.Add(1))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.15.0"}`))
		case "/api/v1/cli/repair-message":
			repairStep.Store(sequence.Add(1))
			var body generated.CLIRepairMessageRequest
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			requestBody <- body
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
		default:
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(err)

	var selectedSourceID atomic.Int64
	var preflightStep atomic.Int32
	readOnlyOpened := false
	command := newRepairMessageCmd(repairMessageCommandDeps{
		isDaemonSubprocess: func() bool { return false },
		openHTTPStore: func(context.Context) (*daemonclient.Client, HTTPStoreInfo, error) {
			return client, HTTPStoreInfo{Kind: HTTPStoreLocalDaemon}, nil
		},
		openReadOnlyStore: func() (*store.Store, func(), error) {
			readOnlyOpened = true
			return fixture.Store, func() {}, nil
		},
		preflightReauth: func(_ context.Context, _ *daemonclient.Client, _ HTTPStoreInfo, sourceID int64) error {
			selectedSourceID.Store(sourceID)
			preflightStep.Store(sequence.Add(1))
			return nil
		},
	})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"gmail-target"})

	require.NoError(command.Execute())
	assert.True(readOnlyOpened)
	assert.Equal(targetSource.ID, selectedSourceID.Load())
	assert.Equal(int32(1), capabilityStep.Load())
	assert.Equal(int32(2), preflightStep.Load())
	assert.Equal(int32(3), repairStep.Load())
	body := <-requestBody
	require.NotNil(body.SourceID)
	assert.Equal(targetSource.ID, *body.SourceID)
}

func TestRepairMessageCommandLocalResolutionFailsBeforeOAuthOrRepair(t *testing.T) {
	requirements := require.New(t)
	fixture := storetest.New(t)
	otherSource, err := fixture.Store.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	otherConversation, err := fixture.Store.EnsureConversation(otherSource.ID, "other-thread", "")
	requirements.NoError(err)
	_, err = fixture.Store.UpsertMessage(&store.Message{
		ConversationID: otherConversation, SourceID: otherSource.ID,
		SourceMessageID: "shared", MessageType: "email",
	})
	requirements.NoError(err)
	fixture.CreateMessage("shared")
	imap, err := fixture.Store.GetOrCreateSource("imap", "imap@example.com")
	requirements.NoError(err)
	imapConversation, err := fixture.Store.EnsureConversation(imap.ID, "imap-thread", "")
	requirements.NoError(err)
	_, err = fixture.Store.UpsertMessage(&store.Message{
		ConversationID: imapConversation, SourceID: imap.ID,
		SourceMessageID: "imap-message", MessageType: "email",
	})
	requirements.NoError(err)

	for _, test := range []struct {
		name      string
		reference string
		want      string
	}{
		{name: "ambiguous", reference: "shared", want: "ambiguous"},
		{name: "non Gmail", reference: "imap-message", want: "not gmail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var repairRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.15.0"}`))
					return
				}
				repairRequests.Add(1)
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
			}))
			t.Cleanup(server.Close)
			client, clientErr := daemonclient.New(daemonclient.Config{
				URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
			})
			require.NoError(clientErr)

			var preflightCalls atomic.Int32
			command := newRepairMessageCmd(repairMessageCommandDeps{
				isDaemonSubprocess: func() bool { return false },
				openHTTPStore: func(context.Context) (*daemonclient.Client, HTTPStoreInfo, error) {
					return client, HTTPStoreInfo{Kind: HTTPStoreLocalDaemon}, nil
				},
				openReadOnlyStore: func() (*store.Store, func(), error) {
					return fixture.Store, func() {}, nil
				},
				preflightReauth: func(context.Context, *daemonclient.Client, HTTPStoreInfo, int64) error {
					preflightCalls.Add(1)
					return errors.New("OAuth preflight must not run")
				},
			})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs([]string{test.reference})

			require.ErrorContains(command.Execute(), test.want)
			assert.Zero(preflightCalls.Load())
			assert.Zero(repairRequests.Load())
		})
	}
}

func TestRepairMessageCommandAuditUsesReadOnlyStoreAndExactJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	messageID := fixture.CreateMessage("gmail-audit")
	_, err := fixture.Store.DB().Exec(
		fixture.Store.Rebind(`UPDATE messages SET subject = 'stored subject' WHERE id = ?`), messageID)
	require.NoError(err)
	require.NoError(fixture.Store.UpsertMessageRaw(messageID,
		testemail.NewMessage().Subject("raw subject").Body("body").Bytes()))
	readOnlyOpened := false
	command := newRepairMessageCmd(repairMessageCommandDeps{
		openWritableStore: func() (*store.Store, func(), error) {
			return nil, func() {}, errors.New("audit must not open writable store")
		},
		openReadOnlyStore: func() (*store.Store, func(), error) {
			readOnlyOpened = true
			return fixture.Store, func() {}, nil
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--audit", "--source-id", "1", "--json"})

	require.NoError(command.Execute())
	assert.True(readOnlyOpened)
	var result syncer.RepairAuditResult
	require.NoError(json.Unmarshal(bytes.TrimSpace(output.Bytes()), &result))
	assert.Equal(messageID, result.InternalID)
	assert.Equal(syncer.AuditMismatch, result.Status)
	assert.Equal(syncer.AuditMismatch, result.Fields[syncer.AuditFieldSubject])
	assert.Equal(
		`{"internal_id":`+jsonNumber(messageID)+`,"source_id":1,"source_message_id":"gmail-audit","status":"mismatch","fields":{"attachment_hashes":"match","attachment_part_keys":"match","bcc":"match","body_html":"inconclusive","body_text":"inconclusive","cc":"match","from":"mismatch","raw_mime":"match","rfc822_message_id":"inconclusive","subject":"mismatch","to":"mismatch"}}`+"\n",
		output.String())
}

func TestRepairMessageCommandSurfacesAmbiguousAndNonGmailTargetsBeforeProviderAccess(t *testing.T) {
	requirements := require.New(t)
	fixture := storetest.New(t)
	otherSource, err := fixture.Store.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	otherConversation, err := fixture.Store.EnsureConversation(otherSource.ID, "other-thread", "")
	requirements.NoError(err)
	_, err = fixture.Store.UpsertMessage(&store.Message{
		ConversationID: otherConversation, SourceID: otherSource.ID,
		SourceMessageID: "shared", MessageType: "email",
	})
	requirements.NoError(err)
	fixture.CreateMessage("shared")
	imap, err := fixture.Store.GetOrCreateSource("imap", "imap@example.com")
	requirements.NoError(err)
	imapConversation, err := fixture.Store.EnsureConversation(imap.ID, "imap-thread", "")
	requirements.NoError(err)
	_, err = fixture.Store.UpsertMessage(&store.Message{
		ConversationID: imapConversation, SourceID: imap.ID,
		SourceMessageID: "imap-message", MessageType: "email", Subject: sql.NullString{String: "imap", Valid: true},
	})
	requirements.NoError(err)
	clientCalls := 0
	deps := repairMessageCommandDeps{
		openWritableStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		newGmailClient: func(context.Context, *store.Source) (gmail.API, error) {
			clientCalls++
			return gmail.NewMockAPI(), nil
		},
	}
	for _, test := range []struct {
		name string
		arg  string
		want string
	}{
		{name: "ambiguous", arg: "shared", want: "ambiguous"},
		{name: "non gmail", arg: "imap-message", want: "not gmail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			command := newRepairMessageCmd(deps)
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs([]string{test.arg})
			require.ErrorContains(command.Execute(), test.want)
		})
	}
	assert.Zero(t, clientCalls)
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}

func marshalRepairCommandJSON(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}
