package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	archivesync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

func inboxIMAPDaemonFixture(t *testing.T) (*storeAPIAdapter, inboxcontrol.Target) {
	t.Helper()
	a, target, _ := inboxIMAPDaemonFixtureWithCommands(t)
	return a, target
}

func inboxIMAPDaemonFixtureWithCommands(t *testing.T) (*storeAPIAdapter, inboxcontrol.Target, *inboxIMAPCommandCounts) {
	t.Helper()
	addr, counts := startInboxIMAPCommandServer(t)
	cfg := reviewIMAPConfig(t, addr)
	client := imaplib.NewClient(cfg, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", cfg.Identifier())
	require.NoError(t, err)
	encoded, err := cfg.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, encoded))
	opts := archivesync.DefaultOptions()
	opts.SourceType, opts.NoResume = "imap", true
	summary, err := archivesync.New(client, st, opts).Full(t.Context(), source.Identifier)
	require.NoError(t, err)
	require.NoError(t, saveIMAPFolderStates(t.Context(), st, source, client, summary, 0))
	observations := client.ObservedMemberships()
	require.NotEmpty(t, observations)
	providerID := observations[0].SourceMessageID
	ids, err := st.MessageExistsBatch(source.ID, []string{providerID})
	require.NoError(t, err)
	require.Contains(t, ids, providerID)
	item := ids[providerID]
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), item)
	require.NoError(t, err)
	epoch := client.ObservedFolderStates()["INBOX"].UIDValidity
	require.NotZero(t, epoch)
	a := &storeAPIAdapter{store: st, config: &config.Config{HomeDir: t.TempDir()}, inboxKey: []byte(strings.Repeat("k", 32)), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return imaplib.NewClient(cfg, testutil.IMAPTestPassword), nil
	}}
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "imap", SourceIdentifier: source.Identifier, AccountID: cfg.Username, Scope: inboxcontrol.ScopeMessage, ItemID: item, ProviderID: providerID, Mailbox: "INBOX", UIDValidity: epoch, UID: 1}
	return a, target, counts
}

func TestDaemonInboxIMAPNativeSignedReadState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target := inboxIMAPDaemonFixture(t)
	principal := inboxcontrol.Principal{ID: "owner", Owner: true}
	authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
	gates := 0
	gate := func(context.Context) (func(), error) { gates++; return func() {}, nil }
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
	preview, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	requirements.NotNil(preview.Before)
	assertions.Equal([]string{}, preview.Before.Flags)
	assertions.Equal([]string{}, preview.Before.Tags)
	assertions.Zero(gates)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-read-once"
	result, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	requirements.NotNil(result.After.Read)
	assertions.True(*result.After.Read)
	assertions.Equal(1, gates)
	var localRead bool
	requirements.NoError(a.store.DB().QueryRow(a.store.Rebind(`SELECT is_read FROM messages WHERE id=?`), target.ItemID).Scan(&localRead))
	assertions.False(localRead, "native read state does not rewrite local archive read state")
	stored, err := a.store.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(stored)
	assertions.Equal([]string{}, stored.Tags)
	assertions.Equal([]string{"\\Seen"}, stored.Flags)
	replay, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(1, gates)
}

func TestDaemonInboxIMAPRejectsAccountAndConfigMismatchBeforeFactory(t *testing.T) {
	for _, mismatch := range []string{"account", "config"} {
		t.Run(mismatch, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			a, target := inboxIMAPDaemonFixture(t)
			calls := 0
			a.emailTagClientFactory = func(context.Context, *store.Source) (gmail.API, error) { calls++; return nil, nil }
			if mismatch == "account" {
				target.AccountID = target.SourceIdentifier
			} else {
				requirements.NoError(a.store.UpdateSourceSyncConfig(target.SourceID, `{"host":"other.example.test","username":"reader","port":993,"tls":true}`))
			}
			result, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
			requirements.ErrorIs(err, inboxcontrol.ErrDenied)
			assertions.Nil(result)
			assertions.Zero(calls)
		})
	}
}

func TestDaemonInboxIMAPSDKPreservesEmptyExpectedState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target := inboxIMAPDaemonFixture(t)
	a.config.Server.APIKey = "synthetic-native-owner-key"
	a.config.Server.AgentAccess = true
	router := api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router()
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: a.config.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
	preview, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(preview.Before)
	assertions.Equal([]string{}, preview.Before.Flags)
	assertions.Equal([]string{}, preview.Before.Tags)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "sdk-empty-state-once"
	result, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err, "the actual generated request must retain empty FLAGS in signed expected state")
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.Equal([]string{}, result.After.Tags)
	replay, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	receipt, err := client.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID})
	requirements.NoError(err)
	assertions.Equal([]string{}, receipt.Before.Flags)
	assertions.Equal([]string{}, receipt.After.Tags)

	// The CLI and MCP use the same daemon/adapter and exact imported membership.
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: a.config.Server.APIKey, AllowInsecure: true}})
	read := inboxcontrol.Request{Target: &target}
	encoded, err := json.Marshal(read)
	requirements.NoError(err)
	command := newInboxCmd()
	command.SetContext(ctx)
	command.SetArgs([]string{"get-state", "--request", "-"})
	command.SetIn(bytes.NewReader(encoded))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	requirements.NoError(command.Execute())
	var cliResult inboxcontrol.Result
	requirements.NoError(json.Unmarshal(output.Bytes(), &cliResult))
	requirements.NotNil(cliResult.Before)
	assertions.Equal([]string{}, cliResult.Before.Tags)
	assertions.Equal([]string{"\\Seen"}, cliResult.Before.Flags)
	encoded, err = json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(encoded, &targetArgs))
	session := nativeInboxMCPSession(t, inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, target.SourceID))
	mcpResult, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_get_state", Arguments: map[string]any{"target": targetArgs}})
	requirements.NoError(err)
	requirements.False(mcpResult.IsError)
	encoded, err = json.Marshal(mcpResult.StructuredContent)
	requirements.NoError(err)
	var envelope struct {
		Result *inboxcontrol.Result `json:"result"`
	}
	requirements.NoError(json.Unmarshal(encoded, &envelope))
	requirements.NotNil(envelope.Result)
	requirements.NotNil(envelope.Result.Before)
	assertions.Equal([]string{}, envelope.Result.Before.Tags)
	assertions.Equal([]string{"\\Seen"}, envelope.Result.Before.Flags)
}

type inboxIMAPCommandCounts struct {
	stores, moves, copies, expunges, creates atomic.Int64
}

type inboxIMAPCommandSession struct {
	imapserver.Session
	counts *inboxIMAPCommandCounts
}

func (s *inboxIMAPCommandSession) Select(name string, opts *imapapi.SelectOptions) (*imapapi.SelectData, error) {
	data, err := s.Session.Select(name, opts)
	if err == nil {
		// The memory server derives its catalog from present flags. Explicitly
		// model a mailbox permitting Seen even while its only message is unread.
		data.PermanentFlags = append(data.PermanentFlags, imapapi.FlagSeen)
	}
	return data, err
}

func inboxIMAPDelegatedClient(t *testing.T, url, ownerKey string, sourceID int64, permissions ...string) *daemonclient.Client {
	t.Helper()
	requirements := require.New(t)
	if len(permissions) == 0 {
		permissions = []string{"inbox.read", "inbox.read-state"}
	}
	payload, err := json.Marshal(map[string]any{"label": "native-imap", "permissions": permissions, "source_ids": []int64{sourceID}})
	requirements.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/api/v1/agent-tokens", bytes.NewReader(payload))
	requirements.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+ownerKey)
	response, err := http.DefaultClient.Do(request)
	requirements.NoError(err)
	defer response.Body.Close()
	requirements.Equal(http.StatusCreated, response.StatusCode)
	var grant struct {
		Secret string `json:"secret"`
	}
	requirements.NoError(json.UnmarshalRead(response.Body, &grant))
	requirements.NotEmpty(grant.Secret)
	client, err := daemonclient.New(daemonclient.Config{URL: url, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func (s *inboxIMAPCommandSession) Store(w *imapserver.FetchWriter, set imapapi.NumSet, flags *imapapi.StoreFlags, opts *imapapi.StoreOptions) error {
	s.counts.stores.Add(1)
	return s.Session.Store(w, set, flags, opts)
}

func (s *inboxIMAPCommandSession) Move(w *imapserver.MoveWriter, set imapapi.NumSet, destination string) error {
	s.counts.moves.Add(1)
	return s.Session.(imapserver.SessionMove).Move(w, set, destination)
}

func (s *inboxIMAPCommandSession) Copy(set imapapi.NumSet, destination string) (*imapapi.CopyData, error) {
	s.counts.copies.Add(1)
	return s.Session.Copy(set, destination)
}

func (s *inboxIMAPCommandSession) Expunge(w *imapserver.ExpungeWriter, set *imapapi.UIDSet) error {
	s.counts.expunges.Add(1)
	return s.Session.Expunge(w, set)
}

func (s *inboxIMAPCommandSession) Create(name string, opts *imapapi.CreateOptions) error {
	s.counts.creates.Add(1)
	return s.Session.Create(name, opts)
}

// Count commands while delegating every operation to real native mailboxes.
func startInboxIMAPCommandServer(t *testing.T) (string, *inboxIMAPCommandCounts) {
	t.Helper()
	counts := &inboxIMAPCommandCounts{}
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archive", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	mem := imapmemserver.New()
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{Caps: imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}, InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &inboxIMAPCommandSession{Session: mem.NewSession(), counts: counts}, nil, nil
	}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String(), counts
}

func TestDaemonInboxIMAPConfirmedTransportsRetainReceipts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target, counts := inboxIMAPDaemonFixtureWithCommands(t)
	a.config.Server.APIKey = "synthetic-native-owner-key"
	a.config.Server.AgentAccess = true
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: a.config.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	ownerClient := client
	client = inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, target.SourceID)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
	preview, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	assertions.Zero(counts.stores.Load())
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "mcp-native-read"
	encoded, err := json.Marshal(request)
	requirements.NoError(err)
	var args map[string]any
	requirements.NoError(json.Unmarshal(encoded, &args))
	delete(args, "operation")
	args["dry_run"] = false
	session := nativeInboxMCPSession(t, client)
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_set_read", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Zero(counts.stores.Load())
	args["idempotency_key"] = "changed-after-confirmation"
	accept := sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_set_read", Arguments: args, RequestState: pending.RequestState, InputResponses: accept})
	requirements.NoError(err)
	assertions.True(refused.IsError)
	assertions.Zero(counts.stores.Load())
	args["idempotency_key"] = request.IdempotencyKey
	// Refused confirmation is consumed. Obtain a fresh challenge for this intent.
	pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_set_read", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_set_read", Arguments: args, RequestState: pending.RequestState, InputResponses: accept})
	requirements.NoError(err)
	requirements.False(confirmed.IsError)
	assertions.Equal(int64(1), counts.stores.Load())
	replay, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(replay.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, replay.Receipt.Status)
	assertions.Equal([]string{}, replay.Receipt.Before.Flags)
	assertions.Equal([]string{}, replay.Receipt.After.Tags)
	assertions.Equal(int64(1), counts.stores.Load())

	// A real CLI child loses its stdout after the native write has committed.
	client = ownerClient
	request = inboxcontrol.Request{Operation: inboxcontrol.OpSetUnread, Target: &target, DryRun: true}
	preview, err = client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "cli-native-lost-output"
	dir := t.TempDir()
	requestFile, keyFile := filepath.Join(dir, "request.json"), filepath.Join(dir, "owner-key")
	encoded, err = json.Marshal(request)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(requestFile, encoded, 0o600))
	requirements.NoError(os.WriteFile(keyFile, []byte(a.config.Server.APIKey), 0o600))
	r, w, err := os.Pipe()
	requirements.NoError(err)
	requirements.NoError(r.Close())
	defer w.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInboxIMAPCLINativeClosedStdoutHelper$")
	child.Env = append(os.Environ(), "MSGVAULT_IMAP_CLOSED_STDOUT=1", "MSGVAULT_IMAP_DAEMON_URL="+server.URL, "MSGVAULT_IMAP_REQUEST="+requestFile, "MSGVAULT_IMAP_OWNER_KEY_FILE="+keyFile)
	child.Stdout = w
	var childError bytes.Buffer
	child.Stderr = &childError
	requirements.NoError(child.Run(), childError.String())
	replay, err = client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, replay.Receipt.Status)
	assertions.Equal(request.IdempotencyKey, replay.Receipt.IdempotencyKey)
	assertions.Equal([]string{}, replay.After.Flags)
	assertions.Equal([]string{}, replay.Receipt.Projected.Flags)
	assertions.Equal(int64(2), counts.stores.Load(), "recovery by the original key does not repeat the CLI write")
	assertions.Zero(counts.moves.Load())
	assertions.Zero(counts.copies.Load())
	assertions.Zero(counts.expunges.Load())
	assertions.Zero(counts.creates.Load())
}

func TestInboxIMAPCLINativeClosedStdoutHelper(t *testing.T) {
	if os.Getenv("MSGVAULT_IMAP_CLOSED_STDOUT") != "1" {
		return
	}
	key, err := os.ReadFile(os.Getenv("MSGVAULT_IMAP_OWNER_KEY_FILE"))
	require.NoError(t, err)
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: os.Getenv("MSGVAULT_IMAP_DAEMON_URL"), APIKey: string(key), AllowInsecure: true}})
	command := newInboxCmd()
	ensureSilenceUsageWrapped(command)
	command.SetContext(ctx)
	command.SetArgs([]string{"set-unread", "--request", os.Getenv("MSGVAULT_IMAP_REQUEST"), "--apply"})
	command.SetOut(os.Stdout)
	command.SetErr(io.Discard)
	if !errors.Is(command.Execute(), inboxcontrol.ErrOutcomeUnknown) {
		os.Exit(23)
	}
	os.Exit(0)
}

func TestDaemonInboxIMAPMoveRechecksGrantAndReplaysOriginalTarget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target, counts := inboxIMAPDaemonFixtureWithCommands(t)
	a.config.Server.APIKey = "synthetic-native-owner-key"
	a.config.Server.AgentAccess = true
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: a.config.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	folders, err := owner.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpListFolders, Source: &source})
	requirements.NoError(err)
	var destination inboxcontrol.Folder
	for _, folder := range folders.Folders {
		if folder.ID == "Archive" {
			destination = folder
		}
	}
	requirements.NotZero(destination.UIDValidity)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &target, Destination: &destination, DryRun: true}
	readOnly := inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, target.SourceID)
	_, err = readOnly.ControlInbox(t.Context(), request)
	requirements.Error(err, "read-state permission does not grant MOVE")
	assertions.Zero(counts.moves.Load())
	client := inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, target.SourceID, "inbox.read", "inbox.move")
	preview, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	assertions.Zero(counts.moves.Load())
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "daemon-native-move-once"
	for _, changed := range []string{"preview", "destination", "target"} {
		probe := request
		switch changed {
		case "preview":
			probe.PreviewToken += "invalid"
		case "destination":
			other := destination
			other.UIDValidity++
			probe.Destination = &other
		case "target":
			other := target
			other.UID++
			probe.Target = &other
		}
		_, err := client.ControlInbox(t.Context(), probe)
		requirements.Error(err, changed)
		assertions.Zero(counts.moves.Load(), changed)
	}
	result, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	assertions.Equal(int64(1), counts.moves.Load())
	requirements.NotNil(result.After)
	assertions.Equal("Archive", result.After.Target.Mailbox)
	assertions.Equal(destination.UIDValidity, result.After.Target.UIDValidity)
	assertions.Equal(target.ItemID, result.After.Target.ItemID)
	local, err := a.store.EmailTagTargetContext(t.Context(), target.ItemID, "Archive")
	requirements.NoError(err)
	assertions.Equal(result.After.Target.UID, local.UID)
	_, err = a.store.EmailTagTargetContext(t.Context(), target.ItemID, "INBOX")
	requirements.Error(err)
	replay, err := client.ControlInbox(t.Context(), request)
	requirements.NoError(err, "receipt authority survives removal of the original native membership")
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	receiptRequest := inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID}
	receipt, err := client.ControlInbox(t.Context(), receiptRequest)
	requirements.NoError(err)
	assertions.Equal(target, receipt.Receipt.Before.Target)
	assertions.Equal(result.After.Target, receipt.Receipt.After.Target)
	grants, err := owner.ListAgentTokens(t.Context())
	requirements.NoError(err)
	for _, grant := range grants {
		requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
	}
	_, err = client.ControlInbox(t.Context(), request)
	requirements.Error(err)
	_, err = client.ControlInbox(t.Context(), receiptRequest)
	requirements.Error(err)
	assertions.Equal(int64(1), counts.moves.Load())
	assertions.Zero(counts.stores.Load())
	assertions.Zero(counts.copies.Load())
	assertions.Zero(counts.expunges.Load())
	assertions.Zero(counts.creates.Load())
}

func TestDaemonInboxIMAPCreateFolderHasDurableExactEpoch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, target, counts := inboxIMAPDaemonFixtureWithCommands(t)
	principal := inboxcontrol.Principal{ID: "owner", Owner: true}
	authorize := func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }
	gate := func(context.Context) (func(), error) { return func() {}, nil }
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &source, Destination: &inboxcontrol.Folder{Name: "Triage"}, DryRun: true}
	preview, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	assertions.Zero(counts.creates.Load())
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "daemon-create-once"
	result, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	requirements.NotNil(result.After.ProvisionedFolder)
	folder := *result.After.ProvisionedFolder
	assertions.Equal("Triage", folder.ID)
	assertions.NotZero(folder.UIDValidity)
	states, err := a.store.GetIMAPFolderStates(target.SourceID)
	requirements.NoError(err)
	var epoch uint32
	for _, state := range states {
		if state.Mailbox == folder.ID {
			epoch = state.UIDValidity
		}
	}
	assertions.Equal(folder.UIDValidity, epoch)
	replay, err := a.ControlInbox(t.Context(), request, principal, authorize, gate)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(int64(1), counts.creates.Load())
	assertions.Zero(counts.stores.Load())
	assertions.Zero(counts.moves.Load())
	assertions.Zero(counts.copies.Load())
	assertions.Zero(counts.expunges.Load())
}

func TestDaemonInboxIMAPRemovedSourceGrantCannotReachReimport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, original, counts := inboxIMAPDaemonFixtureWithCommands(t)
	a.config.Server.APIKey = "synthetic-native-owner-key"
	a.config.Server.AgentAccess = true
	source, err := a.store.GetSourceByID(original.SourceID)
	requirements.NoError(err)
	requirements.True(source.SyncConfig.Valid)
	cfg, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
	requirements.NoError(err)
	factory := a.emailTagClientFactory
	var factoryCalls atomic.Int64
	a.emailTagClientFactory = func(ctx context.Context, source *store.Source) (gmail.API, error) {
		factoryCalls.Add(1)
		return factory(ctx, source)
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	oldGrant := inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, original.SourceID)
	_, err = oldGrant.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &original})
	requirements.NoError(err, "prove the real issued grant worked before source removal")
	requirements.Positive(factoryCalls.Load())
	requirements.NoError(a.store.RemoveSource(original.SourceID))

	// Reimport the same account and actual message through the production sync.
	// Resolve the new tuple from Store instead of retaining or forcing an ID.
	reimported, err := a.store.GetOrCreateSource("imap", cfg.Identifier())
	requirements.NoError(err)
	requirements.NoError(a.store.UpdateSourceSyncConfig(reimported.ID, source.SyncConfig.String))
	native := imaplib.NewClient(cfg, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = native.Close() })
	opts := archivesync.DefaultOptions()
	opts.SourceType, opts.NoResume = "imap", true
	summary, err := archivesync.New(native, a.store, opts).Full(t.Context(), reimported.Identifier)
	requirements.NoError(err)
	requirements.NoError(saveIMAPFolderStates(t.Context(), a.store, reimported, native, summary, 0))
	ids, err := a.store.MessageExistsBatch(reimported.ID, []string{original.ProviderID})
	requirements.NoError(err)
	requirements.Contains(ids, original.ProviderID)
	current := original
	current.SourceID, current.ItemID = reimported.ID, ids[original.ProviderID]
	current.UIDValidity = native.ObservedFolderStates()["INBOX"].UIDValidity
	requirements.NotZero(current.UIDValidity)
	factoryCalls.Store(0)
	for _, target := range []inboxcontrol.Target{original, current} {
		for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpGetState, inboxcontrol.OpSetRead} {
			_, err := oldGrant.ControlInbox(t.Context(), inboxcontrol.Request{Operation: operation, Target: &target, DryRun: operation.IsMutation()})
			requirements.ErrorIs(err, inboxcontrol.ErrDenied)
			assertions.Zero(factoryCalls.Load(), "old authority must be denied before native client construction")
		}
	}
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: a.config.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	_, err = owner.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &current})
	requirements.NoError(err, "owner can read the actual reimported source")
	freshGrant := inboxIMAPDelegatedClient(t, server.URL, a.config.Server.APIKey, current.SourceID)
	_, err = freshGrant.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &current})
	requirements.NoError(err, "a newly issued source grant can read the same tuple")
	assertions.Positive(factoryCalls.Load())
	assertions.Zero(counts.stores.Load())
	assertions.Zero(counts.moves.Load())
	assertions.Zero(counts.copies.Load())
	assertions.Zero(counts.expunges.Load())
	assertions.Zero(counts.creates.Load())
}
