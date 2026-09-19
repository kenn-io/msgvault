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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

type reviewDraftCommitHandler struct {
	slog.Handler

	onCommit func()
}

func (h reviewDraftCommitHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "sql tx commit" || record.Message == "sql tx slow" {
		h.onCommit()
	}
	return nil
}

func TestDraftLifecycleCancelledAfterClaimBeforeAppend(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	addr, user := startReviewCmdStoreBarrierServer(t, barrier)
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})
	barrier.appendCalls.Store(0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	previous := slog.Default()
	slog.SetDefault(slog.New(reviewDraftCommitHandler{
		Handler: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
		onCommit: func() {
			if draft, err := fixture.store.GetIMAPDraftContext(context.Background(), fixture.draft.DraftID); err == nil && draft.Pending != nil {
				cancel()
			}
		},
	}))
	defer slog.SetDefault(previous)
	var events []api.CLIRunEvent
	err := fixture.adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{Args: []string{"draft-edit", fixture.draft.DraftID, "--revision", "1", "--body", "unsent", "--json"}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.ErrorContains(err, "cancelled")
	requirements.ErrorIs(ctx.Err(), context.Canceled)
	assertions.Zero(barrier.appendCalls.Load())
	pending, err := fixture.store.GetIMAPDraftContext(context.Background(), fixture.draft.DraftID)
	requirements.NoError(err)
	assertions.Nil(pending.Pending)
	assertions.Equal(int64(1), pending.Revision)
	assertions.Equal(uint32(1), reviewDraftMailboxCount(t, addr))
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal(draftLifecycleActive, output.Status)
	assertions.False(output.ManualReconciliation)
}

func TestDraftLifecycleRejectedClaimPersistenceEvidence(t *testing.T) {
	testutil.SkipIfPostgres(t, "claim failure injection uses SQLite triggers")
	for _, fault := range []string{"record", "clear"} {
		t.Run(fault, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{}), appendErr: &emersionimap.Error{Type: emersionimap.StatusResponseTypeNo, Text: "injected rejection"}}
			addr, user := startReviewCmdStoreBarrierServer(t, barrier)
			fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
				testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
			})
			barrier.appendCalls.Store(0)
			condition := "NEW.pending_code = 'append_rejected'"
			if fault == "clear" {
				condition = "OLD.pending_operation IS NOT NULL AND NEW.pending_operation IS NULL"
			}
			_, err := fixture.store.DB().Exec("CREATE TRIGGER fail_rejected_claim BEFORE UPDATE ON imap_drafts WHEN " + condition + " BEGIN SELECT RAISE(FAIL, 'injected rejection persistence failure'); END")
			requirements.NoError(err)
			events, err := runReviewLifecycle(t, fixture.adapter, "draft-edit", fixture.draft.DraftID, "--revision", "1", "--body", "candidate", "--json")
			requirements.Error(err)
			requirements.Len(events, 1)
			var output draftLifecycleOutput
			requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
			if fault == "clear" {
				assertions.Equal("pending", output.Status)
				assertions.True(output.ManualReconciliation)
				assertions.Contains(output.CandidateContent, "candidate")
				assertions.Equal("local_persistence_failed", err.Error())
				coded, ok := errors.AsType[*api.CLIRunCodedError](err)
				requirements.True(ok)
				requirements.ErrorContains(coded.Err, "injected rejection persistence failure")
			} else {
				assertions.Equal(draftLifecycleActive, output.Status)
				assertions.False(output.ManualReconciliation)
			}
			pending, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
			requirements.NoError(err)
			if fault == "clear" {
				requirements.NotNil(pending.Pending)
				assertions.Contains(string(pending.Pending.Raw), "candidate")
			} else {
				assertions.Nil(pending.Pending)
			}
			assertions.Equal(int64(1), pending.Revision)
			assertions.Equal(int32(1), barrier.appendCalls.Load())
			assertions.Equal(uint32(1), reviewDraftMailboxCount(t, addr))
		})
	}
}

func TestDraftReplyHumanLifecycleHandle(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newDraftReplyCommand())
	silenceUsageInRunE(root)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", "handle"})
	requirements.NoError(root.ExecuteContext(t.Context()))
	var draftID string
	requirements.NoError(fixture.store.DB().QueryRow("SELECT draft_id FROM imap_drafts").Scan(&draftID))
	assertions.Contains(stdout.String(), "draft "+draftID+" revision 1")
	draft, err := fixture.store.GetIMAPDraftContext(t.Context(), draftID)
	requirements.NoError(err)
	assertions.Equal(int64(1), draft.Revision)
}

func TestEmitDraftLifecycleOutputHuman(t *testing.T) {
	tests := []struct {
		name   string
		output draftLifecycleOutput
		want   []string
	}{
		{
			name: "sanitizes metadata and multiline content",
			output: draftLifecycleOutput{
				Status:               "status\x1b[31m\r\ninjected",
				DraftID:              "draft-ID\x1b[31m\r\nspoofed",
				Revision:             7,
				Lifecycle:            "life\x1b]0;title\x07cycle",
				Receipt:              draftLifecycleReceipt{Mailbox: "Mailbox\x1b]0;x\x07\r\nreceipt", UIDValidity: 11, UID: 12},
				Content:              "line one\r\nline \x1b[31mred\x1b[0m\nline\u009bthree",
				RawMIME:              "raw-only-value",
				CandidateContent:     "candidate one\ncandidate \x1b]0;evil\x07two\r\nend",
				PendingOperation:     "edit\x1b[31m\r\noperation",
				PendingCode:          "pending\x1b[31m\r\ncode",
				PendingReceipt:       &draftLifecycleReceipt{Mailbox: "Pending\x1b[31m\r\nmail", UIDValidity: 13, UID: 14},
				ManualReconciliation: true,
			},
			want: []string{
				"draft draft-ID  spoofed revision 7 lifecycle",
				"status: status  injected",
				"receipt (revision 7): Mailbox  receipt uidvalidity=11 uid=12",
				"content:\nline one\nline red\nlinethree\n",
				"pending operation: edit  operation",
				"candidate content:\ncandidate one\ncandidate two\nend\n",
				"pending receipt (revision 7): Pending  mail uidvalidity=13 uid=14",
				"provider outcome: pending  code",
				"old draft ID remains blocked at revision 7",
				"manual action: reconcile the provider receipt and local state before retrying",
			},
		},
		{
			name: "sanitizes pending old provider receipt",
			output: draftLifecycleOutput{
				Status:           "pending",
				DraftID:          "pending-id",
				Revision:         8,
				Lifecycle:        "active",
				Receipt:          draftLifecycleReceipt{Mailbox: "Drafts", UIDValidity: 21, UID: 22},
				Content:          "stored\nbody",
				CandidateContent: "candidate\nbody",
				PendingOperation: "edit",
				PendingReceipt:   &draftLifecycleReceipt{Mailbox: "replacement", UIDValidity: 21, UID: 23},
				Observation: &draftLifecycleObservation{
					State: "present", Code: "old\x1b[31mcode", Mailbox: "old\x1b]0;evil\x07mail",
					UIDValidity: 21, UID: 22, Present: true,
				},
			},
			want: []string{
				"old provider receipt: oldmail uidvalidity=21 uid=22 (oldcode)",
				"provider outcome: oldcode",
			},
		},
		{
			name: "sanitizes accepted replacement receipt",
			output: draftLifecycleOutput{
				Status:           "accepted_local_failed",
				DraftID:          "accepted-id",
				Revision:         9,
				Lifecycle:        "active",
				Receipt:          draftLifecycleReceipt{Mailbox: "Drafts", UIDValidity: 31, UID: 32},
				Content:          "stored",
				PendingOperation: "edit",
				PendingReceipt:   &draftLifecycleReceipt{Mailbox: "replacement", UIDValidity: 31, UID: 33},
				ProviderObservation: &draftLifecycleObservation{
					State: "present", Code: "replacement\x1b[31mcode", Mailbox: "new\x1b]0;evil\x07mail",
					UIDValidity: 31, UID: 33, Present: true,
				},
				Observation: &draftLifecycleObservation{
					State: "missing", Code: "old\x1b[31mcode", Mailbox: "old\x1b]0;evil\x07mail",
				},
			},
			want: []string{
				"acknowledged replacement receipt: newmail uidvalidity=31 uid=33 (replacementcode)",
				"provider outcome: replacementcode",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			var events []api.CLIRunEvent
			err := emitDraftLifecycleOutput(func(event api.CLIRunEvent) error {
				events = append(events, event)
				return nil
			}, cliStreamStderr, false, tt.output)
			requirements.NoError(err)
			requirements.Len(events, 1)
			for _, want := range tt.want {
				assertions.Contains(events[0].Data, want)
			}
			if tt.output.RawMIME != "" {
				assertions.NotContains(events[0].Data, tt.output.RawMIME)
			}
			for _, control := range []string{"\x00", "\x07", "\x1b", "\r", "\u009b"} {
				assertions.NotContains(events[0].Data, control)
			}
		})
	}
}

func TestEmitDraftLifecycleOutputJSONRoundTrip(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	output := draftLifecycleOutput{
		Status:           "accepted_local_failed\x1b[31m\r\nstatus",
		DraftID:          "draft\x1b]0;id\x07value",
		Revision:         42,
		Lifecycle:        "active\u009bstate",
		MessageID:        43,
		SourceID:         44,
		Receipt:          draftLifecycleReceipt{Mailbox: "Drafts\r\nmailbox", UIDValidity: 45, UID: 46},
		Content:          "stored\r\n\x1b[31mcontent\x1b[0m\u009b",
		RawMIME:          "From: sender@example.com\r\n\r\n\x1b[31mraw\x1b[0m\u009b",
		CandidateContent: "candidate\r\n\x1b]0;candidate\x07content",
		PendingOperation: "edit\r\noperation",
		PendingCode:      "pending\u009bcode",
		PendingReceipt:   &draftLifecycleReceipt{Mailbox: "Replacement\x1b[31m\r\nmailbox", UIDValidity: 47, UID: 48},
		ProviderObservation: &draftLifecycleObservation{
			State: "present\x1b[31m", Code: "replacement\r\ncode", Mailbox: "Replacement\x1b]0;mail\x07box",
			UIDValidity: 49, UID: 50, Flags: []string{"\\Seen", "flag\u009bvalue"}, Present: true,
			Draft: true, Deleted: false, Complete: true, UIDPlus: true,
		},
		Observation: &draftLifecycleObservation{
			State: "missing\x1b[31m", Code: "old\r\ncode", Mailbox: "Old\x1b]0;mail\x07box",
			UIDValidity: 51, UID: 52, Flags: []string{"\\Draft", "old\u009bflag"}, Present: false,
			Draft: false, Deleted: true, Complete: false, UIDPlus: false,
		},
		ManualReconciliation: true,
	}

	var events []api.CLIRunEvent
	err := emitDraftLifecycleOutput(func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	}, cliStreamStdout, true, output)
	requirements.NoError(err)
	requirements.Len(events, 1)
	var decoded draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &decoded))
	assertions.Equal(output, decoded)
}

func TestDraftLifecycleAcceptedReadFailureEvidence(t *testing.T) {
	testutil.SkipIfPostgres(t, "local read fault injection alters SQLite tables")
	for _, fault := range []string{"message", "reply_link"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", fault, asJSON), func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
				addr, user := startReviewCmdStoreBarrierServer(t, barrier)
				fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
					testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
				})
				barrier.appendCalls.Store(0)
				barrier.storeCalls.Store(0)
				barrier.expungeCalls.Store(0)
				var faultErr error
				barrier.onAppend = func() {
					if fault == "message" {
						_, faultErr = fixture.store.DB().Exec("ALTER TABLE message_bodies RENAME TO fault_message_bodies")
					} else {
						_, faultErr = fixture.store.DB().Exec("ALTER TABLE messages RENAME COLUMN reply_to_message_id TO fault_reply_to_message_id")
					}
				}
				server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: fixture.adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
				t.Cleanup(server.Close)
				configureRemoteDaemonForTest(t, server.URL)
				root := &cobra.Command{Use: "msgvault"}
				root.AddCommand(newDraftEditCommand())
				silenceUsageInRunE(root)
				var stdout, stderr bytes.Buffer
				root.SetOut(&stdout)
				root.SetErr(&stderr)
				args := []string{"draft-edit", fixture.draft.DraftID, "--revision", "1", "--body", "saved candidate"}
				if asJSON {
					args = append(args, "--json")
				}
				root.SetArgs(args)
				err := root.ExecuteContext(t.Context())
				requirements.NoError(faultErr)
				requirements.ErrorContains(err, "accepted_local_failed")
				assertions.Empty(stdout.String())
				if fault == "message" {
					_, err = fixture.store.DB().Exec("ALTER TABLE fault_message_bodies RENAME TO message_bodies")
				} else {
					_, err = fixture.store.DB().Exec("ALTER TABLE messages RENAME COLUMN fault_reply_to_message_id TO reply_to_message_id")
				}
				requirements.NoError(err)
				if asJSON {
					var output draftLifecycleOutput
					requirements.NoError(json.Unmarshal(stderr.Bytes(), &output))
					assertions.Equal("accepted_local_failed", output.Status)
					assertions.Equal(fixture.draft.DraftID, output.DraftID)
					assertions.Contains(output.CandidateContent, "saved candidate")
					requirements.NotNil(output.PendingReceipt)
					assertions.Equal(uint32(2), output.PendingReceipt.UID)
					assertions.True(output.ManualReconciliation)
				} else {
					assertions.Contains(stderr.String(), "status: accepted_local_failed")
					assertions.Contains(stderr.String(), "saved candidate")
					assertions.Contains(stderr.String(), "pending receipt (revision 1): Drafts uidvalidity=1 uid=2")
					assertions.Contains(stderr.String(), "old draft ID remains blocked at revision 1")
				}
				pending, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
				requirements.NoError(err)
				requirements.NotNil(pending.Pending)
				requirements.NotNil(pending.Pending.ReplacementReceipt)
				assertions.Equal(uint32(2), pending.Pending.ReplacementReceipt.UID)
				assertions.Equal(int64(1), pending.Revision)
				assertions.Contains(string(pending.Pending.Raw), "saved candidate")
				assertions.Equal(int32(1), barrier.appendCalls.Load())
				assertions.Zero(barrier.storeCalls.Load())
				assertions.Zero(barrier.expungeCalls.Load())
				assertions.Equal(uint32(2), reviewDraftMailboxCount(t, addr))
			})
		}
	}
}

func TestDraftLifecyclePublicationFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "original")
	conversationID, err := fixture.store.EnsureConversation(fixture.source.ID, "publication-conflict", "Publication conflict")
	requirements.NoError(err)
	_, err = fixture.store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: fixture.source.ID, SourceMessageID: "Drafts|2", ConversationID: conversationID,
			MessageType: store.MessageTypeEmail,
		},
		BodyText: sql.NullString{String: "conflict", Valid: true},
		RawMIME:  []byte("From: conflict@example.com\r\n\r\nconflict\r\n"),
	})
	requirements.NoError(err)

	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newDraftEditCommand())
	silenceUsageInRunE(root)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "candidate", "--json"})
	err = root.ExecuteContext(t.Context())
	assertions.Empty(stdout.String())
	events := []api.CLIRunEvent{{Type: cliStreamStderr, Data: stderr.String()}}
	requirements.Error(err)
	assertions.Equal("accepted_local_failed", err.Error())
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	var output struct {
		Status           string `json:"status"`
		Revision         int64  `json:"revision"`
		CandidateContent string `json:"candidate_content"`
		PendingOperation string `json:"pending_operation"`
		PendingReceipt   struct {
			UID uint32 `json:"uid"`
		} `json:"pending_receipt"`
	}
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("accepted_local_failed", output.Status)
	assertions.Equal(int64(1), output.Revision)
	assertions.Contains(output.CandidateContent, "candidate")
	assertions.Equal("edit", output.PendingOperation)
	assertions.Equal(uint32(2), output.PendingReceipt.UID)
}

func TestDraftLifecycleHumanAcknowledgedReceiptWhenRecordFails(t *testing.T) {
	testutil.SkipIfPostgres(t, "receipt write fault injection uses a SQLite trigger")
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "original")
	_, err := fixture.store.DB().Exec(`
		CREATE TRIGGER fail_draft_replacement_receipt
		BEFORE UPDATE OF pending_replacement_uid ON imap_drafts
		WHEN NEW.pending_replacement_uid IS NOT NULL
		  AND OLD.pending_replacement_uid IS NULL
		BEGIN
			SELECT RAISE(FAIL, 'injected replacement receipt failure');
		END
	`)
	requirements.NoError(err)

	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{HomeDir: t.TempDir()}, Store: adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newDraftEditCommand())
	silenceUsageInRunE(root)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{api.CLIRunDraftEditCommand, created.DraftID,
		"--revision", "1", "--body", "candidate"})
	err = root.ExecuteContext(t.Context())
	requirements.Error(err)
	assertions.Equal("accepted_local_failed", err.Error())
	assertions.Empty(stdout.String())
	assertions.Contains(stderr.String(), "status: accepted_local_failed")
	assertions.Contains(stderr.String(), "pending operation: edit")
	assertions.Contains(stderr.String(), "provider outcome: append_uidplus")
	assertions.Contains(stderr.String(), "acknowledged replacement receipt: Drafts uidvalidity=1 uid=2")
	assertions.Contains(stderr.String(), "receipt (revision 1): Drafts uidvalidity=1 uid=1")
	assertions.Contains(stderr.String(), "candidate content:")
	assertions.Contains(stderr.String(), "candidate")
	assertions.Contains(stderr.String(), "old draft ID remains blocked at revision 1")
	assertions.Contains(stderr.String(), "manual action:")
	assertions.NotContains(stderr.String(), "pending receipt:")
	assertions.NotContains(stderr.String(), "Error: accepted_local_failed")

	pending, err := fixture.store.GetIMAPDraftContext(context.Background(), created.DraftID)
	requirements.NoError(err)
	requirements.Equal(int64(1), pending.Revision)
	requirements.NotNil(pending.Pending)
	requirements.Nil(pending.Pending.ReplacementReceipt)
	requirements.Equal(uint32(1), pending.Pending.OriginalReceipt.UID)
	requirements.Contains(string(pending.Pending.Raw), "candidate")
	provider := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
	oldObservation, err := provider.InspectDraft(t.Context(), imaplib.DraftReceipt{
		Mailbox: "Drafts", UIDValidity: 1, UID: 1,
	})
	requirements.NoError(err)
	newObservation, err := provider.InspectDraft(t.Context(), imaplib.DraftReceipt{
		Mailbox: "Drafts", UIDValidity: 1, UID: 2,
	})
	requirements.NoError(err)
	assertions.True(oldObservation.Present)
	assertions.True(newObservation.Present)
	requirements.NoError(provider.Close())
}

func TestDraftLifecycleUnknownAppend(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, user := startReviewDropAppendServer(t)
	dbPath := filepath.Join(t.TempDir(), "lifecycle.db")
	fixture := newReviewManagedLifecycleFixtureOnServerWithDBPath(t, addr, dbPath, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})
	var providerCalls atomic.Int32
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls.Add(1)
		return imaplib.NewClient(fixture.config, testutil.IMAPTestPassword), nil
	}

	events, err := runReviewLifecycle(t, fixture.adapter, api.CLIRunDraftEditCommand, fixture.draft.DraftID, "--revision", "1", "--body", "candidate", "--json")
	requirements.Error(err)
	assertions.Equal("remote_unknown", err.Error())
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	assertions.Contains(events[0].Data, `"candidate_content"`)
	assertions.Contains(events[0].Data, "candidate")
	assertions.Equal(uint32(2), reviewDraftMailboxCount(t, fixture.config.Addr()))
	pending, err := fixture.store.GetIMAPDraftContext(context.Background(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(pending.Pending)
	assertions.Equal("remote_unknown", pending.Pending.Code)
	assertions.Equal(int32(1), providerCalls.Load())

	requirements.NoError(fixture.store.Close())
	reopened, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	reopenedDraft, err := reopened.GetIMAPDraftContext(context.Background(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(reopenedDraft.Pending)
	assertions.Equal("remote_unknown", reopenedDraft.Pending.Code)
	assertions.Contains(string(reopenedDraft.Pending.Raw), "candidate")
	freshAdapter := &storeAPIAdapter{
		store:       reopened,
		draftPolicy: []config.IMAPDraftSource{{SourceID: fixture.source.ID, Enabled: true, Mailbox: "Drafts"}},
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			providerCalls.Add(1)
			return imaplib.NewClient(fixture.config, testutil.IMAPTestPassword), nil
		},
	}
	events, err = runReviewLifecycle(t, freshAdapter, api.CLIRunDraftEditCommand, fixture.draft.DraftID, "--revision", "1", "--body", "retry", "--json")
	requirements.Error(err)
	assertions.Equal("pending_operation", err.Error())
	assertions.Empty(events)
	assertions.Equal(int32(1), providerCalls.Load())
}

func TestDraftLifecycleHumanUnknownAppendThroughHTTP(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, user := startReviewDropAppendServer(t)
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})

	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{HomeDir: t.TempDir()}, Store: fixture.adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newDraftEditCommand())
	silenceUsageInRunE(root)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{api.CLIRunDraftEditCommand, fixture.draft.DraftID,
		"--revision", "1", "--body", "candidate"})
	err := root.ExecuteContext(t.Context())
	requirements.Error(err)
	assertions.Equal("remote_unknown", err.Error())
	assertions.Empty(stdout.String())
	assertions.Contains(stderr.String(), "status: pending")
	assertions.Contains(stderr.String(), "pending operation: edit")
	assertions.Contains(stderr.String(), "provider outcome: remote_unknown")
	assertions.Contains(stderr.String(), "candidate content:")
	assertions.Contains(stderr.String(), "candidate")
	assertions.Contains(stderr.String(), "receipt (revision 1): Drafts")
	assertions.Contains(stderr.String(), "old draft ID remains blocked at revision 1")
	assertions.Contains(stderr.String(), "manual action:")
	assertions.NotContains(stderr.String(), "Error: remote_unknown")
	assertions.Equal(1, strings.Count(stderr.String(), "provider outcome:"))
}

func TestDraftLifecycleRetainedGet(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "retained")
	var providerCalls atomic.Int32
	adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls.Add(1)
		return nil, errors.New("draft-get must not connect to IMAP")
	}
	_, err := fixture.store.DB().Exec(fixture.store.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?
	`), created.MessageID)
	requirements.NoError(err)
	events, err := runReviewLifecycle(t, adapter, api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, "retained")
	assertions.Contains(events[0].Data, `"provider_observation":{"state":"not_checked","code":"not_checked"`)
	assertions.Zero(providerCalls.Load())

	_, err = fixture.store.ClaimIMAPDraftContext(context.Background(), created.DraftID, 1, store.IMAPDraftOperationEdit, []byte("candidate raw"))
	requirements.NoError(err)
	events, err = runReviewLifecycle(t, adapter, api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	assertions.Contains(events[0].Data, `"candidate_content":"candidate raw"`)
}

func TestDraftLifecycleRepeatedDelete(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "discard me")
	events, err := runReviewLifecycle(t, adapter, api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var deleted struct {
		Revision int64 `json:"revision"`
	}
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &deleted))
	assertions.Equal(int64(2), deleted.Revision)
	var providerCalls atomic.Int32
	adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls.Add(1)
		return nil, errors.New("discarded retry must be offline")
	}
	events, err = runReviewLifecycle(t, adapter, api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"already_discarded"`)
	assertions.Zero(providerCalls.Load())
	_, err = runReviewLifecycle(t, adapter, api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("revision_mismatch", err.Error())
	_, err = runReviewLifecycle(t, adapter, api.CLIRunDraftEditCommand, created.DraftID, "--revision", "2", "--body", "new", "--json")
	requirements.Error(err)
	assertions.Equal("draft_discarded", err.Error())
}

func TestDraftLifecycleExactCopy(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "copy")
	copyClient := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
	copyReceipt, err := copyClient.AppendDraft(t.Context(), "Drafts", []byte("From: alice@example.com\r\nTo: "+testutil.IMAPTestUsername+"\r\nSubject: Question\r\n\r\ncopy\r\n"))
	requirements.NoError(err)
	requirements.NoError(copyClient.Close())
	requirements.NotEqual(uint32(0), copyReceipt.UID)
	testutil.ExpungeIMAPMessage(t, fixture.config.Addr(), "Drafts", emersionimap.UID(1))
	events, err := runReviewLifecycle(t, adapter, api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "must refuse", "--json")
	requirements.Error(err)
	assertions.Equal("absent", err.Error())
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"code":"absent"`)
	draft, err := fixture.store.GetIMAPDraftContext(context.Background(), created.DraftID)
	requirements.NoError(err)
	assertions.Nil(draft.Pending)

	second := createReviewDraft(t, fixture, adapter, "valid second draft")
	events, err = runReviewLifecycle(t, adapter, api.CLIRunDraftEditCommand, second.DraftID, "--revision", "1", "--body", "works", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"revision":2`)
	inspection := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
	observation, err := inspection.InspectDraft(t.Context(), imaplib.DraftReceipt{Mailbox: "Drafts", UIDValidity: copyReceipt.UIDValidity, UID: copyReceipt.UID})
	requirements.NoError(err)
	assertions.True(observation.Present)
}

func TestDraftLifecyclePolicy(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "policy")
	var providerCalls atomic.Int32
	adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls.Add(1)
		return nil, errors.New("policy refusal must precede provider")
	}
	adapter.draftPolicy[0].Mailbox = "Other"
	events, err := runReviewLifecycle(t, adapter, api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "blocked", "--json")
	requirements.Error(err)
	assertions.Equal("invalid_mailbox", err.Error())
	assertions.Empty(events)
	assertions.Zero(providerCalls.Load())
	adapter.draftPolicy = nil
	events, err = runReviewLifecycle(t, adapter, api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, "policy")
	assertions.Zero(providerCalls.Load())

	adapter.draftPolicy = []config.IMAPDraftSource{{SourceID: fixture.source.ID, Enabled: true, Mailbox: "Drafts"}}
	badConfig, err := (&imaplib.Config{Host: "127.0.0.1", Port: 1, Username: testutil.IMAPTestUsername}).ToJSON()
	requirements.NoError(err)
	requirements.NoError(fixture.store.UpdateSourceSyncConfig(fixture.source.ID, badConfig))
	_, err = runReviewLifecycle(t, adapter, api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("invalid_source", err.Error())
	assertions.Zero(providerCalls.Load())
}

func TestDraftLifecycleBoundaries(t *testing.T) {
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "boundaries")
	var providerCalls atomic.Int32
	adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls.Add(1)
		return nil, errors.New("local draft-get must not open provider")
	}
	for _, state := range []string{"active", "pending", "discarded"} {
		t.Run(state, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			switch state {
			case "pending":
				_, err := fixture.store.ClaimIMAPDraftContext(context.Background(), created.DraftID, 1, store.IMAPDraftOperationEdit, []byte("pending"))
				requirements.NoError(err)
			case "discarded":
				_, err := fixture.store.ClaimIMAPDraftContext(context.Background(), created.DraftID, 1, store.IMAPDraftOperationDelete, nil)
				requirements.NoError(err)
				requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(context.Background(), created.DraftID, 1, store.IMAPDraftCodeRemoved, nil))
				_, err = fixture.store.FinishIMAPDraftRemovalContext(context.Background(), created.DraftID, 1)
				requirements.NoError(err)
			}
			events, err := runReviewLifecycle(t, adapter, api.CLIRunDraftGetCommand, created.DraftID, "--json")
			requirements.NoError(err)
			requirements.Len(events, 1)
			assertions.Contains(events[0].Data, "boundaries")
			assertions.Zero(providerCalls.Load())
		})
		if state == "pending" {
			_, err := fixture.store.DB().Exec(fixture.store.Rebind(`UPDATE imap_drafts SET pending_operation = NULL, pending_original_message_id = NULL, pending_original_mailbox = NULL, pending_original_uidvalidity = NULL, pending_original_uid = NULL, pending_raw = NULL, pending_code = NULL WHERE draft_id = ?`), created.DraftID)
			requirements.NoError(err)
		}
	}
}

func TestDraftLifecycleCleanupOutcomeEvidence(t *testing.T) {
	for _, operation := range []string{api.CLIRunDraftEditCommand, api.CLIRunDraftDeleteCommand} {
		for _, fault := range []string{"none", "record", "read"} {
			t.Run(fmt.Sprintf("%s/fault=%s", operation, fault), func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
				addr, user := startReviewCmdStoreBarrierServer(t, barrier)
				fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
					testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
				})
				barrier.armed.Store(true)
				args := []string{operation, fixture.draft.DraftID, "--revision", "1", "--json"}
				if operation == api.CLIRunDraftEditCommand {
					args = append(args, "--body", "candidate")
				}
				eventsCh := make(chan []api.CLIRunEvent, 1)
				errCh := make(chan error, 1)
				go func() {
					events, err := runReviewLifecycle(t, fixture.adapter, args...)
					eventsCh <- events
					errCh <- err
				}()
				<-barrier.stored
				reviewStoreFlagsForLifecycle(t, addr, 1, emersionimap.StoreFlagsDel, emersionimap.FlagDeleted)
				if fault == "record" {
					_, err := fixture.store.DB().Exec(fixture.store.Rebind("UPDATE imap_drafts SET revision = revision + 1 WHERE draft_id = ?"), fixture.draft.DraftID)
					requirements.NoError(err)
				}
				if fault == "read" {
					_, err := fixture.store.DB().Exec("ALTER TABLE imap_drafts RENAME TO fault_imap_drafts")
					requirements.NoError(err)
				}
				close(barrier.release)
				events, err := <-eventsCh, <-errCh
				requirements.Error(err)
				requirements.Len(events, 1)
				var output draftLifecycleOutput
				requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
				assertions.Equal("pending", output.Status)
				assertions.True(output.ManualReconciliation)
				assertions.Equal("store_conflict", output.PendingCode)
				requirements.NotNil(output.Observation)
				assertions.Equal("store_conflict", output.Observation.Code)
				assertions.Equal(uint32(1), output.Observation.UID)
				assertions.True(output.Observation.Present)
				if fault == "read" {
					_, err := fixture.store.DB().Exec("ALTER TABLE fault_imap_drafts RENAME TO imap_drafts")
					requirements.NoError(err)
				}
				latest, loadErr := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
				requirements.NoError(loadErr)
				requirements.NotNil(latest.Pending)
				assertions.Equal(latest.Revision, output.Revision)
				if fault != "none" {
					assertions.Equal("local_persistence_failed", err.Error())
					coded, ok := errors.AsType[*api.CLIRunCodedError](err)
					requirements.True(ok)
					if fault == "record" {
						requirements.ErrorIs(coded.Err, store.ErrIMAPDraftRevision)
					} else {
						requirements.ErrorContains(coded.Err, "imap_drafts")
					}
					assertions.NotEqual("store_conflict", latest.Pending.Code)
				} else {
					assertions.Equal("store_conflict", err.Error())
					assertions.Equal("store_conflict", latest.Pending.Code)
				}
				if operation == api.CLIRunDraftEditCommand {
					assertions.Contains(output.CandidateContent, "candidate")
					assertions.Equal(uint32(2), output.Receipt.UID)
					requirements.NotNil(output.PendingReceipt)
					assertions.Equal(uint32(2), output.PendingReceipt.UID)
					assertions.Equal(uint32(2), latest.CurrentReceipt.UID)
					assertions.Equal(uint32(1), latest.Pending.OriginalReceipt.UID)
				} else {
					assertions.Nil(latest.DiscardedAt)
				}
			})
		}
	}
}

func TestDraftLifecycleCleanup(t *testing.T) {
	testutil.SkipIfPostgres(t, "finish failure injection uses a SQLite trigger")
	for _, operation := range []string{api.CLIRunDraftEditCommand, api.CLIRunDraftDeleteCommand} {
		t.Run(operation, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
			addr, user := startReviewCmdStoreBarrierServer(t, barrier)
			fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
				testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
			})
			_, err := fixture.store.DB().Exec(`
				CREATE TRIGGER fail_draft_finish
				AFTER UPDATE OF pending_code ON imap_drafts
				WHEN NEW.pending_code = 'removed'
				BEGIN
					UPDATE imap_drafts SET revision = NEW.revision + 1 WHERE draft_id = NEW.draft_id;
				END
			`)
			requirements.NoError(err)
			barrier.armed.Store(true)
			eventsCh := make(chan []api.CLIRunEvent, 1)
			errCh := make(chan error, 1)
			args := []string{operation, fixture.draft.DraftID, "--revision", "1", "--json"}
			if operation == api.CLIRunDraftEditCommand {
				args = append(args, "--body", "candidate\nline two")
			}
			go func() {
				events, err := runReviewLifecycle(t, fixture.adapter, args...)
				eventsCh <- events
				errCh <- err
			}()
			<-barrier.stored
			close(barrier.release)
			events, err := <-eventsCh, <-errCh
			requirements.Error(err)
			assertions.Equal("cleanup_local_failed", err.Error())
			requirements.Len(events, 1)
			assertions.Equal(cliStreamStderr, events[0].Type)
			assertions.Contains(events[0].Data, `"status":"pending"`)
			assertions.Contains(events[0].Data, `"manual_reconciliation":true`)
			assertions.Contains(events[0].Data, `"pending_operation":"`+strings.TrimPrefix(operation, "draft-")+`"`)
			assertions.Contains(events[0].Data, `"pending_code":"removed"`)
			assertions.Contains(events[0].Data, `"code":"removed"`)
			latest, loadErr := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
			requirements.NoError(loadErr)
			requirements.NotNil(latest.Pending)
			assertions.Equal(store.IMAPDraftCodeRemoved, latest.Pending.Code)
			assertions.Equal(operation, "draft-"+latest.Pending.Operation)

			_, err = fixture.store.DB().Exec("DROP TRIGGER fail_draft_finish")
			requirements.NoError(err)
			fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
				return nil, errors.New("local completion must not connect to IMAP")
			}
			retryArgs := []string{operation, latest.DraftID, "--revision", strconv.FormatInt(latest.Revision, 10), "--json"}
			if operation == api.CLIRunDraftEditCommand {
				_, err = runReviewLifecycle(t, fixture.adapter, append(retryArgs, "--body", "different")...)
				requirements.ErrorContains(err, "pending_operation")
				retryArgs = append(retryArgs, "--body", "candidate\r\nline two")
			}
			staleArgs := append([]string(nil), retryArgs...)
			staleArgs[3] = "1"
			_, err = runReviewLifecycle(t, fixture.adapter, staleArgs...)
			requirements.ErrorContains(err, "revision_mismatch")
			if operation == api.CLIRunDraftDeleteCommand {
				_, err = runReviewLifecycle(t, fixture.adapter, api.CLIRunDraftEditCommand, latest.DraftID, "--revision", strconv.FormatInt(latest.Revision, 10), "--body", "candidate")
			} else {
				_, err = runReviewLifecycle(t, fixture.adapter, api.CLIRunDraftDeleteCommand, latest.DraftID, "--revision", strconv.FormatInt(latest.Revision, 10))
			}
			requirements.ErrorContains(err, "pending_operation")
			events, err = runReviewLifecycle(t, fixture.adapter, retryArgs...)
			requirements.NoError(err)
			requirements.Len(events, 1)
			finished, err := fixture.store.GetIMAPDraftContext(t.Context(), latest.DraftID)
			requirements.NoError(err)
			assertions.Nil(finished.Pending)
			if operation == api.CLIRunDraftDeleteCommand {
				assertions.NotNil(finished.DiscardedAt)
				assertions.Equal(latest.Revision+1, finished.Revision)
			} else {
				assertions.Equal(latest.Revision, finished.Revision)
				assertions.Contains(events[0].Data, `"status":"edited"`)
			}
		})
	}
}

func TestDraftLifecycleRemovedOutcomePersistenceFailure(t *testing.T) {
	testutil.SkipIfPostgres(t, "removed outcome failure injection uses a SQLite trigger")
	for _, operation := range []string{api.CLIRunDraftEditCommand, api.CLIRunDraftDeleteCommand} {
		t.Run(operation, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			addr, user := startReviewCmdStoreBarrierServer(t, &reviewCmdStoreBarrier{
				stored: make(chan struct{}), release: make(chan struct{}),
			})
			fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
				testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
			})
			_, err := fixture.store.DB().Exec(`
				CREATE TRIGGER fail_removed_outcome
				BEFORE UPDATE OF pending_code ON imap_drafts
				WHEN NEW.pending_code = 'removed'
				BEGIN
					SELECT RAISE(FAIL, 'injected removed outcome persistence failure');
				END
			`)
			requirements.NoError(err)
			args := []string{operation, fixture.draft.DraftID, "--revision", "1", "--json"}
			if operation == api.CLIRunDraftEditCommand {
				args = append(args, "--body", "candidate")
			}
			events, err := runReviewLifecycle(t, fixture.adapter, args...)
			requirements.Error(err)
			assertions.Equal("local_persistence_failed", err.Error())
			requirements.Len(events, 1)
			var output draftLifecycleOutput
			requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
			assertions.Equal("pending", output.Status)
			assertions.True(output.ManualReconciliation)
			requirements.NotNil(output.Observation)
			assertions.Equal(store.IMAPDraftCodeRemoved, output.Observation.Code)
			latest, loadErr := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
			requirements.NoError(loadErr)
			requirements.NotNil(latest.Pending)
			assertions.Equal(operation, "draft-"+latest.Pending.Operation)
			assertions.NotEqual(store.IMAPDraftCodeRemoved, latest.Pending.Code)
			assertions.Nil(latest.DiscardedAt)
		})
	}
}

func TestDraftLifecycleHumanCleanupPartialThroughHTTP(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	addr, user := startReviewCmdStoreBarrierServer(t, barrier)
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})
	barrier.armed.Store(true)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{HomeDir: t.TempDir()}, Store: fixture.adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	configureRemoteDaemonForTest(t, server.URL)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newDraftDeleteCommand())
	silenceUsageInRunE(root)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{api.CLIRunDraftDeleteCommand, fixture.draft.DraftID,
		"--revision", "1"})

	errCh := make(chan error, 1)
	go func() { errCh <- root.ExecuteContext(t.Context()) }()
	<-barrier.stored
	_, err := fixture.store.DB().Exec(fixture.store.Rebind(`
		UPDATE imap_drafts SET revision = 2 WHERE draft_id = ?
	`), fixture.draft.DraftID)
	requirements.NoError(err)
	close(barrier.release)
	err = <-errCh
	requirements.Error(err)
	assertions.Equal("local_persistence_failed", err.Error())
	assertions.Empty(stdout.String())
	assertions.Contains(stderr.String(), "status: pending")
	assertions.Contains(stderr.String(), "pending operation: delete")
	assertions.Contains(stderr.String(), "provider outcome: removed")
	assertions.Contains(stderr.String(), "old provider receipt: Drafts")
	assertions.Contains(stderr.String(), "receipt (revision 2): Drafts")
	assertions.Contains(stderr.String(), "old draft ID remains blocked at revision 2")
	assertions.Contains(stderr.String(), "manual action:")
	assertions.NotContains(stderr.String(), "Error: cleanup_local_failed")
}

func TestDraftLifecycleSyncProjection(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "sync projection")
	var analyticsDir string
	if !fixture.store.IsPostgreSQL() {
		cacheRoot := t.TempDir()
		cacheDB := filepath.Join(cacheRoot, "cache.db")
		analyticsDir = filepath.Join(cacheRoot, "analytics")
		refreshCache := func() error {
			if err := os.Remove(cacheDB); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := fixture.store.BackupDatabase(cacheDB); err != nil {
				return err
			}
			_, err := buildCache(cacheDB, analyticsDir, true)
			return err
		}
		requirements.NoError(refreshCache())
		adapter.draftCacheRefresh = func(context.Context, string) error { return refreshCache() }
	}
	events, err := runReviewLifecycle(t, adapter, api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "projected", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var output struct {
		MessageID int64 `json:"message_id"`
		Revision  int64 `json:"revision"`
		Receipt   struct {
			UIDValidity uint32 `json:"uidvalidity"`
			UID         uint32 `json:"uid"`
		} `json:"receipt"`
	}
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal(int64(2), output.Revision)
	raw, err := fixture.store.GetMessageRawContext(context.Background(), output.MessageID)
	requirements.NoError(err)
	assertions.Contains(string(raw), "projected")
	draft, err := fixture.store.GetIMAPDraftContext(context.Background(), created.DraftID)
	requirements.NoError(err)
	assertions.Equal(output.MessageID, draft.CurrentMessageID)
	assertions.Equal(output.Receipt.UID, draft.CurrentReceipt.UID)
	assertions.Nil(draft.Pending)
	if !fixture.store.IsPostgreSQL() {
		engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
		requirements.NoError(err)
		t.Cleanup(func() { _ = engine.Close() })
		results, err := engine.SearchFast(t.Context(), search.Parse("projected"), query.MessageFilter{}, 100, 0)
		requirements.NoError(err)
		requirements.Len(results, 1)
		assertions.Equal(output.MessageID, results[0].ID)
		assertions.Contains(results[0].Snippet, "projected")
		rows, err := engine.Aggregate(t.Context(), query.ViewLabels, query.DefaultAggregateOptions())
		requirements.NoError(err)
		var draftLabel query.AggregateRow
		for _, row := range rows {
			if row.Key == "Drafts" {
				draftLabel = row
				break
			}
		}
		assertions.Equal(int64(1), draftLabel.Count)
	}
	adminClient, err := imapclient.DialInsecure(fixture.config.Addr(), nil)
	requirements.NoError(err)
	requirements.NoError(adminClient.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	requirements.NoError(adminClient.Create("INBOX", nil).Wait())
	requirements.NoError(adminClient.Close())
	syncClient := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
	syncOptions := msgsync.DefaultOptions()
	syncOptions.SourceType = "imap"
	syncOptions.NoResume = true
	summary, err := newMessageSyncer(syncClient, fixture.store, syncOptions).
		WithLogger(slog.New(slog.DiscardHandler)).
		FullWithFinalizer(t.Context(), fixture.source, func(summary *gmail.SyncSummary) error {
			return saveIMAPFolderStates(t.Context(), fixture.store, fixture.source, syncClient, summary, syncOptions.Limit)
		})
	requirements.NoError(err)
	requirements.Zero(summary.Errors)
	requirements.NoError(syncClient.Close())
	draft, err = fixture.store.GetIMAPDraftContext(context.Background(), created.DraftID)
	requirements.NoError(err)
	assertions.Equal(output.MessageID, draft.CurrentMessageID)
	assertions.Equal(output.Receipt.UIDValidity, draft.CurrentReceipt.UIDValidity)
	assertions.Equal(output.Receipt.UID, draft.CurrentReceipt.UID)
	assertions.Nil(draft.Pending)
	states, err := fixture.store.GetIMAPFolderStates(fixture.source.ID)
	requirements.NoError(err)
	var draftsState store.IMAPFolderState
	for _, state := range states {
		if state.Mailbox == "Drafts" {
			draftsState = state
			break
		}
	}
	assertions.Equal("Drafts", draftsState.Mailbox)
	assertions.Equal(output.Receipt.UIDValidity, draftsState.UIDValidity)
	assertions.Equal(output.Receipt.UID+1, draftsState.UIDNext)
	provider := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
	observation, err := provider.InspectDraft(t.Context(), imaplib.DraftReceipt{Mailbox: "Drafts", UIDValidity: output.Receipt.UIDValidity, UID: output.Receipt.UID})
	requirements.NoError(err)
	assertions.True(observation.Present)
}

func TestDraftLifecycleCancellation(t *testing.T) {
	requirements := require.New(t)
	barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	baseCtx, cancel := context.WithCancel(t.Context())
	ctx := &reviewLateCancelContext{Context: baseCtx, cancel: cancel}
	barrier.onAppend = ctx.arm
	addr, _ := startReviewCmdStoreBarrierServer(t, barrier)
	fixture := newReviewDraftReplyFixtureAtAddr(t, addr)
	adapter := fixture.grantedAdapter()
	adapter.draftCacheRefresh = func(context.Context, string) error { return nil }
	args := []string{"draft-reply", strconv.FormatInt(fixture.parentID, 10),
		"--from", testutil.IMAPTestUsername, "--body", "known cancellation", "--json"}
	var events []api.CLIRunEvent
	err := adapter.runCLIReplyDraft(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.True(ctx.cancelled.Load())
	requirements.Len(events, 1)
	var created draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &created))
	requirements.Equal(draftReplyStatusCreated, created.Status)
	requirements.Equal(int64(1), created.Revision)
	draft, err := fixture.store.GetIMAPDraftContext(context.Background(), created.DraftID)
	requirements.NoError(err)
	requirements.Equal(created.MessageID, draft.CurrentMessageID)
	requirements.Equal(created.UID, draft.CurrentReceipt.UID)
	requirements.Nil(draft.Pending)
	requirements.Equal(int32(1), barrier.appendCalls.Load())
	requirements.Zero(barrier.storeCalls.Load())
}

func TestDraftLifecycleCleanupCompletesAfterLateCancellation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	barrier := &reviewCmdStoreBarrier{stored: make(chan struct{}), release: make(chan struct{})}
	baseCtx, cancel := context.WithCancel(t.Context())
	ctx := &reviewLateCancelContext{Context: baseCtx, cancel: cancel}
	barrier.onFetch = func(n int32) {
		if n == 4 {
			ctx.arm()
		}
	}
	addr, user := startReviewCmdStoreBarrierServer(t, barrier)
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\n\r\noriginal\r\n"))
	})
	var events []api.CLIRunEvent
	err := fixture.adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{
		Args: []string{api.CLIRunDraftDeleteCommand, fixture.draft.DraftID, "--revision", "1", "--json"},
	}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.True(ctx.cancelled.Load())
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"deleted"`)
	draft, err := fixture.store.GetIMAPDraftContext(context.Background(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(draft.DiscardedAt)
	requirements.Nil(draft.Pending)
	assertions.Equal(int32(4), barrier.fetchCalls.Load())
	assertions.Equal(int32(2), barrier.storeCalls.Load())
	assertions.Equal(int32(1), barrier.expungeCalls.Load())
}

func TestDraftLifecycleRejectsEnvAndCwd(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter := &storeAPIAdapter{}
	for _, request := range []api.CLIRunRequest{
		{Args: []string{api.CLIRunDraftGetCommand, "draft-test"}, Env: map[string]string{"HOME": "blocked"}},
		{Args: []string{api.CLIRunDraftGetCommand, "draft-test"}, Cwd: "C:\\blocked"},
	} {
		err := adapter.runCLIDraftLifecycle(t.Context(), request, nil)
		requirements.Error(err)
		assertions.Equal("invalid_args", err.Error())
	}
}

type reviewCreatedDraft struct {
	DraftID   string `json:"draft_id"`
	Revision  int64  `json:"revision"`
	MessageID int64  `json:"message_id"`
}

type reviewLateCancelContext struct {
	context.Context

	cancel    context.CancelFunc
	armed     atomic.Bool
	cancelled atomic.Bool
}

func (c *reviewLateCancelContext) arm() {
	c.armed.Store(true)
}

func (c *reviewLateCancelContext) Err() error {
	if c.armed.Load() && c.cancelled.CompareAndSwap(false, true) {
		c.cancel()
	}
	return c.Context.Err()
}

func newReviewDraftReplyFixtureAtAddr(t *testing.T, addr string) draftReplyFixture {
	t.Helper()
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

func createReviewDraft(t *testing.T, fixture draftReplyFixture, adapter *storeAPIAdapter, body string) reviewCreatedDraft {
	t.Helper()
	events, err := fixture.run(t, adapter, "--body", body, "--json")
	require.NoError(t, err)
	require.Len(t, events, 1)
	var created reviewCreatedDraft
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &created))
	require.NotEmpty(t, created.DraftID)
	require.Equal(t, int64(1), created.Revision)
	return created
}

func runReviewLifecycle(t *testing.T, adapter *storeAPIAdapter, args ...string) ([]api.CLIRunEvent, error) {
	t.Helper()
	var events []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

type reviewManagedLifecycleFixture struct {
	store   *store.Store
	source  *store.Source
	config  *imaplib.Config
	draft   store.IMAPDraft
	adapter *storeAPIAdapter
}

func newReviewManagedLifecycleFixtureOnServer(t *testing.T, addr string, appendInitial func()) reviewManagedLifecycleFixture {
	t.Helper()
	imapConfig := reviewIMAPConfig(t, addr)
	appendInitial()
	reviewStoreFlagsForLifecycle(t, addr, 1, emersionimap.StoreFlagsAdd, emersionimap.FlagDraft)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", imapConfig.Identifier())
	require.NoError(t, err)
	configJSON, err := imapConfig.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, configJSON))
	conversationID, err := st.EnsureConversation(source.ID, "lifecycle-review", "Lifecycle review")
	require.NoError(t, err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\nContent-Type: text/plain\r\n\r\noriginal\r\n")
	draft, err := st.PersistIMAPDraftContext(context.Background(), receipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message:  &store.Message{SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt), MessageType: store.MessageTypeEmail, ConversationID: conversationID},
			BodyText: sql.NullString{String: "original", Valid: true}, RawMIME: raw,
		}
	})
	require.NoError(t, err)
	adapter := &storeAPIAdapter{
		store:       st,
		draftPolicy: []config.IMAPDraftSource{{SourceID: source.ID, Enabled: true, Mailbox: "Drafts"}},
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			return imaplib.NewClient(imapConfig, testutil.IMAPTestPassword), nil
		},
	}
	return reviewManagedLifecycleFixture{store: st, source: source, config: imapConfig, draft: draft, adapter: adapter}
}

func newReviewManagedLifecycleFixtureOnServerWithDBPath(
	t *testing.T, addr, dbPath string, appendInitial func(),
) reviewManagedLifecycleFixture {
	t.Helper()
	imapConfig := reviewIMAPConfig(t, addr)
	appendInitial()
	reviewStoreFlagsForLifecycle(t, addr, 1, emersionimap.StoreFlagsAdd, emersionimap.FlagDraft)
	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("imap", imapConfig.Identifier())
	require.NoError(t, err)
	configJSON, err := imapConfig.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, configJSON))
	conversationID, err := st.EnsureConversation(source.ID, "lifecycle-review", "Lifecycle review")
	require.NoError(t, err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\nContent-Type: text/plain\r\n\r\noriginal\r\n")
	draft, err := st.PersistIMAPDraftContext(context.Background(), receipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message:  &store.Message{SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt), MessageType: store.MessageTypeEmail, ConversationID: conversationID},
			BodyText: sql.NullString{String: "original", Valid: true}, RawMIME: raw,
		}
	})
	require.NoError(t, err)
	adapter := &storeAPIAdapter{
		store:       st,
		draftPolicy: []config.IMAPDraftSource{{SourceID: source.ID, Enabled: true, Mailbox: "Drafts"}},
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			return imaplib.NewClient(imapConfig, testutil.IMAPTestPassword), nil
		},
	}
	return reviewManagedLifecycleFixture{store: st, source: source, config: imapConfig, draft: draft, adapter: adapter}
}

type reviewDropAppendSession struct {
	imapserver.Session

	conn *imapserver.Conn
}

func (s *reviewDropAppendSession) Append(
	mailbox string,
	r emersionimap.LiteralReader,
	options *emersionimap.AppendOptions,
) (*emersionimap.AppendData, error) {
	data, err := s.Session.Append(mailbox, r, options)
	if err == nil {
		_ = s.conn.NetConn().Close()
	}
	if err != nil {
		return nil, fmt.Errorf("drop append session: %w", err)
	}
	return data, nil
}

func startReviewDropAppendServer(t *testing.T) (string, *imapmemserver.User) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("Drafts", nil))
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		Caps:         emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		InsecureAuth: true,
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &reviewDropAppendSession{Session: memServer.NewSession(), conn: conn}, nil, nil
		},
	})
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String(), user
}

type reviewCmdStoreBarrier struct {
	stored       chan struct{}
	release      chan struct{}
	armed        atomic.Bool
	blocked      atomic.Bool
	appendCalls  atomic.Int32
	fetchCalls   atomic.Int32
	storeCalls   atomic.Int32
	expungeCalls atomic.Int32
	appendErr    error
	onAppend     func()
	onFetch      func(int32)
}

type reviewCmdStoreBarrierSession struct {
	imapserver.Session

	barrier *reviewCmdStoreBarrier
}

func (s *reviewCmdStoreBarrierSession) Append(
	mailbox string,
	r emersionimap.LiteralReader,
	options *emersionimap.AppendOptions,
) (*emersionimap.AppendData, error) {
	if s.barrier.appendErr != nil {
		s.barrier.appendCalls.Add(1)
		return nil, s.barrier.appendErr
	}
	data, err := s.Session.Append(mailbox, r, options)
	s.barrier.appendCalls.Add(1)
	if s.barrier.onAppend != nil {
		s.barrier.onAppend()
	}
	if err != nil {
		return nil, fmt.Errorf("append barrier session: %w", err)
	}
	return data, nil
}

func (s *reviewCmdStoreBarrierSession) Fetch(
	w *imapserver.FetchWriter,
	numSet emersionimap.NumSet,
	options *emersionimap.FetchOptions,
) error {
	err := s.Session.Fetch(w, numSet, options)
	n := s.barrier.fetchCalls.Add(1)
	if s.barrier.onFetch != nil {
		s.barrier.onFetch(n)
	}
	if err != nil {
		return fmt.Errorf("fetch barrier session: %w", err)
	}
	return nil
}

func (s *reviewCmdStoreBarrierSession) Store(
	w *imapserver.FetchWriter,
	numSet emersionimap.NumSet,
	flags *emersionimap.StoreFlags,
	options *emersionimap.StoreOptions,
) error {
	err := s.Session.Store(w, numSet, flags, options)
	s.barrier.storeCalls.Add(1)
	if s.barrier.armed.Load() && s.barrier.blocked.CompareAndSwap(false, true) {
		close(s.barrier.stored)
		<-s.barrier.release
	}
	if err != nil {
		return fmt.Errorf("store barrier session: %w", err)
	}
	return nil
}

func (s *reviewCmdStoreBarrierSession) Expunge(
	w *imapserver.ExpungeWriter,
	uids *emersionimap.UIDSet,
) error {
	err := s.Session.Expunge(w, uids)
	s.barrier.expungeCalls.Add(1)
	if err != nil {
		return fmt.Errorf("expunge barrier session: %w", err)
	}
	return nil
}

func startReviewCmdStoreBarrierServer(t *testing.T, barrier *reviewCmdStoreBarrier) (string, *imapmemserver.User) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("Drafts", nil))
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		Caps:         emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
		InsecureAuth: true,
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &reviewCmdStoreBarrierSession{Session: memServer.NewSession(), barrier: barrier}, nil, nil
		},
	})
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	return ln.Addr().String(), user
}

func reviewIMAPConfig(t *testing.T, addr string) *imaplib.Config {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
}

func reviewStoreFlagsForLifecycle(t *testing.T, addr string, uid uint32, op emersionimap.StoreFlagsOp, flag emersionimap.Flag) {
	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = client.Select("Drafts", nil).Wait()
	require.NoError(t, err)
	var uids emersionimap.UIDSet
	uids.AddNum(emersionimap.UID(uid))
	require.NoError(t, client.Store(uids, &emersionimap.StoreFlags{Op: op, Flags: []emersionimap.Flag{flag}}, nil).Close())
}

func reviewDraftMailboxCount(t *testing.T, addr string) uint32 {
	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	data, err := client.Status("Drafts", &emersionimap.StatusOptions{NumMessages: true}).Wait()
	require.NoError(t, err)
	require.NotNil(t, data.NumMessages)
	return *data.NumMessages
}
