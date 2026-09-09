package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftReplyPolicyStates(t *testing.T) {
	cases := []struct {
		name    string
		policy  []config.IMAPDraftSource
		source  int64
		typ     string
		mailbox string
		code    string
	}{
		{name: "disabled", policy: []config.IMAPDraftSource{{SourceID: 42, Mailbox: "Drafts"}}, source: 42, typ: "imap", code: "draft_disabled"},
		{name: "wrong source", policy: []config.IMAPDraftSource{{SourceID: 42, Enabled: true, Mailbox: "Drafts"}}, source: 41, typ: "imap", code: "draft_disabled"},
		{name: "wrong provider", policy: []config.IMAPDraftSource{{SourceID: 42, Enabled: true, Mailbox: "Drafts"}}, source: 42, typ: "gmail", code: "draft_disabled"},
		{name: "blank mailbox", policy: []config.IMAPDraftSource{{SourceID: 42, Enabled: true, Mailbox: " "}}, source: 42, typ: "imap", code: "invalid_mailbox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			mailbox, err := authorizeIMAPDraft(tc.policy, tc.source, tc.typ)
			requirements.Error(err)
			assertions.Empty(mailbox)
			assertions.Equal(tc.code, err.Error())
			coded, ok := errors.AsType[*api.CLIRunCodedError](err)
			requirements.True(ok)
			assertions.Error(coded.Err)
		})
	}
}

func TestRunCLIReplyDraftUsesTypedRoute(t *testing.T) {
	run := false
	adapter := &storeAPIAdapter{}
	err := adapter.runCLICommandWithRunner(context.Background(), api.CLIRunRequest{
		Args: []string{"draft-reply"},
	}, nil, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
		run = true
		return nil
	})
	require.ErrorContains(t, err, "invalid_args")
	assert.False(t, run)
}

func TestDraftPolicySnapshotRequiresDaemonRestart(t *testing.T) {
	cfg := config.Config{IMAP: config.IMAPConfig{Drafts: []config.IMAPDraftSource{{SourceID: 42, Enabled: true, Mailbox: "Drafts"}}}}
	snapshot := snapshotIMAPDraftPolicy(&cfg)
	cfg.IMAP.Drafts[0].Enabled = false
	assert.True(t, snapshot[0].Enabled)
}

func TestConfirmedSourceIdentityRejectsMismatch(t *testing.T) {
	identities := []store.AccountIdentity{{Address: "user@example.com", ConfirmedAt: time.Now()}}
	assert.False(t, hasConfirmedSourceIdentity(identities, "other@example.com"))
	assert.True(t, hasConfirmedSourceIdentity(identities, "USER@example.com"))
}

// draftReplyFixture is one archived IMAP parent message on a source backed by
// an in-memory IMAP server that advertises UIDPLUS.
type draftReplyFixture struct {
	store    *store.Store
	source   *store.Source
	parentID int64
	config   *imaplib.Config
	// refreshed records every analytics cache refresh label the route requested.
	refreshed *[]string
}

func newDraftReplyFixture(t *testing.T) draftReplyFixture {
	t.Helper()
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
	})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	imapConfig := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", imapConfig.Identifier())
	require.NoError(t, err)
	configJSON, err := imapConfig.ToJSON()
	require.NoError(t, err)
	// Request-side draft settings smuggled into the source JSON must be ignored.
	configJSON = strings.TrimSuffix(configJSON, "}") + `,"draft_enabled":false,"drafts_mailbox":"AttackerMailbox"}`
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, configJSON))
	require.NoError(t, st.AddAccountIdentity(source.ID, testutil.IMAPTestUsername, "manual"))
	conversationID, err := st.EnsureConversation(source.ID, "thread-666", "Question")
	require.NoError(t, err)
	senderID, err := st.EnsureParticipant("sender@example.com", "Sender", "example.com")
	require.NoError(t, err)
	ownerID, err := st.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
	require.NoError(t, err)
	parentRaw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"Message-ID: <parent@example.com>\r\n\r\n" +
		"Parent body\r\n")
	parentID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "INBOX|9",
			ConversationID: conversationID, RFC822MessageID: sql.NullString{String: "parent@example.com", Valid: true},
			MessageType: store.MessageTypeEmail, SenderID: sql.NullInt64{Int64: senderID, Valid: true},
			Subject:      sql.NullString{String: "Question", Valid: true},
			SentAt:       sql.NullTime{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
			SizeEstimate: int64(len(parentRaw)),
		},
		BodyText: sql.NullString{String: "Parent body", Valid: true}, RawMIME: parentRaw,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{senderID}, EmailAddresses: []string{"sender@example.com"}},
			{Type: "to", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{testutil.IMAPTestUsername}},
		},
	})
	require.NoError(t, err)
	return draftReplyFixture{store: st, source: source, parentID: parentID, config: imapConfig, refreshed: new([]string)}
}

func (f draftReplyFixture) grantedAdapter() *storeAPIAdapter {
	return &storeAPIAdapter{
		store:       f.store,
		draftPolicy: []config.IMAPDraftSource{{SourceID: f.source.ID, Enabled: true, Mailbox: "Drafts"}},
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			return imaplib.NewClient(f.config, testutil.IMAPTestPassword), nil
		},
		draftCacheRefresh: func(ctx context.Context, label string) error {
			// The refresh must run with the source lock already released.
			execution, err := f.store.AcquireSyncExecutionContext(ctx, f.source.ID)
			if err != nil {
				return fmt.Errorf("source still locked during cache refresh: %w", err)
			}
			*f.refreshed = append(*f.refreshed, label)
			return execution.Release()
		},
	}
}

func (f draftReplyFixture) run(t *testing.T, adapter *storeAPIAdapter, flags ...string) ([]api.CLIRunEvent, error) {
	t.Helper()
	args := append([]string{"draft-reply", strconv.FormatInt(f.parentID, 10), "--from", testutil.IMAPTestUsername}, flags...)
	var events []api.CLIRunEvent
	err := adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func TestRunCLIReplyDraftPublishesRemoteAndLocalRows(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()

	events, err := fixture.run(t, adapter, "--body", "reply body", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result map[string]any
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal("created", result["status"])
	assertions.Equal("Drafts", result["mailbox"])
	assertions.Contains(result["operation_ref"], fmt.Sprintf("%d:Drafts|", fixture.source.ID))
	assertions.NotContains(events[0].Data, "reply body")
	assertions.Equal([]string{fixture.source.Identifier}, *fixture.refreshed)

	events, err = fixture.run(t, adapter, "--log-level=debug", "--verbose", "--body", "second body", "--log-sql-slow-ms", "50")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Len(*fixture.refreshed, 2)
	var latestUIDValidity, latestUID uint32
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`
		SELECT uidvalidity, uid FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? ORDER BY uid DESC LIMIT 1
	`), fixture.source.ID, "Drafts").Scan(&latestUIDValidity, &latestUID))
	assertions.Contains(events[0].Data, fmt.Sprintf("Drafts|%d|%d", latestUIDValidity, latestUID))
	assertions.Contains(events[0].Data, fmt.Sprintf("operation %d:Drafts|%d:%d", fixture.source.ID, latestUIDValidity, latestUID))

	var localID int64
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`
		SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
	`), fixture.source.ID, "Drafts|1").Scan(&localID))
	assertions.NotZero(localID)
	var replyTo sql.NullInt64
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), localID).Scan(&replyTo))
	assertions.Equal(fixture.parentID, replyTo.Int64)
	assertions.True(replyTo.Valid)
	var rfc822MessageID string
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), localID).Scan(&rfc822MessageID))
	assertions.True(strings.HasPrefix(rfc822MessageID, "<"))
	assertions.True(strings.HasSuffix(rfc822MessageID, ">"))
	var membershipCount, cursorCount int
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE message_id = ?`), localID).Scan(&membershipCount))
	var membershipMailbox, membershipFlags string
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`SELECT mailbox, flags FROM imap_message_memberships WHERE message_id = ?`), localID).Scan(&membershipMailbox, &membershipFlags))
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`SELECT COUNT(*) FROM imap_folder_state WHERE source_id = ?`), fixture.source.ID).Scan(&cursorCount))
	assertions.Equal(1, membershipCount)
	assertions.Equal("Drafts", membershipMailbox)
	assertions.Equal(`["\\Draft"]`, membershipFlags)
	assertions.Zero(cursorCount)
	raw, err := fixture.store.GetMessageRaw(localID)
	requirements.NoError(err)
	assertions.Contains(string(raw), "In-Reply-To: <parent@example.com>")
	assertions.Contains(string(raw), "reply body")
}

func TestRunCLIReplyDraftDeniesBeforeConnecting(t *testing.T) {
	fixture := newDraftReplyFixture(t)
	refuseConnect := func(context.Context, *store.Source) (*imaplib.Client, error) {
		return nil, errors.New("denied requests must not open an IMAP connection")
	}
	cases := []struct {
		name    string
		adapter *storeAPIAdapter
		flags   []string
		code    string
		cause   string
	}{
		{
			name:    "no grant",
			adapter: &storeAPIAdapter{store: fixture.store, draftClientFactory: refuseConnect},
			flags:   []string{"--body", "reply body"},
			code:    "draft_disabled",
			cause:   "no enabled [[imap.drafts]] grant",
		},
		{
			name: "duplicate from flag",
			adapter: &storeAPIAdapter{
				store:              fixture.store,
				draftPolicy:        []config.IMAPDraftSource{{SourceID: fixture.source.ID, Enabled: true, Mailbox: "Drafts"}},
				draftClientFactory: refuseConnect,
			},
			flags: []string{"--from", "other@example.com", "--body", "reply body"},
			code:  "invalid_args",
			cause: "--from given more than once",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			events, err := fixture.run(t, tc.adapter, tc.flags...)
			requirements.Error(err)
			assertions.Empty(events)
			assertions.Equal(tc.code, err.Error())
			coded, ok := errors.AsType[*api.CLIRunCodedError](err)
			requirements.True(ok)
			requirements.ErrorContains(coded.Err, tc.cause)
		})
	}

	t.Run("unconfirmed identity", func(t *testing.T) {
		adapter := &storeAPIAdapter{
			store:              fixture.store,
			draftPolicy:        []config.IMAPDraftSource{{SourceID: fixture.source.ID, Enabled: true, Mailbox: "Drafts"}},
			draftClientFactory: refuseConnect,
		}
		var events []api.CLIRunEvent
		err := adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{
			Args: []string{"draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", "other@example.com", "--body", "reply body"},
		}, func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		})
		requirements := require.New(t)
		assertions := assert.New(t)
		requirements.Error(err)
		assertions.Empty(events)
		assertions.Equal("invalid_from", err.Error())
		coded, ok := errors.AsType[*api.CLIRunCodedError](err)
		requirements.True(ok)
		requirements.ErrorContains(coded.Err, "not a confirmed identity")
		assertions.NotContains(coded.Err.Error(), "other@example.com")
	})
}

func TestRunCLIReplyDraftRefusesWhileSourceSyncs(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	execution, err := fixture.store.AcquireSyncExecutionContext(t.Context(), fixture.source.ID)
	requirements.NoError(err)
	defer func() { _ = execution.Release() }()
	adapter := fixture.grantedAdapter()
	adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		return nil, errors.New("a request refused for an active sync must not open an IMAP connection")
	}

	events, err := fixture.run(t, adapter, "--body", "reply body")
	requirements.Error(err)
	assertions.Empty(events)
	assertions.Equal("sync_active", err.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](err)
	requirements.True(ok)
	requirements.ErrorIs(coded.Err, store.ErrSyncAlreadyActive)
	assertions.NotContains(coded.Err.Error(), "reply body")
}

func TestRunCLIReplyDraftReportsLocalPersistenceFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	// Occupy the provider key the first APPEND into the empty mailbox will
	// receive, so the remote accepts the draft but local publication conflicts.
	conversationID, err := fixture.store.EnsureConversation(fixture.source.ID, "thread-stale", "Stale")
	requirements.NoError(err)
	_, err = fixture.store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: fixture.source.ID, SourceMessageID: "Drafts|1", ConversationID: conversationID,
			MessageType: store.MessageTypeEmail, Subject: sql.NullString{String: "Stale", Valid: true},
		},
		BodyText: sql.NullString{String: "stale", Valid: true}, RawMIME: []byte("Subject: Stale\r\n\r\nstale\r\n"),
	})
	requirements.NoError(err)

	events, err := fixture.run(t, fixture.grantedAdapter(), "--body", "reply body", "--json")
	requirements.Error(err)
	assertions.Equal("remote_accepted_local_failed", err.Error())
	coded, ok := errors.AsType[*api.CLIRunCodedError](err)
	requirements.True(ok)
	requirements.ErrorContains(coded.Err, "source_key_conflict")
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	var result map[string]any
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal("remote_accepted_local_failed", result["status"])
	assertions.Equal(fmt.Sprintf("%d:Drafts|%v:%v", fixture.source.ID, result["uidvalidity"], result["uid"]), result["operation_ref"])
	assertions.NotContains(events[0].Data, "reply body")
	var membershipCount int
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?`), fixture.source.ID).Scan(&membershipCount))
	assertions.Zero(membershipCount)
	assertions.Empty(*fixture.refreshed)
}

func TestAppendDraftReplyRetainsFailureCause(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}},
	})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	adapter := &storeAPIAdapter{
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			return imaplib.NewClient(&imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword), nil
		},
	}
	for _, tc := range []struct{ name, mailbox, code, cause string }{
		{"missing UIDPLUS", "Drafts", "uidplus_required", "IMAP server does not advertise UIDPLUS"},
		{"invalid mailbox", "", "invalid_mailbox", "mailbox must be nonblank"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			var events []api.CLIRunEvent
			_, err := adapter.appendDraftReply(t.Context(), draftReplyTarget{source: &store.Source{}, mailbox: tc.mailbox}, []byte("Subject: Reply\r\n\r\nreply body\r\n"), func(event api.CLIRunEvent) error {
				events = append(events, event)
				return nil
			})
			requirements.EqualError(err, tc.code)
			coded, ok := errors.AsType[*api.CLIRunCodedError](err)
			requirements.True(ok)
			requirements.EqualError(coded.Err, tc.cause)
			assertions.Equal([]api.CLIRunEvent{{Type: cliStreamStderr, Data: tc.code + "\n"}}, events)
		})
	}
}

func TestDraftReplyCLIFailureOutput(t *testing.T) {
	for _, failure := range []string{"local persistence", "APPEND rejected", "no result"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", failure, asJSON), func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				fixture := newDraftReplyFixture(t)
				adapter := fixture.grantedAdapter()
				switch failure {
				case "local persistence":
					// The remote accepts UID 1, but the archive already owns its key.
					conversationID, err := fixture.store.EnsureConversation(fixture.source.ID, "stale", "Stale")
					requirements.NoError(err)
					_, err = fixture.store.PersistMessage(&store.MessagePersistData{
						Message: &store.Message{
							SourceID: fixture.source.ID, SourceMessageID: "Drafts|1",
							ConversationID: conversationID,
							MessageType:    store.MessageTypeEmail,
						},
						BodyText: sql.NullString{String: "stale", Valid: true},
						RawMIME:  []byte("Subject: Stale\r\n\r\nstale\r\n"),
					})
					requirements.NoError(err)
				case "APPEND rejected":
					adapter.draftPolicy[0].Mailbox = "Missing"
				case "no result":
					adapter.draftPolicy = nil
				}
				server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
					Config: &config.Config{HomeDir: t.TempDir()},
					Store:  adapter,
					Logger: slog.New(slog.DiscardHandler),
				}).Router())
				t.Cleanup(server.Close)
				configureRemoteDaemonForTest(t, server.URL)
				root := &cobra.Command{Use: "msgvault"}
				root.AddCommand(newDraftReplyCommand())
				silenceUsageInRunE(root)
				var stdout, stderr bytes.Buffer
				root.SetOut(&stdout)
				root.SetErr(&stderr)
				args := []string{"draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", "reply body"}
				if asJSON {
					args = append(args, "--json")
				}
				root.SetArgs(args)
				requirements.Error(root.ExecuteContext(t.Context()))
				assertions.Empty(stdout.String())
				assertions.Len(strings.Split(strings.TrimSpace(stderr.String()), "\n"), 1, stderr.String())
				switch failure {
				case "local persistence":
					if asJSON {
						var result draftReplyOutput
						requirements.NoError(json.Unmarshal(stderr.Bytes(), &result), stderr.String())
						assertions.Equal(draftReplyStatusLocalFailed, result.Status)
						assertions.NotEmpty(result.OperationRef)
					} else {
						assertions.Contains(stderr.String(), "remote accepted; local persistence failed, inspect operation ")
					}
				case "APPEND rejected":
					assertions.Equal("append_rejected\n", stderr.String())
				case "no result":
					assertions.Contains(stderr.String(), "Error:")
					assertions.Contains(stderr.String(), "draft_disabled")
				}
			})
		}
	}
}
